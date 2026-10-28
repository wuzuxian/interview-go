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

// 积压超限关闭连接（Java bufferLimit 语义：超限判定连接不可靠并关闭）；
// 关闭后的发送安全丢弃。
func TestWSSink积压超限关闭连接(t *testing.T) {
	_, sink := newTestSink(t, 64) // 上限 64 字节

	// 客户端不读取，写出阻塞后队列积压；累计字节超过上限 → 连接被关闭。
	big := make([]byte, 100)
	for i := 0; i < 50 && !sink.isClosed(); i++ {
		sink.send(big)
		time.Sleep(5 * time.Millisecond)
	}
	if !sink.isClosed() {
		t.Fatal("积压超限应关闭连接")
	}
	sink.send(big) // 关闭后安全丢弃
}
