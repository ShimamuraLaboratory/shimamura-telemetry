package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/collector"
	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/version"
)

// 監視 Worker との取り決め（shimamura-IdP の pkg/schema/src/serverMonitor/messages.ts と
// app/server-monitor/src/internal.ts）。
const (
	// ProtocolVersion はメッセージ形式のバージョン（SERVER_MONITOR_PROTOCOL_VERSION）。
	ProtocolVersion = 1
	// MaxMessageBytes は 1 メッセージの上限（AGENT_MESSAGE_MAX_BYTES）。
	MaxMessageBytes = 64 * 1024

	// closeReplaced は同じサーバーに新しいエージェントが接続したため、古い接続が切られたことを表す。
	closeReplaced websocket.StatusCode = 4000
	// closeRevoked はトークンの再発行・サーバーの削除で接続が切られたことを表す。
	closeRevoked websocket.StatusCode = 4001
	// closeInvalidMessage はメッセージが不正なため接続が切られたことを表す。
	closeInvalidMessage websocket.StatusCode = 4002
)

// 再接続の間隔。
const (
	// backoffBase・backoffMax は指数バックオフの初期値と上限。
	backoffBase = time.Second
	backoffMax  = time.Minute
	// stableAfter はこの時間以上つながっていた接続が切れた場合に、バックオフを初期値に戻す。
	// 接続直後に切られることを繰り返す場合は、間隔を伸ばし続ける。
	stableAfter = time.Minute
	// authRetryDelay は認証に失敗した・トークンが失効した場合の再接続の間隔。
	// 設定ファイルを直すまで成功しないため長めにとる。
	authRetryDelay = 5 * time.Minute
	// replacedRetryDelay は別の接続に置き換えられた場合の再接続の間隔。
	// 同じトークンで 2 つのエージェントが動いていると互いに切り合うため、すぐには接続し直さない。
	replacedRetryDelay = time.Minute

	dialTimeout  = 10 * time.Second
	writeTimeout = 10 * time.Second
	closeTimeout = 5 * time.Second
)

// metricsMessage は AgentMetricsMessageSchema に対応する。
type metricsMessage struct {
	V    int    `json:"v"`
	Type string `json:"type"`
	// MeasuredAt は計測した時刻（UNIX ミリ秒）。
	MeasuredAt int64             `json:"measuredAt"`
	Metrics    collector.Metrics `json:"metrics"`
}

// serverMessage は監視 Worker からエージェントへ送られるメッセージ。現在は type: "error" だけ。
type serverMessage struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// WebSocket は監視 Worker へ外向きの WebSocket でスナップショットを送る Sender。
//
// 接続は Send の中で必要になったときに張る。切断・接続の失敗後は指数バックオフ（ジッター付き）で待ち、
// 待っている間に届いたスナップショットは送らずに捨てる（ErrNotConnected を返す）。
// メトリクスはリアルタイム表示のためのもので、再接続後に古い値をまとめて送っても、
// 監視 Worker は 2.5 秒より短い間隔のものを捨てるため意味がない。
type WebSocket struct {
	url    string
	token  string
	logger *slog.Logger
	now    func() time.Time
	// jitter は 0 以上 1 未満の乱数を返す。テストで固定するために分けている。
	jitter func() float64

	mu         sync.Mutex
	conn       *connection
	attempts   int
	nextDialAt time.Time
	// authFailed は前回の接続が認証の失敗で終わったか。同じログを繰り返し出さないために使う。
	authFailed bool
	closed     bool
}

var _ Sender = (*WebSocket)(nil)

// connection は 1 本の WebSocket 接続。
type connection struct {
	ws          *websocket.Conn
	connectedAt time.Time
	// done は読み込みが終わった（接続が切れた）ときに閉じる。err はその理由、closedAt はその時刻。
	// 再接続までの待ち時間は、次の Send で気付いた時刻ではなく closedAt から数える。
	done     chan struct{}
	err      error
	closedAt time.Time
}

// NewWebSocket は WebSocket を作る。endpoint は監視 Worker のエージェント用エンドポイント
// （例: wss://idp-monitor.example.com/agent/connect）。logger が nil のときは slog.Default() を使う。
func NewWebSocket(endpoint, token string, logger *slog.Logger) *WebSocket {
	if logger == nil {
		logger = slog.Default()
	}
	if u, err := url.Parse(endpoint); err == nil && u.Scheme == "ws" && !isLoopback(u.Hostname()) {
		logger.Warn("connecting without TLS; the agent token is sent in plain text", "url", endpoint)
	}
	return &WebSocket{
		url:    endpoint,
		token:  token,
		logger: logger,
		now:    time.Now,
		jitter: rand.Float64,
	}
}

// Send はスナップショットを 1 件送る。接続していなければ接続してから送る。
// 再接続を待っている間は ErrNotConnected を返す。
func (w *WebSocket) Send(ctx context.Context, snapshot *collector.Snapshot) error {
	data, err := json.Marshal(metricsMessage{
		V:          ProtocolVersion,
		Type:       "metrics",
		MeasuredAt: snapshot.CollectedAt.UnixMilli(),
		Metrics:    snapshot.Metrics,
	})
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}
	if len(data) > MaxMessageBytes {
		return fmt.Errorf("message too large: %d bytes (max %d)", len(data), MaxMessageBytes)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("sender is closed")
	}

	if w.conn != nil {
		select {
		case <-w.conn.done:
			w.disconnected(w.conn.err, w.conn.closedAt)
		default:
		}
	}
	if w.conn == nil {
		if err := w.connect(ctx); err != nil {
			return err
		}
	}

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := w.conn.ws.Write(writeCtx, websocket.MessageText, data); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		w.disconnected(fmt.Errorf("write: %w", err), w.now())
		return fmt.Errorf("%w: %w", ErrNotConnected, err)
	}
	return nil
}

