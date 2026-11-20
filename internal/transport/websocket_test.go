package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/collector"
)

const testToken = "test-token"

// fakeMonitor は監視 Worker の代わりのテスト用 WebSocket サーバー。
type fakeMonitor struct {
	t      *testing.T
	server *httptest.Server
	// received は受け取ったメッセージ。
	received chan []byte
	// conns は接続のたびに送られる。テストから切断などに使う。
	conns chan *websocket.Conn
	// closes はエージェントから閉じられたときのクローズコード。
	closes chan websocket.StatusCode

	mu       sync.Mutex
	status   int // 0 以外なら Upgrade せずにこのステータスを返す
	dials    int
	lastAuth string
	lastUA   string
}

func newFakeMonitor(t *testing.T) *fakeMonitor {
	t.Helper()
	m := &fakeMonitor{t: t, received: make(chan []byte, 16), conns: make(chan *websocket.Conn, 16), closes: make(chan websocket.StatusCode, 16)}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)
	return m
}

func (m *fakeMonitor) url() string {
	return "ws" + strings.TrimPrefix(m.server.URL, "http") + "/agent/connect"
}

func (m *fakeMonitor) setStatus(status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = status
}

func (m *fakeMonitor) dialCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dials
}

func (m *fakeMonitor) handle(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.dials++
	m.lastAuth = r.Header.Get("Authorization")
	m.lastUA = r.Header.Get("User-Agent")
	status := m.status
	m.mu.Unlock()

	if r.URL.Path != "/agent/connect" {
		http.NotFound(w, r)
		return
	}
	if status != 0 {
		http.Error(w, "error", status)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		m.t.Errorf("accept: %v", err)
		return
	}
	m.conns <- conn
	for {
		_, data, err := conn.Read(context.Background())
		if err != nil {
			m.closes <- websocket.CloseStatus(err)
			return
		}
		m.received <- data
	}
}

func (m *fakeMonitor) nextConn(t *testing.T) *websocket.Conn {
	t.Helper()
	select {
	case c := <-m.conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no connection")
		return nil
	}
}

func (m *fakeMonitor) nextMessage(t *testing.T) metricsMessage {
	t.Helper()
	select {
	case data := <-m.received:
		var msg metricsMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("invalid message %s: %v", data, err)
		}
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
		return metricsMessage{}
	}
}

// fakeClock は手で進める時計。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// syncBuffer は複数のゴルーチンから書き込まれるログを受け取る。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newTestSender(t *testing.T, url, token string) (*WebSocket, *fakeClock, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	w := NewWebSocket(url, token, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	w.now = clock.Now
	// 待ち時間を常に上限（d）にする
	w.jitter = func() float64 { return 1 }
	t.Cleanup(func() { _ = w.Close() })
	return w, clock, logs
}

func testSnapshot(at time.Time) *collector.Snapshot {
	return &collector.Snapshot{
		CollectedAt: at,
		Metrics: collector.Metrics{
			CPU:    collector.CPUMetrics{UsagePercent: 12.5, Cores: []float64{10, 15}},
			Memory: collector.MemoryMetrics{TotalBytes: 100, UsedBytes: 40},
			Disks:  []collector.DiskMetrics{},
			GPUs:   []collector.GPUMetrics{},
		},
	}
}

// waitDisconnected は Sender が切断に気付く（読み込みが終わる）まで待つ。
func waitDisconnected(t *testing.T, w *WebSocket) {
	t.Helper()
	w.mu.Lock()
	c := w.conn
	w.mu.Unlock()
	if c == nil {
		t.Fatal("not connected")
	}
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect was not detected")
	}
}

func TestWebSocketSend(t *testing.T) {
	m := newFakeMonitor(t)
	w, _, _ := newTestSender(t, m.url(), testToken)

	at := time.UnixMilli(1_700_000_000_123)
	if err := w.Send(t.Context(), testSnapshot(at)); err != nil {
		t.Fatal(err)
	}
	msg := m.nextMessage(t)
	if msg.V != 1 || msg.Type != "metrics" || msg.MeasuredAt != at.UnixMilli() {
		t.Errorf("message = %+v", msg)
	}
	if msg.Metrics.CPU.UsagePercent != 12.5 || msg.Metrics.Memory.UsedBytes != 40 {
		t.Errorf("metrics = %+v", msg.Metrics)
	}
	m.mu.Lock()
	auth, ua := m.lastAuth, m.lastUA
	m.mu.Unlock()
	if auth != "Bearer "+testToken {
		t.Errorf("Authorization = %q", auth)
	}
	if !strings.HasPrefix(ua, "shimamura-telemetry/") {
		t.Errorf("User-Agent = %q", ua)
	}

	// 2 件目は同じ接続で送る
	if err := w.Send(t.Context(), testSnapshot(at.Add(5*time.Second))); err != nil {
		t.Fatal(err)
	}
	m.nextMessage(t)
	if n := m.dialCount(); n != 1 {
		t.Errorf("dials = %d, want 1", n)
	}
}

