// Package transport は収集したリソース情報を監視 Worker へ送る処理を提供する。
package transport

import (
	"context"
	"errors"

	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/collector"
)

// ErrNotConnected は監視 Worker に接続できていないため、スナップショットを送らずに捨てたことを表す。
// 接続・切断の理由は Sender 側でログに出しているため、呼び出し側では詳しく出さなくてよい。
var ErrNotConnected = errors.New("not connected to the monitor")

// Sender はスナップショットを監視 Worker へ送る。
type Sender interface {
	// Send はスナップショットを 1 件送る。
	// 接続が切れている場合の再接続は実装側の責務とする。
	Send(ctx context.Context, snapshot *collector.Snapshot) error
	// Close は接続を閉じる。
	Close() error
}
