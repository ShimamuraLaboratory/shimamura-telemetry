package spec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/collector"
)

var errNotConnected = errors.New("not connected")

type fakeConn struct {
	id     uint64
	errors uint64
	fail   bool
	sent   []string
}

func (c *fakeConn) ConnectionID() uint64 { return c.id }
func (c *fakeConn) ErrorCount() uint64   { return c.errors }

func (c *fakeConn) SendIfConnected(_ context.Context, v any) error {
	if c.id == 0 || c.fail {
		return errNotConnected
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.sent = append(c.sent, string(b))
	return nil
}

type fakeCollector struct {
	spec  collector.Spec
	calls int
}

func (f *fakeCollector) Spec(context.Context) (collector.Spec, error) {
	f.calls++
	return f.spec, nil
}

var t0 = time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)

type testReporter struct {
	*Reporter
	conn  *fakeConn
	col   *fakeCollector
	clock time.Time
	logs  *bytes.Buffer
}

func testSpec(mem uint64) collector.Spec {
	return collector.Spec{
		CPU:              collector.CPUSpec{Model: "AMD EPYC 7543 32-Core Processor", Sockets: 2, Cores: 64, Threads: 128},
		MemoryTotalBytes: mem,
		Disks:            []collector.DiskSpec{{Mount: "/", TotalBytes: 3840755982336}},
		GPUs:             []collector.GPUSpec{},
	}
}

func newTestReporter() *testReporter {
	tr := &testReporter{conn: &fakeConn{}, col: &fakeCollector{spec: testSpec(540431343616)}, clock: t0, logs: &bytes.Buffer{}}
	tr.Reporter = &Reporter{
		conn:      tr.conn,
		collector: tr.col,
		logger:    slog.New(slog.NewTextHandler(tr.logs, nil)),
		now:       func() time.Time { return tr.clock },
	}
	return tr
}

func (tr *testReporter) tick(d time.Duration) {
	tr.clock = tr.clock.Add(d)
	tr.step(context.Background())
}

func TestReporterSendsOnConnect(t *testing.T) {
	tr := newTestReporter()
	tr.tick(pollInterval)
	if len(tr.conn.sent) != 0 || tr.col.calls != 0 {
		t.Fatalf("sent while disconnected: %v (calls %d)", tr.conn.sent, tr.col.calls)
	}

	tr.conn.id = 1
	tr.tick(pollInterval)
	want := `{"v":1,"type":"spec","collectedAt":` + jsonInt(t0.Add(2*pollInterval).UnixMilli()) +
		`,"spec":{"cpu":{"model":"AMD EPYC 7543 32-Core Processor","sockets":2,"cores":64,"threads":128},` +
		`"memoryTotalBytes":540431343616,"disks":[{"mount":"/","totalBytes":3840755982336}],"gpus":[]}}`
	if len(tr.conn.sent) != 1 || tr.conn.sent[0] != want {
		t.Fatalf("sent =\n%v\nwant\n%s", tr.conn.sent, want)
	}

	// 変化が無ければ送らない
	for range 20 {
		tr.tick(pollInterval)
	}
	if len(tr.conn.sent) != 1 {
		t.Errorf("sent again: %v", tr.conn.sent)
	}

	// 接続し直したら必ず送る（同じ内容でも）
	tr.conn.id = 2
	tr.tick(pollInterval)
	if len(tr.conn.sent) != 2 {
		t.Errorf("not sent after reconnect: %v", tr.conn.sent)
	}
}

func TestReporterRechecksHourly(t *testing.T) {
	tr := newTestReporter()
	tr.conn.id = 1
	tr.tick(pollInterval)
	tr.tick(ackWindow)
	calls := tr.col.calls

	// 1 時間たつまでは取り直さない
	tr.tick(recheckInterval - time.Minute)
	if tr.col.calls != calls {
		t.Fatalf("collected before the recheck interval: %d", tr.col.calls)
	}
	// 1 時間後、同じなら送らない
	tr.tick(time.Minute)
	if tr.col.calls != calls+1 || len(tr.conn.sent) != 1 {
		t.Fatalf("calls = %d, sent = %v", tr.col.calls, tr.conn.sent)
	}

	// メモリを増設した → 次の確認で送る
	tr.col.spec = testSpec(1080862687232)
	tr.tick(recheckInterval)
	if len(tr.conn.sent) != 2 || !strings.Contains(tr.conn.sent[1], `"memoryTotalBytes":1080862687232`) {
		t.Fatalf("sent = %v", tr.conn.sent)
	}
	if !strings.Contains(tr.logs.String(), "hardware configuration changed") {
		t.Errorf("logs = %s", tr.logs.String())
	}
}

