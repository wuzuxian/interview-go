// 进程内假 DashScope ASR（OmniRealtime）服务端夹具（spec 测试决策：真协议客户端打假端点）。
// 供本票契约测试与后续管道测试（票 04/09）复用：可脚本化在连接建立后推送任意事件序列，
// 支持延迟、乱序关闭、拒绝握手与挂起握手。只服务外部行为断言，不 mock 客户端内部步骤。
package voiceinterview

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// asrFakeEvent 脚本化事件：连接建立后延迟 delay 推送 msg（JSON 文本帧）。
type asrFakeEvent struct {
	delay time.Duration
	msg   map[string]any
}

// fakeAsrServer 假 ASR 服务端。字段在 newFakeAsrServer 之后、客户端建连之前设置。
type fakeAsrServer struct {
	t   *testing.T
	srv *httptest.Server

	// 脚本化行为。
	script          []asrFakeEvent // 每条被接受的连接建立后自动依次推送
	rejectHandshake bool           // 拒绝升级（HTTP 403），客户端建连立即失败
	stallHandshake  bool           // 接受 TCP/HTTP 但不完成升级，客户端握手挂起（ready 超时用）

	mu sync.Mutex
	// 观测记录。
	handshakes     []string         // 每次升级请求的 RequestURI（含 ?model= 查询参数）
	authHeaders    []string         // 每次升级请求的 Authorization 头
	inspectHeaders []string         // 每次升级请求的 X-DashScope-DataInspection 头
	conns          []*fakeAsrConn   // 被接受的连接（按接入顺序）
	sessionUpdates []map[string]any // 每次收到的 session.update 内的 session 对象
	appends        []string         // 每次收到的 input_audio_buffer.append 的 audio 原文（base64）
	finishCount    int              // 收到的 session.finish 计数

	release chan struct{} // 关闭服务端时释放挂起的握手连接
}

// fakeAsrConn 服务端侧一条已升级的连接。
type fakeAsrConn struct {
	conn *websocket.Conn
	wmu  sync.Mutex

	closedByPeer bool // 客户端侧已关闭（服务端读循环因此退出）
}

// isClosedByServer 观测：服务端读循环是否已因客户端关闭而退出。
func (c *fakeAsrConn) isClosedByServer() bool {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.closedByPeer
}

// send 向客户端推送一条 JSON 事件（文本帧）。
func (c *fakeAsrConn) send(msg map[string]any) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.conn.WriteJSON(msg)
}

// close 服务端主动关闭连接（客户端读泵收到关闭）。
func (c *fakeAsrConn) close(code int, reason string) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	_ = c.conn.Close()
}

func newFakeAsrServer(t *testing.T) *fakeAsrServer {
	t.Helper()
	s := &fakeAsrServer{t: t, release: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

func (s *fakeAsrServer) Close() {
	s.mu.Lock()
	conns := append([]*fakeAsrConn(nil), s.conns...)
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.conn.Close()
	}
	close(s.release) // 释放挂起的握手连接（重复 Close 由测试保证只发生一次：t.Cleanup 单次）
	s.srv.Close()
}

// wsURL 客户端应配置的 ASR 服务地址（OmniRealtimeParam 会在其后拼 ?model=<model>）。
func (s *fakeAsrServer) wsURL() string {
	return "ws" + strings.TrimPrefix(s.srv.URL, "http")
}

func (s *fakeAsrServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.handshakes = append(s.handshakes, r.URL.RequestURI())
	s.authHeaders = append(s.authHeaders, r.Header.Get("Authorization"))
	s.inspectHeaders = append(s.inspectHeaders, r.Header.Get("X-DashScope-DataInspection"))
	script := append([]asrFakeEvent(nil), s.script...)
	reject, stall := s.rejectHandshake, s.stallHandshake
	s.mu.Unlock()

	if reject {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if stall {
		// 劫持连接但不响应升级：客户端 Dial 挂起直至握手超时（模拟网络黑洞）。
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		<-s.release
		_ = conn.Close()
		return
	}

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	fc := &fakeAsrConn{conn: ws}
	s.mu.Lock()
	s.conns = append(s.conns, fc)
	s.mu.Unlock()

	go s.readLoop(fc)
	go s.playScript(fc, script)
}

// readLoop 记录客户端发来的线协议消息（session.update / input_audio_buffer.append / session.finish）。
func (s *fakeAsrServer) readLoop(fc *fakeAsrConn) {
	for {
		_, data, err := fc.conn.ReadMessage()
		if err != nil {
			fc.wmu.Lock()
			fc.closedByPeer = true
			fc.wmu.Unlock()
			return
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		s.mu.Lock()
		switch msg["type"] {
		case "session.update":
			if session, ok := msg["session"].(map[string]any); ok {
				s.sessionUpdates = append(s.sessionUpdates, session)
			}
		case "input_audio_buffer.append":
			if audio, ok := msg["audio"].(string); ok {
				s.appends = append(s.appends, audio)
			}
		case "session.finish":
			s.finishCount++
		}
		s.mu.Unlock()
	}
}

func (s *fakeAsrServer) playScript(fc *fakeAsrConn, script []asrFakeEvent) {
	for _, ev := range script {
		if ev.delay > 0 {
			time.Sleep(ev.delay)
		}
		if err := fc.send(ev.msg); err != nil {
			return
		}
	}
}

// ===== 观测访问器（快照，供测试断言） =====

func (s *fakeAsrServer) snapshot() (conns []*fakeAsrConn, handshakes, auths, inspects []string,
	updates []map[string]any, appends []string, finishes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*fakeAsrConn(nil), s.conns...),
		append([]string(nil), s.handshakes...),
		append([]string(nil), s.authHeaders...),
		append([]string(nil), s.inspectHeaders...),
		append([]map[string]any(nil), s.sessionUpdates...),
		append([]string(nil), s.appends...),
		s.finishCount
}

func (s *fakeAsrServer) connCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func (s *fakeAsrServer) handshakeURIs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.handshakes...)
}

func (s *fakeAsrServer) sessionUpdateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessionUpdates)
}

func (s *fakeAsrServer) appendPayloads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.appends...)
}

func (s *fakeAsrServer) finishTotal() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishCount
}

func (s *fakeAsrServer) connAt(i int) *fakeAsrConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.conns) {
		s.t.Fatalf("假服务端只有 %d 条连接，访问第 %d 条", len(s.conns), i)
	}
	return s.conns[i]
}

// lastSessionUpdate 最近一次收到的 session 对象（无则让测试失败）。
func (s *fakeAsrServer) lastSessionUpdate() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessionUpdates) == 0 {
		s.t.Fatal("假服务端未收到 session.update")
	}
	return s.sessionUpdates[len(s.sessionUpdates)-1]
}

// push 向第 i 条连接推送事件（乱序/跨连接脚本化）。
func (s *fakeAsrServer) push(i int, msg map[string]any) {
	s.t.Helper()
	if err := s.connAt(i).send(msg); err != nil {
		s.t.Fatalf("向连接 %d 推送事件失败: %v", i, err)
	}
}

// pushAfter 延迟后向第 i 条连接推送事件（在后台 goroutine 执行）。
func (s *fakeAsrServer) pushAfter(i int, delay time.Duration, msg map[string]any) {
	go func() {
		time.Sleep(delay)
		_ = s.connAt(i).send(msg)
	}()
}

// closeConn 关闭第 i 条连接（code/reason 对齐 OmniRealtimeConversation close(1000, "bye")）。
func (s *fakeAsrServer) closeConn(i int, code int, reason string) {
	s.connAt(i).close(code, reason)
}
