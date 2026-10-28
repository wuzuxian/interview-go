package voiceinterview

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// TtsClient DashScope qwen-tts-flash-realtime 实时语音合成 WebSocket 协议客户端
// （ADR 0003 手写；无 Go 官方 SDK，协议为"JSON 事件 + base64 音频帧"）。
//
// 契约来源：Java QwenTtsService（每次合成新建连接、commit 模式、收集
// response.audio.delta）与 DashScope 官方实时 TTS WebSocket 文档
// （help.aliyun.com/zh/model-studio/interactive-process-of-qwen-tts-realtime-synthesis）。
//
// 协议（全部为 JSON 文本帧；客户端每个事件携带随机 event_id 与 type）：
//
//	客户端 → 服务端：
//	  {"event_id":"…","type":"session.update","session":{…}}       配置音色/模式/音频格式等运行时参数
//	  {"event_id":"…","type":"input_text_buffer.append","text":"…"} 送入待合成文本
//	  {"event_id":"…","type":"input_text_buffer.commit"}            提交文本缓冲触发合成（commit 模式）
//	服务端 → 客户端：
//	  session.created / session.updated   会话建立、配置生效
//	  response.audio.delta                {"delta":"<base64 PCM 分片>"} 音频增量（24kHz/16bit/mono）
//	  response.done                       本轮合成结束（完成信号）
//	  error                               {"error":{"type","code","message"}} 协议错误
//
// 每次合成独立新建连接（Java synthesize 每次调用 new QwenTtsRealtime，互不影响）；
// 客户端不设接口接缝（spec 实现决策 19）：URL 等运行时参数来自 TtsSettings
// （语音配置源每次建连前读取即可热生效），测试以进程内假服务端打真协议代码。
type TtsClient struct {
	cfg    Config
	logger *slog.Logger
}

// NewTtsClient 创建 TTS 协议客户端；logger 为 nil 时用 slog.Default()。
func NewTtsClient(cfg Config, logger *slog.Logger) *TtsClient {
	if logger == nil {
		logger = slog.Default()
	}
	return &TtsClient{cfg: cfg, logger: logger}
}

// Synthesize 合成一句文本，返回完整 PCM（24kHz/16bit/mono）。
// 等待合成完成的超时取 Config.TTSTimeout（默认 8s，Java tts-timeout-seconds，
// 超时后由管道决定跳句）。
func (c *TtsClient) Synthesize(ctx context.Context, settings TtsSettings, text string) ([]byte, error) {
	return c.synthesize(ctx, settings, text, c.cfg.TTSTimeout)
}

// SynthesizeFull 合成整段文本（逐句全部失败后的兜底路径，管道票使用），
// 超时取 Config.TTSFullTimeout（默认 30s，Java QwenTtsService 的 30s latch 常量）。
func (c *TtsClient) SynthesizeFull(ctx context.Context, settings TtsSettings, text string) ([]byte, error) {
	return c.synthesize(ctx, settings, text, c.cfg.TTSFullTimeout)
}