// 送るメッセージの JSON のキー名が監視 Worker の形式と一致することを確認する。
func TestWebSocketMessageJSON(t *testing.T) {
	m := newFakeMonitor(t)
	w, _, _ := newTestSender(t, m.url(), testToken)
	if err := w.Send(t.Context(), testSnapshot(time.UnixMilli(1))); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(<-m.received, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"v", "type", "measuredAt", "metrics"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing key %q: %v", key, got)
		}
	}
	if len(got) != 4 {
		t.Errorf("unexpected keys: %v", got)
	}
}

func TestWebSocketAuthFailure(t *testing.T) {
	m := newFakeMonitor(t)
	w, clock, logs := newTestSender(t, m.url(), "wrong-token")

	err := w.Send(t.Context(), testSnapshot(clock.Now()))
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
	if !strings.Contains(logs.String(), "authentication failed") {
		t.Errorf("logs = %s", logs.String())
	}

	// 待ち時間中は接続しない
	clock.Advance(authRetryDelay - time.Second)
	if err := w.Send(t.Context(), testSnapshot(clock.Now())); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v", err)
	}
	if n := m.dialCount(); n != 1 {
		t.Errorf("dials = %d, want 1", n)
	}

	// 待ち時間を過ぎたら接続し直す。同じログは繰り返さない
	clock.Advance(time.Second)
	_ = w.Send(t.Context(), testSnapshot(clock.Now()))
	if n := m.dialCount(); n != 2 {
		t.Errorf("dials = %d, want 2", n)
	}
	if n := strings.Count(logs.String(), "authentication failed"); n != 1 {
		t.Errorf("auth failure logged %d times", n)
	}
}

func TestWebSocketReconnectAfterServerClose(t *testing.T) {
	m := newFakeMonitor(t)
	w, clock, logs := newTestSender(t, m.url(), testToken)

	if err := w.Send(t.Context(), testSnapshot(clock.Now())); err != nil {
		t.Fatal(err)
	}
	m.nextMessage(t)

	// IdP の再デプロイなどでサーバーから閉じられた
	conn := m.nextConn(t)
	_ = conn.Close(websocket.StatusServiceRestart, "restarting")
	waitDisconnected(t, w)

	// 1 回目の待ち時間（backoffBase）の間は送らない
	if err := w.Send(t.Context(), testSnapshot(clock.Now())); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
	if !strings.Contains(logs.String(), "disconnected from the monitor") || !strings.Contains(logs.String(), "code=1012") {
		t.Errorf("logs = %s", logs.String())
	}

	clock.Advance(backoffBase)
	if err := w.Send(t.Context(), testSnapshot(clock.Now())); err != nil {
		t.Fatal(err)
	}
	m.nextMessage(t)
	if n := m.dialCount(); n != 2 {
		t.Errorf("dials = %d, want 2", n)
	}
}

func TestWebSocketReconnectAfterNetworkFailure(t *testing.T) {
	m := newFakeMonitor(t)
	w, clock, _ := newTestSender(t, m.url(), testToken)

	if err := w.Send(t.Context(), testSnapshot(clock.Now())); err != nil {
		t.Fatal(err)
	}
	m.nextMessage(t)

	// サーバーが落ちて接続できない（ネットワーク断と同じ）
	m.setStatus(http.StatusBadGateway)
	conn := m.nextConn(t)
	_ = conn.CloseNow()
	waitDisconnected(t, w)

	// 失敗のたびに待ち時間が伸びる: 1s（切断）→ 2s → 4s
	wantDelays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	for i, d := range wantDelays {
		clock.Advance(d)
		err := w.Send(t.Context(), testSnapshot(clock.Now()))
		if i < len(wantDelays)-1 && !errors.Is(err, ErrNotConnected) {
			t.Fatalf("attempt %d: err = %v, want ErrNotConnected", i, err)
		}
		if i == len(wantDelays)-2 {
			// 復旧した
			m.setStatus(0)
		}
	}
	m.nextMessage(t)
	if n := m.dialCount(); n != 4 {
		t.Errorf("dials = %d, want 4", n)
	}
}

func TestWebSocketCloseCodes(t *testing.T) {
	tests := []struct {
		name      string
		code      websocket.StatusCode
		wantDelay time.Duration
		wantLog   string
	}{
		{"revoked", closeRevoked, authRetryDelay, "agent token was revoked"},
		{"replaced", closeReplaced, replacedRetryDelay, "another agent connected"},
		{"invalid message", closeInvalidMessage, backoffBase, "rejected a message"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newFakeMonitor(t)
			w, clock, logs := newTestSender(t, m.url(), testToken)
			if err := w.Send(t.Context(), testSnapshot(clock.Now())); err != nil {
				t.Fatal(err)
			}
			m.nextMessage(t)
			_ = m.nextConn(t).Close(tt.code, "bye")
			waitDisconnected(t, w)

			clock.Advance(tt.wantDelay - time.Millisecond)
			if err := w.Send(t.Context(), testSnapshot(clock.Now())); !errors.Is(err, ErrNotConnected) {
				t.Fatalf("err = %v, want ErrNotConnected", err)
			}
			clock.Advance(time.Millisecond)
			if err := w.Send(t.Context(), testSnapshot(clock.Now())); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(logs.String(), tt.wantLog) {
				t.Errorf("logs = %s", logs.String())
			}
		})
	}
}

