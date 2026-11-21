package logins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

var errNotConnected = errors.New("not connected")

// fakeConn は送ったメッセージを JSON で記録する Conn。
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

// fakeSource は与えた値を返す source。
type fakeSource struct {
	sessions  []Session
	backlog   []Event
	newEvents []Event
	since     time.Time
	err       error
	// wtmpErr は wtmp（Backlog・NewEvents）だけのエラー。
	wtmpErr error
}

func (s *fakeSource) Sessions() ([]Session, error) { return s.sessions, s.err }

func (s *fakeSource) Backlog(since time.Time) ([]Event, error) {
	s.since = since
	return s.backlog, cmpErr(s.wtmpErr, s.err)
}

func (s *fakeSource) NewEvents() ([]Event, error) {
	e := s.newEvents
	s.newEvents = nil
	return e, cmpErr(s.wtmpErr, s.err)
}

func cmpErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

type testReporter struct {
	*Reporter
	conn  *fakeConn
	src   *fakeSource
	clock time.Time
	logs  *bytes.Buffer
}

func newTestReporter() *testReporter {
	tr := &testReporter{conn: &fakeConn{}, src: &fakeSource{}, clock: t0, logs: &bytes.Buffer{}}
	tr.Reporter = &Reporter{
		conn:   tr.conn,
		source: tr.src,
		logger: slog.New(slog.NewTextHandler(tr.logs, nil)),
		now:    func() time.Time { return tr.clock },
	}
	return tr
}

// tick は時計を d 進めて 1 回分の処理をする。
func (tr *testReporter) tick(d time.Duration) {
	tr.clock = tr.clock.Add(d)
	tr.step(context.Background())
}

func (tr *testReporter) types() []string {
	var types []string
	for _, s := range tr.conn.sent {
		var m struct{ Type string }
		_ = json.Unmarshal([]byte(s), &m)
		types = append(types, m.Type)
	}
	return types
}

func login(user string, at int64) Event {
	return Event{User: user, TTY: "pts/0", Host: "localhost", At: at}
}

func TestReporterSendsInitialStateOnConnect(t *testing.T) {
	tr := newTestReporter()
	tr.src.sessions = []Session{{User: "lab", TTY: "pts/0", LoginAt: 1}}
	tr.src.backlog = []Event{login("lab", 1)}

	// 接続していない間は何もしない
	tr.tick(time.Second)
	if len(tr.conn.sent) != 0 {
		t.Fatalf("sent while disconnected: %v", tr.conn.sent)
	}

	tr.conn.id = 1
	tr.tick(time.Second)
	// 1 回に 1 件、1.5 秒以上あけて送る
	tr.tick(time.Second)
	tr.tick(time.Second)
	tr.tick(time.Second)
	if got := strings.Join(tr.types(), ","); got != "sessions,login_events" {
		t.Fatalf("sent = %v", tr.conn.sent)
	}
	if want := t0.Add(2 * time.Second).Add(-backlogWindow); !tr.src.since.Equal(want) {
		t.Errorf("backlog since = %v, want %v", tr.src.since, want)
	}
	var msg map[string]any
	_ = json.Unmarshal([]byte(tr.conn.sent[0]), &msg)
	if msg["v"] != float64(1) || msg["collectedAt"] != float64(t0.Add(2*time.Second).UnixMilli()) {
		t.Errorf("sessions message = %s", tr.conn.sent[0])
	}
}

