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
	// 誰も待っていなければ、同じ送り手は続けて送れる（自分の間隔は送り手が管理する）
	clock = clock.Add(time.Second)
	if err := logins.SendIfConnected(t.Context(), "sessions"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Second)
	if err := spec.SendIfConnected(t.Context(), "spec"); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
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

// ログイン履歴が送り続けても、待たされたハードウェア情報に順番が回ってくる。
func TestAckGateWaiterGetsTurn(t *testing.T) {
	fc := &gateFakeConn{}
	g := NewAckGate(fc)
	clock := time.Unix(0, 0)
	g.now = func() time.Time { return clock }
	logins := g.Client(4 * time.Second)
	spec := g.Client(8 * time.Second)

	// ログイン履歴は 3 秒ごとに送ろうとし、ハードウェア情報は 5 秒ごとに送ろうとする
	specSentAt := time.Duration(-1)
	for ms := time.Duration(0); ms <= 60*time.Second; ms += time.Second {
		clock = time.Unix(0, 0).Add(ms)
		if ms%(3*time.Second) == 0 {
			_ = logins.SendIfConnected(t.Context(), "login_events")
		}
		if ms > 0 && ms%(5*time.Second) == 0 && specSentAt < 0 {
			if spec.SendIfConnected(t.Context(), "spec") == nil {
				specSentAt = ms
			}
		}
	}
	if specSentAt < 0 {
		t.Fatalf("spec was never sent: %v", fc.sent)
	}
	if specSentAt > 15*time.Second {
		t.Errorf("spec was sent at %v, want within 15s", specSentAt)
	}
}

// 順番待ちの送り手が送り直さなくなったら、他の送り手を待たせ続けない。
func TestAckGateWaiterExpires(t *testing.T) {
	fc := &gateFakeConn{}
	g := NewAckGate(fc)
	clock := time.Unix(0, 0)
	g.now = func() time.Time { return clock }
	logins := g.Client(4 * time.Second)
	spec := g.Client(8 * time.Second)

	_ = logins.SendIfConnected(t.Context(), "a")
	clock = clock.Add(time.Second)
	if err := spec.SendIfConnected(t.Context(), "spec"); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v", err)
	}
	// ハードウェア情報が順番待ちの間は、ログイン履歴も続けて送れない
	clock = clock.Add(2 * time.Second)
	if err := logins.SendIfConnected(t.Context(), "b"); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy while spec is waiting", err)
	}
	// ハードウェア情報が送り直さないまま時間が過ぎたら、順番待ちを外す
	clock = clock.Add(waiterTTL + time.Second)
	if err := logins.SendIfConnected(t.Context(), "c"); err != nil {
		t.Fatalf("err = %v, want nil after the waiter expired", err)
	}
}