// synthesize 单次合成完整流程：建连（握手超时 TTSConnectTimeout）→ session.update →
// append → commit → 按 deadline 收集增量 → response.done 收尾。
// wait 为 commit 之后的合成等待预算，对齐 Java 两段式超时：建连 5s（Handler
// tts-connect-timeout-seconds）与 latch 等待（分句 8s / 整段 30s）彼此独立。
func (c *TtsClient) synthesize(ctx context.Context, settings TtsSettings, text string, wait time.Duration) ([]byte, error) {
	// 对齐 Java QwenTtsService.synthesize L125-128：空文本直接返回空数组，不建连。
	if strings.TrimSpace(text) == "" {
		c.logger.Debug("TTS 收到空文本，直接返回空音频")
		return []byte{}, nil
	}

	// 建连：握手超时走 TTSConnectTimeout（Java connectWithTimeout L261-296 的
	// connectionFinishedLatch.await(connectTimeoutSeconds)）。
	header := make(http.Header)
	// APIKey 仅用于握手鉴权（DashScope 要求 Authorization: Bearer），不进日志与错误信息。
	header.Set("Authorization", "Bearer "+settings.APIKey)
	header.Set("user-agent", "interview-go voiceinterview tts/1.0")
	// 对齐 SDK DashScopeHeaders.buildWebSocketHeaders 恒发的数据 inspection 头。
	header.Set("X-DashScope-DataInspection", "enable")

	dialer := &websocket.Dialer{HandshakeTimeout: c.cfg.TTSConnectTimeout}
	dialURL := ttsDialURL(settings)
	c.logger.Debug("TTS WebSocket 开始建连", slog.String("url", dialURL), slog.String("model", settings.Model))
	conn, resp, err := dialer.DialContext(ctx, dialURL, header)
	if err != nil {
		// 对齐 Java createClient onFailure L248-258：握手失败附带 HTTP 状态
		// （"TTS WebSocket handshake failed: HTTP <code> <message>"），状态码无敏感信息。
		if resp != nil {
			return nil, fmt.Errorf("TTS WebSocket 建连失败: HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		}
		return nil, fmt.Errorf("TTS WebSocket 建连失败: %w", err)
	}
	defer conn.Close()
	c.logger.Debug("TTS WebSocket 连接已建立")

	// session.update：运行时参数字段名对齐 SDK QwenTtsRealtimeConfig.getConfig
	// （voice/mode/response_format/sample_rate/speech_rate/volume/language_type，
	// 未设置的字段省略，等价 gson 缺省 null 不序列化；Java updateSession L180-190）。
	if err := conn.WriteJSON(ttsEvent{
		EventID: newTtsEventID(),
		Type:    "session.update",
		Session: &ttsSessionConfig{
			Voice:          settings.Voice,
			Mode:           settings.Mode,
			ResponseFormat: settings.Format,
			SampleRate:     settings.SampleRate,
			SpeechRate:     settings.SpeechRate,
			Volume:         settings.Volume,
			LanguageType:   settings.LanguageType,
		},
	}); err != nil {
		return nil, fmt.Errorf("TTS 发送 session.update 失败: %w", err)
	}

	// commit 模式：append 送入文本后显式 commit 触发合成（Java appendText + commit，L196-197）。
	if err := conn.WriteJSON(ttsEvent{
		EventID: newTtsEventID(),
		Type:    "input_text_buffer.append",
		Text:    text,
	}); err != nil {
		return nil, fmt.Errorf("TTS 发送 input_text_buffer.append 失败: %w", err)
	}
	if err := conn.WriteJSON(ttsEvent{
		EventID: newTtsEventID(),
		Type:    "input_text_buffer.commit",
	}); err != nil {
		return nil, fmt.Errorf("TTS 发送 input_text_buffer.commit 失败: %w", err)
	}
	c.logger.Info("[TTS] 文本已提交，等待音频响应", slog.Int("textLength", len(text)), slog.String("voice", settings.Voice))

	// 合成等待预算自 commit 起算（Java：synthesisLatch.await(30s) / Handler future.get(8s)）；
	// 增量中途停滞或服务端不回 response.done 都会在此超时失败。
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		return nil, fmt.Errorf("TTS 设置读超时失败: %w", err)
	}

	var pcm chunks
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return c.readFailure(err, wait, pcm.bytes())
		}

		var event ttsServerEvent
		if err := json.Unmarshal(data, &event); err != nil {
			// 对齐 Java handleServerEvent L409-413：事件解析失败按错误收尾。
			return nil, fmt.Errorf("TTS 服务端事件解析失败: %w", err)
		}

		switch event.Type {
		case "response.audio.delta": // Java L361-371：delta 为 base64 音频分片
			if event.Delta != "" {
				chunk, err := base64.StdEncoding.DecodeString(event.Delta)
				if err != nil {
					return nil, fmt.Errorf("TTS 音频分片 base64 解码失败: %w", err)
				}
				pcm.append(chunk)
			}
		case "response.done": // Java L373-378：response.done 是完成信号，返回收集到的 PCM
			c.logger.Info("[TTS] 合成完成", slog.Int("pcmBytes", pcm.len()))
			return pcm.bytes(), nil
		case "error": // Java L380-403：按 "TTS Error [type/code]: message" 形状上报
			msg := formatTtsServerError(event.Error)
			c.logger.Error("[TTS] 服务端返回错误", slog.String("error", msg))
			return nil, fmt.Errorf("%s", msg)
		default:
			// session.created / session.updated / response.created 等事件不参与收尾（Java default 分支仅记日志）。
			c.logger.Debug("收到 TTS 事件", slog.String("type", event.Type))
		}
	}
}

// readFailure 归类读循环错误：读超时 → 合成超时；服务端主动关闭 → 对齐 Java
// onClose L164-167（关闭也解除等待，返回已收集的音频，可能为空）；其余 → 连接中断错误。
func (c *TtsClient) readFailure(err error, wait time.Duration, collected []byte) ([]byte, error) {
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		c.logger.Error("[TTS] 合成超时", slog.Duration("timeout", wait))
		return nil, fmt.Errorf("TTS 合成超时：等待音频完成超过 %s", wait)
	}
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
		c.logger.Warn("[TTS] 连接在 response.done 前被服务端关闭，返回已收集音频",
			slog.Int("pcmBytes", len(collected)))
		return collected, nil
	}
	return nil, fmt.Errorf("TTS WebSocket 连接中断: %w", err)
}

