package logins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

const (
	// protocolVersion はメッセージ形式のバージョン（SERVER_MONITOR_PROTOCOL_VERSION）。
	protocolVersion = 1
	// maxMessageBytes は 1 メッセージの上限（AGENT_MESSAGE_MAX_BYTES）。
	maxMessageBytes = 64 * 1024

	// pollInterval は utmp・wtmp を見る間隔。
	pollInterval = time.Second
	// minSendGap はログイン関連のメッセージの間隔。監視 Worker は 1 秒（AGENT_LOGIN_MESSAGE_MIN_INTERVAL_MS）より
	// 短い間隔のものに error を返すため、余裕を持たせる。
	minSendGap = 1500 * time.Millisecond
	// sessionsRefresh は変化が無くても sessions を送り直す間隔。
	sessionsRefresh = 5 * time.Minute
	// backlogWindow は接続直後に送り直すイベントの期間。
	backlogWindow = 24 * time.Hour
	// ackWindow は送ったメッセージに error が返ってこないかを待つ時間。
	// 監視 Worker は受け付けたメッセージに応答を返さないため、この間に error が来なければ届いたとみなす。
	ackWindow = 3 * time.Second
	// maxRetries は error が返ってきたメッセージを送り直す回数。
	maxRetries = 3
)

// Conn は監視 Worker との接続。transport.WebSocket が実装する。
type Conn interface {
	// ConnectionID は今の接続の番号を返す。接続していなければ 0。接続し直すたびに変わる。
	ConnectionID() uint64
	// ErrorCount は監視 Worker から受け取った error メッセージの数を返す。
	ErrorCount() uint64
	// SendIfConnected は接続している場合だけ v を JSON で送る。接続していなければ transport.ErrNotConnected を返す。
	// 接続はしない（メトリクスの送信が接続・再接続を担う）。
	SendIfConnected(ctx context.Context, v any) error
}

// source は utmp・wtmp を読む処理。テストで差し替えるために分けている。
type source interface {
	Sessions() ([]Session, error)
	Backlog(since time.Time) ([]Event, error)
	NewEvents() ([]Event, error)
}

type sessionsMessage struct {
	V           int       `json:"v"`
	Type        string    `json:"type"`
	CollectedAt int64     `json:"collectedAt"`
	Sessions    []Session `json:"sessions"`
}

type eventsMessage struct {
	V      int     `json:"v"`
	Type   string  `json:"type"`
	Events []Event `json:"events"`
}

// outgoing は送るメッセージ 1 件。
type outgoing struct {
	msg any
	// events は login_events の場合のイベント。送れなかったときに戻すために持つ。
	events []Event
	// retries は error が返ってきて送り直した回数。
	retries int
}

// inflight は送ったばかりで、error が返ってこないかを待っているメッセージ。
type inflight struct {
	out        outgoing
	sentAt     time.Time
	errorCount uint64
}

// Reporter は utmp・wtmp を定期的に読み、ログイン関連のメッセージを監視 Worker へ送る。
//
//   - 接続し直したら（接続直後）、現在のセッションと直近 24 時間のイベントを送り直す
//   - セッションは変化があったとき、変化が無くても 5 分ごとに送る（監視 Worker は受け取った一覧で置き換える）
//   - 新しいイベントは 300 件ずつに分けて送る
//   - メッセージの間隔は 1.5 秒以上あける。error が返ってきたら送り直す
type Reporter struct {
	conn   Conn
	source source
	logger *slog.Logger
	now    func() time.Time

	connID uint64
	// backlogPending は接続し直した後、直近 24 時間のイベントをまだ読めていないか。
	// 読めるまで（wtmp が一時的に読めない場合など）は続きからの読み取りをせず、読み直しを続ける。
	backlogPending bool
	// lastPickedSessions は前回セッションを選んだか。セッションとイベントの両方がある場合に交互に送るために使う。
	// 接続し直したらまずセッションを送るよう、reset で false に戻す。
	lastPickedSessions bool
	// pendingSessions は次に送るセッションの一覧（新しいものだけ残す）。
	pendingSessions *sessionsMessage
	// pendingEvents は次に送るイベント。
	pendingEvents []Event
	// retry は error が返ってきて送り直すメッセージ。
	retry *outgoing
	// inflight は error が返ってこないかを待っているメッセージ。
	inflight *inflight

	lastSessions   []Session
	lastSessionsAt time.Time
	lastSentAt     time.Time
	// readFailed はファイル（"utmp"・"wtmp"）ごとに、前回の読み取りが失敗したか。
	// 片方だけが読めない場合に、もう片方の成功で「回復した」と誤ってログに出さないよう分けて持つ。
	readFailed         map[string]bool
	retriesExhaustedAt time.Time
}

