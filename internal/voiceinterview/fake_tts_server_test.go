package voiceinterview

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeTtsServer 进程内假 DashScope qwen-tts-realtime 服务端（契约测试夹具，
// spec 测试决策：真客户端 × 假服务端，全程无外网）。按连接序脚本化 commit 后
// 的行为：正常合成、部分音频后停滞（触发超时）、error 事件；另提供拒连服务端
// 与挂起握手的裸 TCP 监听构造器。
//
// 服务端行为对齐 DashScope 协议：收到 session.update 回 session.updated；
// 收到 input_text_buffer.append 记录文本；收到 input_text_buffer.commit 执行脚本。
type fakeTtsServer struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// conns 已接受的 WebSocket 连接数（每次合成应恰好 +1）。
	conns int
	// authHeaders 每个连接握手请求的 Authorization 头（按连接序）。
	authHeaders []string
	// frames 收到的全部客户端帧（按序，event_id 保留原样供断言）。
	frames []map[string]any
	// scripts 每个连接 commit 后的脚本（按连接序；不足时最后一个复用）。
	scripts []func(fs *fakeTtsServer, conn *websocket.Conn)
}

// newFakeTtsServer 启动假服务端；调用方用 scripts 字段按连接注册 commit 后脚本。
func newFakeTtsServer(t *testing.T) *fakeTtsServer {
	t.Helper()
	fs := &fakeTtsServer{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/", fs.handleWS)
	fs.srv = httptest.NewServer(mux)
	t.Cleanup(fs.srv.Close)
	return fs
}

// URL 返回假服务端的 ws:// 地址（客户端 TtsSettings.URL 直接使用）。
func (f *fakeTtsServer) URL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

func (f *fakeTtsServer) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	f.mu.Lock()
	connIndex := f.conns
	f.conns++
	f.authHeaders = append(f.authHeaders, r.Header.Get("Authorization"))
	f.mu.Unlock()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return // 客户端超时关闭或连接中断，结束该连接的服务
		}
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			return
		}
		f.mu.Lock()
		f.frames = append(f.frames, frame)
		f.mu.Unlock()

		switch frameType, _ := frame["type"].(string); frameType {
		case "session.update":
			_ = conn.WriteJSON(map[string]any{
				"event_id": "srv_session_updated",
				"type":     "session.updated",
				"session":  map[string]any{"id": "sess_fake_1"},
			})
		case "input_text_buffer.commit":
			f.mu.Lock()
			script := f.scripts[min(connIndex, len(f.scripts)-1)]
			f.mu.Unlock()
			if script != nil {
				script(f, conn)
			}
		}
	}
}

// ConnectionCount 返回已接受的连接数。
func (f *fakeTtsServer) ConnectionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

// Frames 返回收到的客户端帧副本（按序）。
func (f *fakeTtsServer) Frames() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.frames...)
}

// AppendedText 拼接全部 input_text_buffer.append 帧的文本。
func (f *fakeTtsServer) AppendedText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, frame := range f.frames {
		if frameType, _ := frame["type"].(string); frameType == "input_text_buffer.append" {
			text, _ := frame["text"].(string)
			b.WriteString(text)
		}
	}
	return b.String()
}

// AuthHeader 返回第 i 个连接的握手 Authorization 头。
func (f *fakeTtsServer) AuthHeader(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authHeaders[i]
}

// sendEvent 服务端向客户端发送一个事件帧。
func (f *fakeTtsServer) sendEvent(conn *websocket.Conn, event map[string]any) {
	f.t.Helper()
	if err := conn.WriteJSON(event); err != nil {
		f.t.Errorf("假服务端发送事件失败: %v", err)
	}
}

// scriptSynthDone 正常合成脚本：response.created → 逐个音频增量 → response.done。
func scriptSynthDone(chunks ...[]byte) func(*fakeTtsServer, *websocket.Conn) {
	return func(fs *fakeTtsServer, conn *websocket.Conn) {
		fs.sendEvent(conn, map[string]any{
			"event_id": "srv_response_created", "type": "response.created",
			"response": map[string]any{"id": "resp_fake_1"},
		})
		for _, chunk := range chunks {
			fs.sendEvent(conn, map[string]any{
				"event_id": "srv_delta", "type": "response.audio.delta",
				"delta": base64.StdEncoding.EncodeToString(chunk),
			})
		}
		fs.sendEvent(conn, map[string]any{
			"event_id": "srv_done", "type": "response.done",
			"response": map[string]any{"id": "resp_fake_1"},
		})
	}
}

// scriptStall 部分音频后停滞：发送若干增量后保持连接打开不再回复，
// 触发客户端合成超时（对齐票面"部分后停滞（触发超时）"）。
func scriptStall(chunks ...[]byte) func(*fakeTtsServer, *websocket.Conn) {
	return func(fs *fakeTtsServer, conn *websocket.Conn) {
		for _, chunk := range chunks {
			fs.sendEvent(conn, map[string]any{
				"event_id": "srv_delta", "type": "response.audio.delta",
				"delta": base64.StdEncoding.EncodeToString(chunk),
			})
		}
		// 不发 response.done，不关闭连接：客户端应在超时后放弃。
	}
}

// scriptErrorEvent error 事件脚本：commit 后直接回协议错误。
func scriptErrorEvent(errType, code, message string) func(*fakeTtsServer, *websocket.Conn) {
	return func(fs *fakeTtsServer, conn *websocket.Conn) {
		fs.sendEvent(conn, map[string]any{
			"event_id": "srv_err", "type": "error",
			"error": map[string]any{"type": errType, "code": code, "message": message},
		})
	}
}

// newRejectingTtsServer 构造握手前拒连的服务端（HTTP 403，未升级 WebSocket）。
func newRejectingTtsServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// newHangingTtsListener 构造挂起握手的裸 TCP 监听：accept 后不读不写，
// 客户端只能靠建连超时脱身（对齐票面"服务端挂起握手时按超时失败"）。
func newHangingTtsListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动挂起监听失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// 收下连接但永不回应，握手指尖挂起。
			go func(c net.Conn) {
				time.Sleep(time.Hour)
				_ = c.Close()
			}(conn)
		}
	}()
	return "ws://" + ln.Addr().String() + "/"
}
