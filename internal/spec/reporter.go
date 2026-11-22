// Package spec はサーバーのハードウェア情報（CPU・メモリ・SSD・GPU）を、接続したときと構成が変わったときに
// 監視 Worker へ送る（spec メッセージ）。IdP の管理画面で手入力していたハードウェア情報の代わりになる。
package spec

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/collector"
)

const (
	// protocolVersion はメッセージ形式のバージョン（SERVER_MONITOR_PROTOCOL_VERSION）。
	protocolVersion = 1
	// maxMessageBytes は 1 メッセージの上限（AGENT_MESSAGE_MAX_BYTES）。
	maxMessageBytes = 64 * 1024

	// pollInterval は接続し直したか・送り直す時刻かを見る間隔。
	pollInterval = 5 * time.Second
	// recheckInterval は接続中にハードウェア情報を取り直す間隔。前回送ったものと違えば送る。
	recheckInterval = time.Hour
	// minSendGap は同じ接続で spec を送る間隔の下限。監視 Worker は 1 分より短い間隔の spec に error を返すため、余裕を持たせる。
	minSendGap = 65 * time.Second
	// ackWindow は送ったメッセージに error が返ってこないかを待つ時間。
	// 監視 Worker は受け付けたメッセージに応答を返さないため、この間に error が来なければ届いたとみなす。
	ackWindow = 3 * time.Second
	// maxRetries は error が返ってきたメッセージを送り直す回数。使い切ったら次に取り直す（1 時間後か再接続）まで送らない。
	maxRetries = 3
)

// Conn は監視 Worker との接続。transport.WebSocket が実装する。
type Conn interface {
	// ConnectionID は今の接続の番号を返す。接続していなければ 0。接続し直すたびに変わる。
	ConnectionID() uint64
	// ErrorCount は監視 Worker から受け取った error メッセージの数を返す。
	ErrorCount() uint64
	// SendIfConnected は接続している場合だけ v を JSON で送る（接続はしない）。
	SendIfConnected(ctx context.Context, v any) error
}

// Collector はハードウェア情報を読む。collector.System が実装する。
type Collector interface {
	Spec(ctx context.Context) (collector.Spec, error)
}

// message は AgentSpecMessageSchema に対応する。
type message struct {
	V    int    `json:"v"`
	Type string `json:"type"`
	// CollectedAt は取得した時刻（UNIX ミリ秒）。
	CollectedAt int64          `json:"collectedAt"`
	Spec        collector.Spec `json:"spec"`
}

// Reporter はハードウェア情報を監視 Worker へ送る。
//
//   - 接続し直したら（接続直後）、取り直して送る
//   - 接続中は 1 時間ごとに取り直し、前回送ったものと違えば送る
//   - 同じ接続では 65 秒以上あけて送る。error が返ってきたら 65 秒後に 3 回まで送り直す
type Reporter struct {
	conn      Conn
	collector Collector
	logger    *slog.Logger
	now       func() time.Time

	connID uint64
	// pending は次に送るハードウェア情報。
	pending *collector.Spec
	retries int
	// lastSent は届いた（error が返ってこなかった）ハードウェア情報。構成が変わったかの判定に使う。
	lastSent *collector.Spec
	// lastSentAt は今の接続で最後に送った時刻。
	lastSentAt  time.Time
	collectedAt time.Time
	// inflight は送ったばかりで、error が返ってこないかを待っているもの。
	inflight *inflight
	gaveUp   bool
}

type inflight struct {
	spec       collector.Spec
	sentAt     time.Time
	errorCount uint64
}

// NewReporter は Reporter を作る。logger が nil のときは slog.Default() を使う。
func NewReporter(conn Conn, c Collector, logger *slog.Logger) *Reporter {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reporter{conn: conn, collector: c, logger: logger, now: time.Now}
}

// Run は ctx がキャンセルされるまで、5 秒ごとに接続の状態を見て送る。
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

