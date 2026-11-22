package transport

import (
	"context"
	"errors"
	"testing"
	"time"
)

type gateFakeConn struct {
	sent []string
	err  error
}

func (c *gateFakeConn) ConnectionID() uint64 { return 1 }
func (c *gateFakeConn) ErrorCount() uint64   { return 7 }

func (c *gateFakeConn) SendIfConnected(_ context.Context, v any) error {
	if c.err != nil {
		return c.err
	}
	c.sent = append(c.sent, v.(string))
	return nil
}

func TestAckGate(t *testing.T) {
	fc := &gateFakeConn{}
	g := NewAckGate(fc)
	clock := time.Unix(0, 0)
	g.now = func() time.Time { return clock }
	logins := g.Client(4 * time.Second)
	spec := g.Client(8 * time.Second)

	if logins.ConnectionID() != 1 || spec.ErrorCount() != 7 {
		t.Fatal("ConnectionID / ErrorCount are not passed through")
	}

	// ログイン履歴を送った直後は、ハードウェア情報を送らせない（error を取り違えないため）
	if err := logins.SendIfConnected(t.Context(), "login_events"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(3 * time.Second)
	if err := spec.SendIfConnected(t.Context(), "spec"); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	// 同じ送り手は続けて送れる（自分の間隔は送り手が管理する）
	if err := logins.SendIfConnected(t.Context(), "sessions"); err != nil {
		t.Fatal(err)
	}

	// 送り手の hold（4 秒）を過ぎたら送れる
	clock = clock.Add(4 * time.Second)
	if err := spec.SendIfConnected(t.Context(), "spec"); err != nil {
		t.Fatal(err)
	}
	// ハードウェア情報の hold（8 秒）の間は、ログイン履歴を送らせない
	clock = clock.Add(5 * time.Second)
	if err := logins.SendIfConnected(t.Context(), "login_events"); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	clock = clock.Add(3 * time.Second)
	if err := logins.SendIfConnected(t.Context(), "login_events"); err != nil {
		t.Fatal(err)
	}

	if got := len(fc.sent); got != 4 {
		t.Errorf("sent = %v", fc.sent)
	}
}

// 送れなかった（接続していない）メッセージは、他の送り手を待たせない。
func TestAckGateFailedSendDoesNotHold(t *testing.T) {
	fc := &gateFakeConn{err: ErrNotConnected}
	g := NewAckGate(fc)
	a, b := g.Client(time.Hour), g.Client(time.Hour)
	if err := a.SendIfConnected(t.Context(), "x"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v", err)
	}
	fc.err = nil
	if err := b.SendIfConnected(t.Context(), "y"); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}