func TestReporterSessionsOnChangeAndRefresh(t *testing.T) {
	tr := newTestReporter()
	tr.conn.id = 1
	tr.src.sessions = []Session{}
	tr.tick(time.Second) // 接続直後の sessions（空の一覧）
	for range 10 {
		tr.tick(time.Second)
	}
	if got := len(tr.conn.sent); got != 1 || tr.conn.sent[0] != fmt.Sprintf(`{"v":1,"type":"sessions","collectedAt":%d,"sessions":[]}`, t0.Add(time.Second).UnixMilli()) {
		t.Fatalf("sent = %v", tr.conn.sent)
	}

	// ログインした
	host := "localhost"
	tr.src.sessions = []Session{{User: "lab", TTY: "pts/0", Host: &host, LoginAt: 5}}
	tr.tick(time.Second)
	tr.tick(time.Second)
	if got := len(tr.conn.sent); got != 2 {
		t.Fatalf("not sent on change: %v", tr.conn.sent)
	}

	// 変化が無くても 5 分ごとに送る（前回は t0+12s に送った）
	tr.tick(sessionsRefresh - 5*time.Second) // t0+308s
	if got := len(tr.conn.sent); got != 2 {
		t.Fatalf("sent before refresh: %v", tr.conn.sent)
	}
	tr.tick(5 * time.Second) // t0+313s
	if got := len(tr.conn.sent); got != 3 {
		t.Fatalf("not refreshed: %v", tr.conn.sent)
	}
}

func TestReporterNewEventsAndChunking(t *testing.T) {
	tr := newTestReporter()
	tr.conn.id = 1
	tr.tick(time.Second)
	tr.tick(2 * time.Second) // sessions
	tr.conn.sent = nil

	for i := range MaxEventsPerMessage + 10 {
		tr.src.newEvents = append(tr.src.newEvents, login("lab", int64(i+1)))
	}
	tr.tick(2 * time.Second)
	tr.tick(2 * time.Second)
	tr.tick(2 * time.Second)
	if got := strings.Join(tr.types(), ","); got != "login_events,login_events" {
		t.Fatalf("sent = %v", tr.types())
	}
	var first, second struct{ Events []json.RawMessage }
	_ = json.Unmarshal([]byte(tr.conn.sent[0]), &first)
	_ = json.Unmarshal([]byte(tr.conn.sent[1]), &second)
	if len(first.Events) != MaxEventsPerMessage || len(second.Events) != 10 {
		t.Errorf("chunks = %d, %d", len(first.Events), len(second.Events))
	}
}

func TestReporterRetriesOnError(t *testing.T) {
	tr := newTestReporter()
	tr.conn.id = 1
	tr.src.sessions = []Session{}
	tr.tick(time.Second) // sessions を送る
	if len(tr.conn.sent) != 1 {
		t.Fatalf("sent = %v", tr.conn.sent)
	}

	// 監視 Worker が error を返した（間隔が短すぎるなど）→ 送り直す
	tr.conn.errors++
	tr.tick(time.Second)
	tr.tick(time.Second)
	if len(tr.conn.sent) != 2 || tr.conn.sent[0] != tr.conn.sent[1] {
		t.Fatalf("not resent: %v", tr.conn.sent)
	}

	// error が返ってこなければ届いたとみなし、送り直さない
	tr.tick(ackWindow)
	tr.tick(2 * time.Second)
	if len(tr.conn.sent) != 2 {
		t.Fatalf("resent without error: %v", tr.conn.sent)
	}
}

func TestReporterGivesUpAfterRetries(t *testing.T) {
	tr := newTestReporter()
	tr.conn.id = 1
	tr.src.sessions = []Session{}
	tr.tick(time.Second)
	for range maxRetries + 2 {
		tr.conn.errors++
		tr.tick(2 * time.Second)
		tr.tick(2 * time.Second)
	}
	if got := len(tr.conn.sent); got != 1+maxRetries {
		t.Errorf("sent %d times, want %d", got, 1+maxRetries)
	}
	if n := strings.Count(tr.logs.String(), "rejected a login message repeatedly"); n != 1 {
		t.Errorf("logged %d times:\n%s", n, tr.logs.String())
	}
}

