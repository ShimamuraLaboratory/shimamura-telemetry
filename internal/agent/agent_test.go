package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/collector"
	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/transport"
)

type fakeCollector struct {
	err error
}

func (f *fakeCollector) Collect(context.Context) (*collector.Snapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &collector.Snapshot{CollectedAt: time.Now()}, nil
}

type fakeSender struct {
	mu     sync.Mutex
	sent   int
	closed bool
	cancel context.CancelFunc
	stopAt int
}

func (f *fakeSender) Send(context.Context, *collector.Snapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent++
	if f.sent >= f.stopAt {
		f.cancel()
	}
	return nil
}

func (f *fakeSender) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRunSendsUntilCanceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s := &fakeSender{cancel: cancel, stopAt: 3}
	a := New(&fakeCollector{}, s, 10*time.Millisecond, discardLogger())
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("Run stopped before sending 3 snapshots (ctx err: %v)", ctx.Err())
	}
	if s.sent != 3 {
		t.Errorf("sent = %d, want 3", s.sent)
	}
	if !s.closed {
		t.Error("sender was not closed")
	}
}

func TestRunContinuesOnCollectError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	s := &fakeSender{cancel: cancel, stopAt: 1}
	a := New(&fakeCollector{err: errors.New("boom")}, s, 10*time.Millisecond, discardLogger())
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if s.sent != 0 {
		t.Errorf("sent = %d, want 0", s.sent)
	}
	if !s.closed {
		t.Error("sender was not closed")
	}
}

// errSender は常に同じエラーを返す Sender。
type errSender struct {
	err    error
	cancel context.CancelFunc
	sent   int
}

func (e *errSender) Send(context.Context, *collector.Snapshot) error {
	e.sent++
	if e.sent >= 2 {
		e.cancel()
	}
	return e.err
}

func (e *errSender) Close() error { return nil }

// 再接続を待っている間（ErrNotConnected）は、送信のたびにエラーログを出さない。
func TestRunLogsSendErrors(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantLog bool
	}{
		{"not connected", fmt.Errorf("%w: dial failed", transport.ErrNotConnected), false},
		{"other error", errors.New("message too large"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var logs bytes.Buffer
			s := &errSender{err: tt.err, cancel: cancel}
			a := New(&fakeCollector{}, s, 10*time.Millisecond, slog.New(slog.NewTextHandler(&logs, nil)))
			if err := a.Run(ctx); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(logs.String(), "failed to send"); got != tt.wantLog {
				t.Errorf("logged = %v, want %v:\n%s", got, tt.wantLog, logs.String())
			}
		})
	}
}