// dashScopeTTSRealtimeURL 生产默认端点（voice_config.tts_json 无 url 键——Java
// SDK QwenTtsRealtimeParam 内置端点同值；票 06 补齐 settings_provider.go 注明的
// 缺口，测试仍以 TtsSettings.URL 指向进程内假服务端）。
const dashScopeTTSRealtimeURL = "wss://dashscope.aliyuncs.com/api-ws/v1/realtime"

// ttsDialURL 组装建连地址：模型以查询参数携带（SDK QwenTtsRealtimeParam：
// baseWebsocketApiUrl + "?model=" + model；官方文档形如
// wss://dashscope.aliyuncs.com/api-ws/v1/realtime?model=qwen3-tts-flash-realtime）。
// 配置里的 URL 已带查询参数时用 & 续接。
func ttsDialURL(settings TtsSettings) string {
	base := settings.URL
	if base == "" {
		base = dashScopeTTSRealtimeURL
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "model=" + url.QueryEscape(settings.Model)
}

// newTtsEventID 生成协议要求的每事件唯一 event_id（官方文档：每个客户端事件
// 使用独立 UUID 作为 event_id；SDK 形如 "event_<uuid>"）。
func newTtsEventID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("event_%d", time.Now().UnixNano())
	}
	return "event_" + hex.EncodeToString(b[:])
}

// ttsEvent 客户端事件的统一信封：session.update 带 session，append 带 text，
// commit 两者皆无（omitempty 后即协议形状）。
type ttsEvent struct {
	EventID string            `json:"event_id"`
	Type    string            `json:"type"`
	Session *ttsSessionConfig `json:"session,omitempty"`
	Text    string            `json:"text,omitempty"`
}

// ttsSessionConfig session.update 的 session 对象。字段名为 DashScope 协议名
// （SDK QwenTtsRealtimeConfig.getConfig 逐字）；omitempty 对应 SDK 的"null 不下发"。
// 注意 Java 服务端硬编码 24kHz 枚举而忽略配置值；Go 按运行时配置下发
// （voice_config 存量默认即 pcm/24000，线上效果一致）。
type ttsSessionConfig struct {
	Voice          string  `json:"voice,omitempty"`
	Mode           string  `json:"mode,omitempty"` // commit：客户端显式提交触发合成
	ResponseFormat string  `json:"response_format,omitempty"`
	SampleRate     int     `json:"sample_rate,omitempty"`
	SpeechRate     float64 `json:"speech_rate,omitempty"`
	Volume         int     `json:"volume,omitempty"`
	LanguageType   string  `json:"language_type,omitempty"`
}

// ttsServerEvent 服务端事件的宽松解析：只取客户端关心的字段（Java handleServerEvent
// 同样只 switch type 并按需取 delta/error）。
type ttsServerEvent struct {
	Type  string          `json:"type"`
	Delta string          `json:"delta"` // response.audio.delta 的 base64 音频分片
	Error json.RawMessage `json:"error"` // error 事件负载；可能为 JSON 对象或字符串
}

// formatTtsServerError 按服务端错误负载生成错误消息，形状与 Java L384-397 逐字对齐：
// 对象负载取 type/code/message（缺省 "unknown"/"Unknown error"），格式
// "TTS Error [%s/%s]: %s"；非对象负载（Java errorElement.toString()）作为消息原文。
// 消息由服务端生成，不含本端 APIKey。
func formatTtsServerError(raw json.RawMessage) string {
	var obj struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Type == "" {
			obj.Type = "unknown"
		}
		if obj.Code == "" {
			obj.Code = "unknown"
		}
		if obj.Message == "" {
			obj.Message = "Unknown error"
		}
		return fmt.Sprintf("TTS Error [%s/%s]: %s", obj.Type, obj.Code, obj.Message)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return fmt.Sprintf("TTS Error [unknown/unknown]: %s", text)
	}
	return fmt.Sprintf("TTS Error [unknown/unknown]: %s", string(raw))
}

// chunks 增量音频收集器（等价 Java ByteArrayContainer：追加均摊 O(1)）。
type chunks struct {
	buf []byte
}

func (c *chunks) append(b []byte) { c.buf = append(c.buf, b...) }
func (c *chunks) len() int        { return len(c.buf) }
func (c *chunks) bytes() []byte   { return c.buf }