func (r *Reporter) step(ctx context.Context) {
	id := r.conn.ConnectionID()
	if id == 0 {
		return
	}
	now := r.now()
	if id != r.connID {
		// 接続し直したら必ず送る（監視 Worker は接続ごとに受け付ける）
		r.connID = id
		r.inflight, r.lastSentAt, r.retries, r.gaveUp = nil, time.Time{}, 0, false
		r.collect(ctx, now, true)
	} else if now.Sub(r.collectedAt) >= recheckInterval {
		r.collect(ctx, now, false)
	}
	r.checkInflight(now)
	r.sendPending(ctx, now)
}

// collect はハードウェア情報を取り直す。force でなければ、前回送ったものと違う場合だけ送る。
func (r *Reporter) collect(ctx context.Context, now time.Time, force bool) {
	spec, err := r.collector.Spec(ctx)
	if err != nil {
		// ctx のキャンセル以外では Spec はエラーを返さない
		return
	}
	r.collectedAt = now
	if !force && r.lastSent != nil && sameSpec(spec, *r.lastSent) {
		return
	}
	if !force && r.lastSent != nil {
		r.logger.Info("hardware configuration changed; sending the spec")
	}
	r.pending = &spec
	r.retries, r.gaveUp = 0, false
}

// checkInflight は送ったものに error が返ってきたら送り直しに回す。待つ時間を過ぎたら届いたとみなす。
func (r *Reporter) checkInflight(now time.Time) {
	if r.inflight == nil {
		return
	}
	if r.conn.ErrorCount() > r.inflight.errorCount {
		spec := r.inflight.spec
		r.inflight = nil
		if r.retries >= maxRetries {
			if !r.gaveUp {
				r.logger.Warn("the monitor rejected the hardware spec repeatedly; will retry after reconnecting or in an hour")
			}
			r.gaveUp = true
			return
		}
		r.retries++
		// 新しく取り直したものが待っていれば、そちらを送る
		if r.pending == nil {
			r.pending = &spec
		}
		return
	}
	if now.Sub(r.inflight.sentAt) >= ackWindow {
		spec := r.inflight.spec
		r.lastSent = &spec
		r.inflight = nil
	}
}

// sendPending は送るものがあり、前回から 65 秒以上あいていれば送る。
func (r *Reporter) sendPending(ctx context.Context, now time.Time) {
	if r.pending == nil || r.inflight != nil || r.gaveUp {
		return
	}
	if !r.lastSentAt.IsZero() && now.Sub(r.lastSentAt) < minSendGap {
		return
	}
	spec := *r.pending
	msg := message{V: protocolVersion, Type: "spec", CollectedAt: r.collectedAt.UnixMilli(), Spec: fit(spec)}
	errCount := r.conn.ErrorCount()
	if err := r.conn.SendIfConnected(ctx, msg); err != nil {
		if !errors.Is(err, context.Canceled) {
			r.logger.Debug("failed to send the hardware spec", "error", err)
		}
		return
	}
	r.pending = nil
	r.lastSentAt = now
	r.inflight = &inflight{spec: spec, sentAt: now, errorCount: errCount}
}

// fit はメッセージが 64 KB に収まるよう、ディスクを減らす（実際には数十件で収まる）。
func fit(spec collector.Spec) collector.Spec {
	for len(spec.Disks) > 0 {
		b, err := json.Marshal(message{V: protocolVersion, Type: "spec", Spec: spec})
		if err == nil && len(b) <= maxMessageBytes {
			break
		}
		// 大きさに比例して減らす（1 件ずつ減らして毎回エンコードし直すと遅い）
		n := len(spec.Disks) * maxMessageBytes / max(len(b), 1)
		spec.Disks = spec.Disks[:min(n, len(spec.Disks)-1)]
	}
	return spec
}

// sameSpec は 2 つのハードウェア情報が同じかを返す。
func sameSpec(a, b collector.Spec) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}