func TestReporterRetriesAfterErrorWithGap(t *testing.T) {
	tr := newTestReporter()
	tr.conn.id = 1
	tr.tick(pollInterval) // 送る
	tr.conn.errors++      // 監視 Worker が error を返した

	// 1 分（65 秒）より前には送り直さない
	for range 12 {
		tr.tick(pollInterval)
	}
	if len(tr.conn.sent) != 1 {
		t.Fatalf("resent too early: %d", len(tr.conn.sent))
	}
	tr.tick(pollInterval) // 65 秒
	if len(tr.conn.sent) != 2 || tr.conn.sent[0] != tr.conn.sent[1] {
		t.Fatalf("not resent: %v", tr.conn.sent)
	}
	// error が来なければ届いたとみなす
	tr.tick(ackWindow)
	if tr.lastSent == nil || tr.inflight != nil {
		t.Errorf("lastSent = %v, inflight = %v", tr.lastSent, tr.inflight)
	}
}

func TestReporterGivesUpAfterRetries(t *testing.T) {
	tr := newTestReporter()
	tr.conn.id = 1
	tr.tick(pollInterval)
	for range maxRetries + 2 {
		tr.conn.errors++
		tr.tick(minSendGap)
	}
	if got := len(tr.conn.sent); got != 1+maxRetries {
		t.Errorf("sent %d times, want %d", got, 1+maxRetries)
	}
	if n := strings.Count(tr.logs.String(), "rejected the hardware spec repeatedly"); n != 1 {
		t.Errorf("logged %d times", n)
	}

	// 再接続したら、また送る
	tr.conn.id = 2
	tr.tick(pollInterval)
	if got := len(tr.conn.sent); got != 2+maxRetries {
		t.Errorf("not sent after reconnect: %d", got)
	}
}

func TestReporterKeepsPendingWhenSendFails(t *testing.T) {
	tr := newTestReporter()
	tr.conn.id = 1
	tr.conn.fail = true
	tr.tick(pollInterval)
	if tr.pending == nil {
		t.Fatal("pending was dropped")
	}
	tr.conn.fail = false
	tr.tick(pollInterval)
	if len(tr.conn.sent) != 1 {
		t.Errorf("sent = %v", tr.conn.sent)
	}
}

func TestFitLimitsMessageSize(t *testing.T) {
	s := testSpec(1)
	for range 2000 {
		s.Disks = append(s.Disks, collector.DiskSpec{Mount: "/" + strings.Repeat("m", 200), TotalBytes: 1})
	}
	got := fit(message{V: 1, Type: "spec", CollectedAt: t0.UnixMilli(), Spec: s})
	b, _ := json.Marshal(got)
	if len(b) > maxMessageBytes || len(got.Spec.Disks) == 0 {
		t.Errorf("size = %d, disks = %d", len(b), len(got.Spec.Disks))
	}
}

// 大きさは、実際に送る collectedAt（13 桁）を含めて測る。0 で測ると収まっていても、送るときに上限を超えうる。
func TestFitCountsTimestamp(t *testing.T) {
	s := testSpec(1)
	s.Disks = nil
	// エスケープされる文字（"<" は \u003c の 6 バイトになる）で、上限ぎりぎりの大きさにする
	mount := "/" + strings.Repeat("<", 254)
	for range 64 {
		s.Disks = append(s.Disks, collector.DiskSpec{Mount: mount, TotalBytes: 18446744073709551615})
	}
	msg := fit(message{V: 1, Type: "spec", CollectedAt: t0.UnixMilli(), Spec: s})
	b, _ := json.Marshal(msg)
	if len(b) > maxMessageBytes {
		t.Errorf("message is %d bytes", len(b))
	}
	if msg.CollectedAt != t0.UnixMilli() {
		t.Errorf("collectedAt = %d", msg.CollectedAt)
	}
}

func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
