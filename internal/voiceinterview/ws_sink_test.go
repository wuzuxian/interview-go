// wsSink 发送保护单测（Java ConcurrentWebSocketSessionDecorator(10s, 512KB) 同构行为）：
// 关闭后发送安全丢弃；积压超限关闭连接。
package voiceinterview

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// sinkTestPair 起回显升级端点，返回客户端连接与服务端侧连接（服务端连接交给
// wsSink 驱动写循环，客户端连接用于观测下发）。
func sinkTestPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverCh := make(chan *websocket.Conn, 1)
	closed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverCh <- ws
		<-closed
	}))
	t.Cleanup(func() { close(closed); srv.Close() })

	clientConn, resp, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v (resp=%v)", err, resp)
	}
	t.Cleanup(func() { _ = clientConn.Close() })
	return clientConn, <-serverCh
}

func newTestSink(t *testing.T, limit int64) (*websocket.Conn, *wsSink) {
	t.Helper()
	clientConn, serverConn := sinkTestPair(t)
	sink := newWSSink(wsSinkParams{
		conn:         serverConn,
		limit:        limit,
		writeTimeout: 2 * time.Second,
		logger:       discardLogger(),
	})
	return clientConn, sink
}

// 正常路径：消息按序可达客户端；会话关闭后发送安全丢弃（不 panic、不阻塞）。
func TestWSSink发送与关闭后安全丢弃(t *testing.T) {
	clientConn, sink := newTestSink(t, 524288)

	sink.send([]byte(`{"n":1}`))
	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("首条消息应可达: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil || m["n"] != float64(1) {
		t.Fatalf("消息内容错误: %s", data)
	}

	sink.close()
	sink.close() // 幂等
	for i := 0; i < 100; i++ {
		sink.send([]byte(`{"after":"close"}`))
	}
	if !sink.isClosed() {
		t.Fatal("close 后应处于关闭态")
	}
}

// 空闲直写快路径：无发送中且无积压时，单条大消息不受积压上限约束
// （Java ConcurrentWebSocketSessionDecorator 直写快路径；bug4 回归——
// 942KB 开场音频曾因入队后按字节数判限被误杀连接）。
func TestWSSink空闲单条大消息不受积压上限(t *testing.T) {
	clientConn, sink := newTestSink(t, 64) // 上限仅 64 字节

	big := make([]byte, 1<<20) // 1MB >> 64B 上限
	sink.send(big)
	if sink.isClosed() {
		t.Fatal("空闲连接上的单条大消息不应触发积压上限")
	}
	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("大消息应完整可达: %v", err)
	}
	if len(data) != len(big) {
		t.Fatalf("消息长度不符: %d != %d", len(data), len(big))
	}
	if sink.isClosed() {
		t.Fatal("发送后连接不应被关闭")
	}
}

// 积压超限关闭连接（Java bufferLimit 语义：仅作用于发送中的排队堆积，
// 超限判定连接不可靠并关闭）；关闭后的发送安全丢弃。
func TestWSSink积压超限关闭连接(t *testing.T) {
	_, sink := newTestSink(t, 64) // 上限 64 字节

	// 白盒注入「发送中」：直写快路径被禁止，消息只能入队并按字节数判限。
	// （真实世界的阻塞写依赖对端不读取与内核缓冲耗尽，大小随环境浮动，
	// 以标志注入取代以保证确定性。）
	sink.sending.Store(true)

	small := make([]byte, 100)
	sink.send(small) // 100B > 64B 上限 → 判限关闭
	if !sink.isClosed() {
		t.Fatal("发送中积压超限应关闭连接")
	}
	sink.send(small) // 关闭后安全丢弃
}