func TestWebSocketLogsErrorMessages(t *testing.T) {
	m := newFakeMonitor(t)
	w, clock, logs := newTestSender(t, m.url(), testToken)
	if err := w.Send(t.Context(), testSnapshot(clock.Now())); err != nil {
		t.Fatal(err)
	}
	conn := m.nextConn(t)
	if err := conn.Write(t.Context(), websocket.MessageText, []byte(`{"v":1,"type":"error","message":"メッセージの形式が不正です"}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "メッセージの形式が不正です") {
		if time.Now().After(deadline) {
			t.Fatalf("logs = %s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWebSocketMessageTooLarge(t *testing.T) {
	m := newFakeMonitor(t)
	w, clock, _ := newTestSender(t, m.url(), testToken)
	snap := testSnapshot(clock.Now())
	snap.Metrics.Disks = make([]collector.DiskMetrics, 2000)
	for i := range snap.Metrics.Disks {
		snap.Metrics.Disks[i] = collector.DiskMetrics{Mount: strings.Repeat("x", 50)}
	}
	err := w.Send(t.Context(), snap)
	if err == nil || errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want too large error", err)
	}
	if n := m.dialCount(); n != 0 {
		t.Errorf("dials = %d, want 0", n)
	}
}

func TestWebSocketClose(t *testing.T) {
	m := newFakeMonitor(t)
	w, clock, _ := newTestSender(t, m.url(), testToken)
	if err := w.Send(t.Context(), testSnapshot(clock.Now())); err != nil {
		t.Fatal(err)
	}
	m.nextConn(t)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// サーバー側は正常終了のクローズを受け取る
	select {
	case got := <-m.closes:
		if got != websocket.StatusNormalClosure {
			t.Errorf("close status = %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not see the close")
	}
	if err := w.Send(t.Context(), testSnapshot(clock.Now())); err == nil {
		t.Error("send after close succeeded")
	}
	if err := w.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

func TestWebSocketBackoff(t *testing.T) {
	w := NewWebSocket("ws://localhost/agent/connect", testToken, slog.New(slog.DiscardHandler))
	w.now = func() time.Time { return time.Unix(0, 0) }

	w.jitter = func() float64 { return 1 }
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	for i, sec := range want {
		if got := w.backoff(); got != sec*time.Second {
			t.Errorf("attempt %d: backoff = %v, want %v", i, got, sec*time.Second)
		}
	}
	if got := w.nextDialAt; !got.Equal(time.Unix(60, 0)) {
		t.Errorf("nextDialAt = %v", got)
	}

	// ジッターは上限の半分まで
	w.attempts = 0
	w.jitter = func() float64 { return 0 }
	if got := w.backoff(); got != 500*time.Millisecond {
		t.Errorf("backoff with zero jitter = %v, want 500ms", got)
	}

	// 長く続いた接続の後はバックオフを初期値に戻す
	w.attempts = 5
	w.jitter = func() float64 { return 1 }
	w.conn = &connection{ws: dummyConn(t), connectedAt: time.Unix(0, 0).Add(-stableAfter), done: make(chan struct{})}
	w.disconnected(errors.New("eof"), w.now())
	if !w.nextDialAt.Equal(time.Unix(1, 0)) {
		t.Errorf("nextDialAt after stable connection = %v, want +1s", w.nextDialAt)
	}
}

// dummyConn は disconnected のテスト用に、すぐ閉じられる接続を作る。
func dummyConn(t *testing.T) *websocket.Conn {
	t.Helper()
	m := newFakeMonitor(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, m.url(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + testToken}},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewWebSocketWarnsWithoutTLS(t *testing.T) {
	tests := []struct {
		url  string
		warn bool
	}{
		{"ws://monitor.example.com/agent/connect", true},
		{"wss://monitor.example.com/agent/connect", false},
		{"ws://localhost:8787/agent/connect", false},
		{"ws://127.0.0.1:8787/agent/connect", false},
	}
	for _, tt := range tests {
		var logs bytes.Buffer
		NewWebSocket(tt.url, testToken, slog.New(slog.NewTextHandler(&logs, nil)))
		if got := strings.Contains(logs.String(), "without TLS"); got != tt.warn {
			t.Errorf("%s: warned = %v, want %v", tt.url, got, tt.warn)
		}
	}
}