// Close は接続を閉じる。閉じた後の Send はエラーを返す。
func (w *WebSocket) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.conn == nil {
		return nil
	}
	c := w.conn
	w.conn = nil
	err := c.ws.Close(websocket.StatusNormalClosure, "agent shutting down")
	select {
	case <-c.done:
	case <-time.After(closeTimeout):
	}
	if err != nil && websocket.CloseStatus(err) == -1 && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// connect は監視 Worker へ接続する。待ち時間中・失敗した場合は ErrNotConnected を返す。
func (w *WebSocket) connect(ctx context.Context) error {
	if w.now().Before(w.nextDialAt) {
		return ErrNotConnected
	}

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	ws, resp, err := websocket.Dial(dialCtx, w.url, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": {"Bearer " + w.token},
			"User-Agent":    {"shimamura-telemetry/" + version.Version},
		},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			delay := w.retryAfter(authRetryDelay)
			if !w.authFailed {
				w.logger.Error("authentication failed; check agent_token in the config file",
					"status", resp.StatusCode, "retry_in", delay)
			}
			w.authFailed = true
			return fmt.Errorf("%w: authentication failed", ErrNotConnected)
		}
		delay := w.backoff()
		attrs := []any{"error", err, "retry_in", delay}
		if resp != nil {
			attrs = append(attrs, "status", resp.StatusCode)
		}
		w.logger.Warn("failed to connect to the monitor", attrs...)
		return fmt.Errorf("%w: %w", ErrNotConnected, err)
	}

	w.authFailed = false
	c := &connection{ws: ws, connectedAt: w.now(), done: make(chan struct{})}
	w.conn = c
	go w.readLoop(c)
	w.logger.Info("connected to the monitor", "url", w.url)
	return nil
}

// readLoop は監視 Worker からのメッセージを読む。コントロールフレーム（ping・close）の処理にも読み込みが必要。
func (w *WebSocket) readLoop(c *connection) {
	defer close(c.done)
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.err = err
			c.closedAt = w.now()
			return
		}
		var msg serverMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			w.logger.Debug("ignored a message from the monitor", "error", err)
			continue
		}
		if msg.Type == "error" {
			w.logger.Warn("the monitor rejected a message", "message", msg.Message)
		}
	}
}

// disconnected は接続が切れたときの後片付けをし、次に接続するまでの待ち時間を決める。
// closedAt は接続が切れた時刻で、待ち時間はここから数える。
func (w *WebSocket) disconnected(cause error, closedAt time.Time) {
	c := w.conn
	w.conn = nil
	_ = c.ws.CloseNow()

	if closedAt.Sub(c.connectedAt) >= stableAfter {
		w.attempts = 0
	}
	switch status := websocket.CloseStatus(cause); status {
	case closeRevoked:
		delay := w.retryAfterFrom(closedAt, authRetryDelay)
		w.logger.Error("the agent token was revoked or the server was deleted; update agent_token in the config file",
			"retry_in", delay)
	case closeReplaced:
		delay := w.retryAfterFrom(closedAt, replacedRetryDelay)
		w.logger.Warn("disconnected because another agent connected for this server; is another agent running with the same token?",
			"retry_in", delay)
	case closeInvalidMessage:
		delay := w.backoffFrom(closedAt)
		w.logger.Warn("disconnected because the monitor rejected a message", "retry_in", delay)
	default:
		delay := w.backoffFrom(closedAt)
		attrs := []any{"retry_in", delay}
		if status != -1 {
			attrs = append(attrs, "code", int(status))
		} else {
			attrs = append(attrs, "error", cause)
		}
		w.logger.Warn("disconnected from the monitor", attrs...)
	}
}

// backoff は今から次の接続までの待ち時間を指数バックオフで決め、nextDialAt に設定する。
func (w *WebSocket) backoff() time.Duration {
	return w.backoffFrom(w.now())
}

// backoffFrom は from から次の接続までの待ち時間を指数バックオフで決め、nextDialAt に設定する。
// 待ち時間は base·2^attempts（上限 backoffMax）の半分から全体までの間でばらつかせる。
// 再デプロイ後に全サーバーのエージェントが同時に接続し直すのを避けるため。
func (w *WebSocket) backoffFrom(from time.Time) time.Duration {
	d := backoffMax
	if w.attempts < 16 {
		d = min(backoffBase<<w.attempts, backoffMax)
	}
	w.attempts++
	return w.retryAfterFrom(from, d)
}

// retryAfter は今から d の半分〜d の間で待ち時間を決め、nextDialAt に設定する。
func (w *WebSocket) retryAfter(d time.Duration) time.Duration {
	return w.retryAfterFrom(w.now(), d)
}

// retryAfterFrom は from から d の半分〜d の間で待ち時間を決め、nextDialAt に設定する。
func (w *WebSocket) retryAfterFrom(from time.Time, d time.Duration) time.Duration {
	delay := d/2 + time.Duration(w.jitter()*float64(d/2))
	w.nextDialAt = from.Add(delay)
	return delay.Round(time.Millisecond)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
