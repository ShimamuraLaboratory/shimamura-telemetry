// Package agent は収集と送信を一定間隔で繰り返すエージェント本体のループを提供する。
package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/collector"
	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/transport"
)

// Agent は Collector で集めた情報を Sender で送る。
type Agent struct {
	collector collector.Collector
	sender    transport.Sender
	interval  time.Duration
	logger    *slog.Logger
}

// New は Agent を作る。
func New(c collector.Collector, s transport.Sender, interval time.Duration, logger *slog.Logger) *Agent {
	if logger == nil {
		logger = slog.Default()
	}
	return &Agent{collector: c, sender: s, interval: interval, logger: logger}
}

// Run は ctx がキャンセルされるまで interval ごとに収集・送信する。
// 1 回分の収集・送信の失敗はログに残して次の周期へ進む。
func (a *Agent) Run(ctx context.Context) error {
	defer func() {
		if err := a.sender.Close(); err != nil {
			a.logger.Warn("failed to close sender", "error", err)
		}
	}()

	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()

	for {
		a.tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (a *Agent) tick(ctx context.Context) {
	snapshot, err := a.collector.Collect(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			a.logger.Error("failed to collect", "error", err)
		}
		return
	}
	if err := a.sender.Send(ctx, snapshot); err != nil {
		switch {
		case errors.Is(err, context.Canceled):
		case errors.Is(err, transport.ErrNotConnected):
			// 接続・切断の理由は Sender がログに出している。再接続を待つ間は 5 秒ごとに出さない
			a.logger.Debug("skipped sending while disconnected", "error", err)
		default:
			a.logger.Error("failed to send", "error", err)
		}
	}
}
