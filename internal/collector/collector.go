// Package collector は計算機サーバーのリソース情報を収集する処理のインターフェースを定義する。
//
// 実装（gopsutil による CPU・メモリ・SSD・プロセス、go-nvml による GPU）は後続の PR で追加する。
package collector

import (
	"context"
	"time"
)

// Snapshot はある時点で収集したリソース情報。
//
// TODO: フィールドは shimamura-IdP の pkg/schema で定義されるメッセージ形式（Issue #526）が
// 確定してから合わせる。それまでは時刻だけを持つ仮の型とする。
type Snapshot struct {
	// CollectedAt は収集した時刻。
	CollectedAt time.Time
}

// Collector はリソース情報を収集する。
type Collector interface {
	// Collect は現在のリソース情報を収集して返す。
	// ctx がキャンセルされたら速やかに中断すること。
	Collect(ctx context.Context) (*Snapshot, error)
}