// NewReporter は Reporter を作る。logger が nil のときは slog.Default() を使う。
func NewReporter(conn Conn, src *Source, logger *slog.Logger) *Reporter {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reporter{conn: conn, source: src, logger: logger, now: time.Now}
}

// Run は ctx がキャンセルされるまで、1 秒ごとに utmp・wtmp を見て送る。
func (r *Reporter) Run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		r.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// step は 1 回分の処理。
func (r *Reporter) step(ctx context.Context) {
	id := r.conn.ConnectionID()
	if id == 0 {
		// 接続していない間は読まない。接続し直したら直近 24 時間分を送り直すため取りこぼさない
		return
	}
	now := r.now()
	if id != r.connID {
		r.connID = id
		r.reset(now)
	} else {
		r.poll(now)
	}
	r.checkInflight(now)
	r.sendNext(ctx, now)
}

// reset は接続し直したときに、送るものを今のセッションと直近 24 時間のイベントにする。
func (r *Reporter) reset(now time.Time) {
	r.retry, r.inflight, r.pendingEvents, r.pendingSessions = nil, nil, nil, nil
	r.lastPickedSessions = false
	r.lastSessions, r.lastSessionsAt = nil, time.Time{}
	r.pollSessions(now, true)
	r.backlogPending = true
	r.loadBacklog(now)
}

// loadBacklog は直近 24 時間のイベントを読み、送るものにする。失敗したら次の poll で読み直す。
func (r *Reporter) loadBacklog(now time.Time) {
	events, err := r.source.Backlog(now.Add(-backlogWindow))
	if r.logReadError("wtmp", err) {
		return
	}
	r.backlogPending = false
	r.pendingEvents = events
}

// poll は変化したセッションと新しいイベントを、送るものに加える。
func (r *Reporter) poll(now time.Time) {
	r.pollSessions(now, false)
	if r.backlogPending {
		// 続きから読むと、直近 24 時間に絞らずに wtmp 全体を読んでしまうため、読めるまで Backlog を読み直す
		r.loadBacklog(now)
		return
	}
	events, err := r.source.NewEvents()
	if r.logReadError("wtmp", err) {
		return
	}
	r.pendingEvents = append(r.pendingEvents, events...)
}

func (r *Reporter) pollSessions(now time.Time, force bool) {
	sessions, err := r.source.Sessions()
	if r.logReadError("utmp", err) {
		return
	}
	changed := !slices.EqualFunc(sessions, r.lastSessions, sessionEqual)
	if !force && !changed && now.Sub(r.lastSessionsAt) < sessionsRefresh {
		return
	}
	r.lastSessions, r.lastSessionsAt = sessions, now
	r.pendingSessions = &sessionsMessage{
		V:           protocolVersion,
		Type:        "sessions",
		CollectedAt: now.UnixMilli(),
		Sessions:    sessions,
	}
}

func sessionEqual(a, b Session) bool {
	return a.User == b.User && a.TTY == b.TTY && a.LoginAt == b.LoginAt &&
		(a.Host == nil) == (b.Host == nil) && (a.Host == nil || *a.Host == *b.Host)
}

// logReadError は読み取りのエラーをログに出し、エラーなら true を返す。
// ファイルごとに、失敗し始めたとき・回復したときだけログに出す。
func (r *Reporter) logReadError(what string, err error) bool {
	if r.readFailed == nil {
		r.readFailed = map[string]bool{}
	}
	if err == nil {
		if r.readFailed[what] {
			r.logger.Info("reading login records recovered", "file", what)
		}
		r.readFailed[what] = false
		return false
	}
	if !r.readFailed[what] {
		r.logger.Warn("failed to read login records", "file", what, "error", err)
	}
	r.readFailed[what] = true
	return true
}

