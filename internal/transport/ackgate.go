package transport

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrBusy は、別の送り手のメッセージが error を待っている間なので送らなかったことを表す。少し待って送り直せばよい。
var ErrBusy = errors.New("another message is awaiting acknowledgement")

// waiterTTL は、ErrBusy で待たされた送り手を「順番待ち」として扱う時間。送り手は待たされると後で送り直すので、
// その間は順番待ちを続ける。送り直さなくなった（接続が切れた・送るものが無くなった）送り手が、他を待たせ続けないようにする。
const waiterTTL = 15 * time.Second

// conn は AckGate が使う接続（WebSocket が実装する）。
type conn interface {
	ConnectionID() uint64
	ErrorCount() uint64
	SendIfConnected(ctx context.Context, v any) error
}

// AckGate は、メトリクス以外のメッセージ（ハードウェア情報・ログイン履歴）を送る複数の送り手が、
// 監視 Worker から返ってくる error を取り違えないようにする。
//
// 監視 Worker は受け付けたメッセージに応答せず、拒否したときだけ（どのメッセージへのものかを示さずに）error を返す。
// 送り手は「送ってからしばらくの間に ErrorCount が増えたら自分のメッセージが拒否された」とみなすため、
// 別の送り手のメッセージがその間にある間は送らせない（ErrBusy を返す）。
//
// 待たされた送り手は順番待ちになり、今の送り手は続けて送れなくなる。今の送り手がメッセージを送り続けて
// 待たせる時間を延ばし続け、他の送り手がいつまでも送れない、ということが無いようにする。
type AckGate struct {
	conn conn
	now  func() time.Time

	mu         sync.Mutex
	clients    int
	lastClient int
	lastSentAt time.Time
	lastHold   time.Duration
	// waiter は順番待ちの送り手（0 なら無し）、waitedAt は最後に待たされた時刻。
	waiter   int
	waitedAt time.Time
}

// NewAckGate は AckGate を作る。
func NewAckGate(c conn) *AckGate {
	return &AckGate{conn: c, now: time.Now}
}

// Client は送り手 1 つ分の接続を返す。送り手ごとに 1 回ずつ呼ぶ。
// hold は、この送り手が送った後に他の送り手を待たせる時間で、送り手が error を待つ時間に、
// error に気付くまでの間隔（ポーリングの間隔）を足したもの以上にする。
func (g *AckGate) Client(hold time.Duration) *GatedConn {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.clients++
	return &GatedConn{gate: g, id: g.clients, hold: hold}
}

// GatedConn は AckGate を通して送る接続。logins.Conn・spec.Conn を実装する。
type GatedConn struct {
	gate *AckGate
	id   int
	hold time.Duration
}

// ConnectionID は今の接続の番号を返す。
func (c *GatedConn) ConnectionID() uint64 { return c.gate.conn.ConnectionID() }

// ErrorCount は監視 Worker から受け取った error メッセージの数を返す。
func (c *GatedConn) ErrorCount() uint64 { return c.gate.conn.ErrorCount() }

// SendIfConnected は、別の送り手のメッセージが error を待っておらず、別の送り手が順番待ちでもなければ v を送る。
// 送れなければ ErrBusy を返す。
func (c *GatedConn) SendIfConnected(ctx context.Context, v any) error {
	g := c.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if g.waiter != 0 && now.Sub(g.waitedAt) > waiterTTL {
		g.waiter = 0
	}
	busy := g.lastClient != 0 && g.lastClient != c.id && now.Sub(g.lastSentAt) < g.lastHold
	yield := g.waiter != 0 && g.waiter != c.id
	if busy || yield {
		if busy && (g.waiter == 0 || g.waiter == c.id) {
			g.waiter, g.waitedAt = c.id, now
		}
		return ErrBusy
	}
	if err := g.conn.SendIfConnected(ctx, v); err != nil {
		return err
	}
	g.lastClient, g.lastSentAt, g.lastHold = c.id, now, c.hold
	if g.waiter == c.id {
		g.waiter = 0
	}
	return nil
}