func TestReporterResendsAfterReconnect(t *testing.T) {
	tr := newTestReporter()
	tr.conn.id = 1
	tr.src.sessions = []Session{}
	tr.src.backlog = []Event{login("lab", 1)}
	// 1 件送ったら、error が返ってこないかを待ってから次を送る
	tr.tick(time.Second)
	tr.tick(ackWindow)
	if len(tr.conn.sent) != 2 {
		t.Fatalf("sent = %v", tr.conn.sent)
	}

	// 送る前に切れた分は、接続し直したら直近 24 時間分として送り直す
	tr.src.newEvents = []Event{login("alice", 2)}
	tr.conn.fail = true
	tr.tick(ackWindow)
	tr.tick(ackWindow)
	tr.conn.fail = false
	tr.conn.id = 0
	tr.tick(ackWindow)

	tr.conn.id = 2
	tr.src.backlog = []Event{login("lab", 1), login("alice", 2)}
	tr.conn.sent = nil
	tr.tick(ackWindow)
	tr.tick(ackWindow)
	if got := strings.Join(tr.types(), ","); got != "sessions,login_events" {
		t.Fatalf("sent after reconnect = %v", tr.conn.sent)
	}
	if !strings.Contains(tr.conn.sent[1], `"alice"`) || !strings.Contains(tr.conn.sent[1], `"lab"`) {
		t.Errorf("backlog = %s", tr.conn.sent[1])
	}
}

func TestReporterReadErrorIsLoggedOnce(t *testing.T) {
	tr := newTestReporter()
	tr.conn.id = 1
	tr.src.err = errors.New("permission denied")
	for range 5 {
		tr.tick(2 * time.Second)
	}
	// utmp と wtmp でそれぞれ 1 回だけ
	if n := strings.Count(tr.logs.String(), "failed to read login records"); n != 2 {
		t.Errorf("logged %d times", n)
	}
	if len(tr.conn.sent) != 0 {
		t.Errorf("sent = %v", tr.conn.sent)
	}
}

func TestTakeEventsFitsMessageSize(t *testing.T) {
	big := strings.Repeat("h", maxHostLength)
	var events []Event
	for range MaxEventsPerMessage {
		events = append(events, Event{User: strings.Repeat("u", maxUserLength), TTY: "pts/0", Host: big, At: 1})
	}
	msg, rest := takeEvents(events)
	if encodedSize(msg) > maxMessageBytes {
		t.Fatalf("message is %d bytes", encodedSize(msg))
	}
	if len(msg.Events)+len(rest) != MaxEventsPerMessage || len(rest) == 0 {
		t.Errorf("split = %d + %d", len(msg.Events), len(rest))
	}
}

func TestFitSessions(t *testing.T) {
	big := strings.Repeat("h", maxHostLength)
	msg := &sessionsMessage{V: 1, Type: "sessions"}
	for i := range MaxSessions {
		msg.Sessions = append(msg.Sessions, Session{User: strings.Repeat("u", maxUserLength), TTY: "pts/0", Host: &big, LoginAt: int64(i + 1)})
	}
	fitSessions(msg)
	if encodedSize(msg) > maxMessageBytes || len(msg.Sessions) == 0 {
		t.Fatalf("size = %d, sessions = %d", encodedSize(msg), len(msg.Sessions))
	}
	// 新しいログインを残す
	if last := msg.Sessions[len(msg.Sessions)-1].LoginAt; last != MaxSessions {
		t.Errorf("last loginAt = %d", last)
	}
}

// utmp は読めて wtmp だけ読めない場合でも、wtmp のエラーを毎回ログに出さない（utmp の成功で回復扱いにしない）。
func TestReporterReadErrorPerFile(t *testing.T) {
	tr := newTestReporter()
	tr.conn.id = 1
	tr.src.sessions = []Session{}
	tr.src.wtmpErr = errors.New("permission denied")
	for range 10 {
		tr.tick(2 * time.Second)
	}
	logs := tr.logs.String()
	if n := strings.Count(logs, "failed to read login records"); n != 1 {
		t.Errorf("warned %d times:\n%s", n, logs)
	}
	if strings.Contains(logs, "recovered") {
		t.Errorf("logged a false recovery:\n%s", logs)
	}
	// utmp は読めているので sessions は送る
	if got := strings.Join(tr.types(), ","); got != "sessions" {
		t.Errorf("sent = %v", tr.types())
	}

	// wtmp が読めるようになったら、wtmp の回復だけをログに出す
	tr.src.wtmpErr = nil
	tr.tick(2 * time.Second)
	if n := strings.Count(tr.logs.String(), `msg="reading login records recovered" file=wtmp`); n != 1 {
		t.Errorf("recovery logged %d times:\n%s", n, tr.logs.String())
	}
}