// checkInflight は送ったメッセージに error が返ってきたら送り直しに回す。待つ時間を過ぎたら届いたとみなす。
func (r *Reporter) checkInflight(now time.Time) {
	if r.inflight == nil {
		return
	}
	if r.conn.ErrorCount() > r.inflight.errorCount {
		out := r.inflight.out
		r.inflight = nil
		if out.retries >= maxRetries {
			// 監視 Worker が対応していない（古い）場合などに、5 秒ごとに同じログが出ないようにする
			if now.Sub(r.retriesExhaustedAt) >= sessionsRefresh {
				r.logger.Warn("the monitor rejected a login message repeatedly; dropped it")
			}
			r.retriesExhaustedAt = now
			return
		}
		out.retries++
		r.retry = &out
		return
	}
	if now.Sub(r.inflight.sentAt) >= ackWindow {
		r.inflight = nil
	}
}

// sendNext は間隔があいていれば、次のメッセージを 1 件送る。
// 送り直し → セッション → イベントの順。error を待っている間は次を送らない（どれへの error か分からなくなるため）。
func (r *Reporter) sendNext(ctx context.Context, now time.Time) {
	if r.inflight != nil || now.Sub(r.lastSentAt) < minSendGap {
		return
	}
	out, ok := r.next()
	if !ok {
		return
	}
	errCount := r.conn.ErrorCount()
	if err := r.conn.SendIfConnected(ctx, out.msg); err != nil {
		// 送れなかったものは戻す。接続し直した場合は reset で作り直す
		r.retry = &out
		if !errors.Is(err, context.Canceled) {
			r.logger.Debug("failed to send a login message", "error", err)
		}
		return
	}
	r.lastSentAt = now
	r.inflight = &inflight{out: out, sentAt: now, errorCount: errCount}
}

// next は次に送るメッセージを取り出す。送り直しを先に送り、セッションとイベントの両方がある場合は交互に送る
// （セッションが頻繁に変わるサーバーで、イベントがいつまでも送られないことが無いように）。
func (r *Reporter) next() (outgoing, bool) {
	if r.retry != nil {
		out := *r.retry
		r.retry = nil
		return out, true
	}
	hasEvents := len(r.pendingEvents) > 0
	if r.pendingSessions != nil && (!hasEvents || !r.lastPickedSessions) {
		msg := r.pendingSessions
		r.pendingSessions = nil
		r.lastPickedSessions = true
		fitSessions(msg)
		return outgoing{msg: msg}, true
	}
	if hasEvents {
		msg, rest := takeEvents(r.pendingEvents)
		r.pendingEvents = rest
		r.lastPickedSessions = false
		if msg == nil {
			return outgoing{}, false
		}
		return outgoing{msg: msg, events: msg.Events}, true
	}
	return outgoing{}, false
}

// fitSessions はメッセージが 64 KB に収まるまで、ログイン時刻の古いセッションから減らす。
func fitSessions(msg *sessionsMessage) {
	for len(msg.Sessions) > 0 && encodedSize(msg) > maxMessageBytes {
		msg.Sessions = msg.Sessions[1:]
	}
}

// takeEvents は先頭から 300 件まで（64 KB に収まるだけ）のイベントを取り出す。
func takeEvents(events []Event) (*eventsMessage, []Event) {
	n := min(len(events), MaxEventsPerMessage)
	for n > 0 {
		msg := &eventsMessage{V: protocolVersion, Type: "login_events", Events: events[:n]}
		if encodedSize(msg) <= maxMessageBytes {
			return msg, events[n:]
		}
		n /= 2
	}
	// 1 件でも収まらない（あり得ないが）ものは捨てる
	return nil, events[1:]
}

func encodedSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return maxMessageBytes + 1
	}
	return len(b)
}

// Snapshot は今のセッションと直近 24 時間のイベントを、送るメッセージの形で返す（動作確認用）。
func Snapshot(src *Source, now time.Time) (any, error) {
	sessions, err := src.Sessions()
	if err != nil {
		return nil, fmt.Errorf("read utmp: %w", err)
	}
	events, err := src.Backlog(now.Add(-backlogWindow))
	if err != nil {
		return nil, fmt.Errorf("read wtmp: %w", err)
	}
	if events == nil {
		events = []Event{}
	}
	return []any{
		sessionsMessage{V: protocolVersion, Type: "sessions", CollectedAt: now.UnixMilli(), Sessions: sessions},
		eventsMessage{V: protocolVersion, Type: "login_events", Events: events},
	}, nil
}
