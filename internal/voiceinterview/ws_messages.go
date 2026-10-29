// WebSocket 上下行消息契约：下行六类（control/subtitle/text/audio/audio_chunk/error）
// 与上行两类（audio/control）的 JSON 形状逐字对齐 Java
// VoiceInterviewWebSocketHandler（L184-981 的 Map.of 序列化与
// dto/WebSocketControlMessage、dto/WebSocketSubtitleMessage）。
// 键集由 ws_messages_test.go 黄金清单锁死：Java Map.of/Lombok 序列化恒输出全部键
// （无 omitempty 语义），Go 侧结构体字段一一对应，零值也必须输出。
package voiceinterview

import "time"

// wsControlMessage 上行控制消息（Java dto/WebSocketControlMessage：type/action/phase/data，
// @JsonIgnoreProperties 忽略未知字段；action 取值 submit/end_interview/start_phase）。
type wsControlMessage struct {
	Type   string         `json:"type"`
	Action string         `json:"action"`
	Phase  string         `json:"phase"`
	Data   map[string]any `json:"data"`
}

// 上行消息 type 取值（Java handleTextMessage 的 switch 分支）。
const (
	upstreamTypeAudio   = "audio"
	upstreamTypeControl = "control"
)

// wsControlAction 上行 action 取值（Java handleControl 的 switch 分支）。
const (
	controlActionSubmit       = "submit"
	controlActionEndInterview = "end_interview"
	controlActionStartPhase   = "start_phase"
)

// 下行 control 的 action 取值（Java sendAsrStatus/sendAudioComplete/sendPauseWarning/
// handlePauseTimeout 与 createWelcomeMessage 的字面量）。
const (
	downActionWelcome         = "welcome"
	downActionAsrReady        = "asr_ready"
	downActionAsrReconnecting = "asr_reconnecting"
	downActionAudioComplete   = "audio_complete"
	downActionPauseWarn       = "pause_timeout_warning"
	downActionPauseTimeout    = "pause_timeout"
)

// 下行 control 的 message 话术（Java 逐字）。
const (
	msgWelcome         = "连接成功，准备开始语音面试"
	msgAsrReady        = "语音识别已就绪"
	msgAsrReconnecting = "语音识别连接较慢，正在自动重连"
	msgAudioComplete   = "面试官语音播放完成"
	msgPauseWarn       = "会话将在30秒后暂停，请继续说话或点击继续"
	// Java 原文此处为半角逗号（"已暂停,可在"），非笔误，逐字保留。
	msgPauseTimeout = "会话因超时已暂停,可在历史记录中恢复"
)

// 下行错误话术（Java sendError 调用点与 getErrorMessage 映射逐字）。
const (
	msgErrInit           = "初始化语音识别失败: "               // + 错误信息（afterConnectionEstablished catch）
	msgErrSTT            = "语音识别失败: "                  // + 错误信息（startTranscription error 回调）
	msgErrAsrReadyTimout = "语音识别连接准备超时，请检查语音服务配置或稍后重试" // checkAsrReadyOrRetry 耗尽
	msgErrAsrDown        = "语音识别连接中断，请刷新页面后重试"         // 重启重发 15 次仍失败
	msgErrControl        = "控制消息处理失败: "                // + 错误信息
	msgErrHandle         = "消息处理失败: "                  // + 错误信息
	msgErrTurn           = "AI响应失败: "                  // + 错误信息（triggerLlmResponse catch）
	msgErrAudioPrefix    = "语音处理失败："                   // + 错误信息（getErrorMessage 默认，全角冒号）
)

// wsControlStatus 下行 control 消息（Java：Map.of("type","control","action",..,
// "message",..,"timestamp",System.currentTimeMillis())，四键恒输出）。
type wsControlStatus struct {
	Type      string `json:"type"`
	Action    string `json:"action"`
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
}

func buildControlStatus(action, message string) wsControlStatus {
	return wsControlStatus{
		Type:      "control",
		Action:    action,
		Message:   message,
		Timestamp: time.Now().UnixMilli(),
	}
}

// buildWelcome 建连欢迎（Java createWelcomeMessage L184-191）。
func buildWelcome() wsControlStatus { return buildControlStatus(downActionWelcome, msgWelcome) }

// buildAsrReady / buildAsrReconnecting ASR 就绪状态（Java sendAsrReady/sendAsrStatus L941-955）。
func buildAsrReady() wsControlStatus { return buildControlStatus(downActionAsrReady, msgAsrReady) }

func buildAsrReconnecting() wsControlStatus {
	return buildControlStatus(downActionAsrReconnecting, msgAsrReconnecting)
}

// buildAudioComplete 回合音频播放完成（Java sendAudioComplete L971-981，票 06 管道消费）。
func buildAudioComplete() wsControlStatus {
	return buildControlStatus(downActionAudioComplete, msgAudioComplete)
}

// buildPauseTimeoutWarning / buildPauseTimeout 不活动暂停提示（Java L1169-1195，票 09 调度消费）。
func buildPauseTimeoutWarning() wsControlStatus {
	return buildControlStatus(downActionPauseWarn, msgPauseWarn)
}

func buildPauseTimeout() wsControlStatus {
	return buildControlStatus(downActionPauseTimeout, msgPauseTimeout)
}

// wsSubtitleMessage 下行字幕（Java dto/WebSocketSubtitleMessage：type/text/isFinal 三键）。
type wsSubtitleMessage struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	IsFinal bool   `json:"isFinal"`
}

func buildSubtitle(text string, isFinal bool) wsSubtitleMessage {
	return wsSubtitleMessage{Type: "subtitle", Text: text, IsFinal: isFinal}
}

// wsTextMessage 下行流式/最终文本（Java sendTextMessage L929-935：type/content/final）。
type wsTextMessage struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Final   bool   `json:"final"`
}

func buildTextMessage(content string, final bool) wsTextMessage {
	return wsTextMessage{Type: "text", Content: content, Final: final}
}

// wsAudioMessage 下行整段音频（Java sendAudio L911-923：type/data(base64 WAV)/text）。
type wsAudioMessage struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Text string `json:"text"`
}

func buildAudioMessage(wavBase64, text string) wsAudioMessage {
	return wsAudioMessage{Type: "audio", Data: wavBase64, Text: text}
}

// wsAudioChunkMessage 下行分句音频块（Java sendAudioChunk L957-969：type/data/index/isLast；
// 票 06 的分句发射器消费）。
type wsAudioChunkMessage struct {
	Type   string `json:"type"`
	Data   string `json:"data"`
	Index  int    `json:"index"`
	IsLast bool   `json:"isLast"`
}

func buildAudioChunkMessage(wavBase64 string, index int, isLast bool) wsAudioChunkMessage {
	return wsAudioChunkMessage{Type: "audio_chunk", Data: wavBase64, Index: index, IsLast: isLast}
}

// wsErrorMessage 下行错误（Java sendError L937-939：type/message 两键）。
type wsErrorMessage struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func buildErrorMessage(message string) wsErrorMessage {
	return wsErrorMessage{Type: "error", Message: message}
}
