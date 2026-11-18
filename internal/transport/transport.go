// Package transport は収集したリソース情報を監視 Worker へ送る処理のインターフェースを定義する。
//
// 実装（監視 Worker への外向き WebSocket 接続、再接続、認証）は後続の PR で追加する。
package transport

import (
	"context"

	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/collector"
)

// Sender はスナップショットを監視 Worker へ送る。
type Sender interface {
	// Send はスナップショットを 1 件送る。
	// 接続が切れている場合の再接続は実装側の責務とする。
	Send(ctx context.Context, snapshot *collector.Snapshot) error
	// Close は接続を閉じる。
	Close() error
}
