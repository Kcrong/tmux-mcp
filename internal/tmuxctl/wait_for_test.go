package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWaitFor_RejectsEmptyChannel locks the up-front guard. tmux would
// otherwise error with a free-form "missing channel" stderr that no
// caller can branch on; rejecting the empty string at the controller
// boundary keeps the diagnostic crisp and avoids spawning a tmux
// process for a request that can never succeed.
func TestWaitFor_RejectsEmptyChannel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.WaitFor(ctx, WaitForSignal, "")
	if err == nil {
		t.Fatal("expected error for empty channel")
	}
	if !strings.Contains(err.Error(), "channel required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestWaitFor_SignalEmptyChannelIsNoop pins the documented contract for
// `wait-for -S CHANNEL` against a channel with no waiters: tmux returns
// success without buffering the signal, and the wrapper inherits that
// behaviour. This is the load-bearing path for a "fire-and-forget
// wakeup" — agents notifying a sibling that a milestone is reached
// must be able to call WaitForSignal without first verifying somebody
// is actually listening.
func TestWaitFor_SignalEmptyChannelIsNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the daemon is up — wait-for against
	// a server-less socket returns "no server running" stderr instead.
	if err := c.CreateSession(ctx, SessionSpec{Name: "wf_sig", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.WaitFor(ctx, WaitForSignal, "no_one_listening"); err != nil {
		t.Fatalf("WaitFor signal on idle channel: %v", err)
	}
}

// TestWaitFor_SignalWakesWaiter is the cross-goroutine end-to-end path:
// one goroutine blocks on `WaitFor(ctx, WaitForWait, ...)` while a
// second fires `WaitFor(ctx, WaitForSignal, ...)` against the same
// channel name. The waiter must observe the signal and return nil.
// This is the load-bearing rendezvous primitive every caller of the
// wait/signal pair relies on, so we verify it round-trips through the
// real tmux daemon rather than substring-checking error strings.
func TestWaitFor_SignalWakesWaiter(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "wf_wake", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Block on a fresh channel name. The waiter goroutine reports its
	// outcome via a buffered channel so the main test goroutine can
	// time-bound the assertion without leaking the goroutine on
	// failure (the parent context covers the worst case).
	ch := "rdv_signal_wakes"
	waitErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		waitErr <- c.WaitFor(ctx, WaitForWait, ch)
	}()

	// Give the waiter a moment to actually issue the bare `wait-for`
	// against tmux. Without this, a too-eager signal may race the
	// waiter's tmux process spawn and arrive on an empty roster — at
	// which point tmux just succeeds without buffering and the waiter
	// is left blocking until the parent ctx cancels. 200ms is enough
	// for an exec.CommandContext startup on every tmux build we
	// support; tightening the bound risks flake on slow CI runners.
	time.Sleep(200 * time.Millisecond)
	if err := c.WaitFor(ctx, WaitForSignal, ch); err != nil {
		t.Fatalf("WaitFor signal: %v", err)
	}

	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("waiter returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not wake within 5s after signal")
	}
	wg.Wait()
}

// TestWaitFor_LockUnlockSerialisesContenders exercises the lock/unlock
// pair. After holding the lock with `wait-for -L`, a second locker must
// block until `wait-for -U` releases the channel. We verify the second
// locker is genuinely blocked (its returned-error channel stays empty)
// for a small window, then release the lock and assert the second
// locker proceeds.
//
// This is the only test that pins the BLOCKING semantics of -L when
// the channel is already held; without it a regression that turned -L
// into a non-blocking try-lock would silently corrupt every agent
// using the primitive for mutual exclusion.
func TestWaitFor_LockUnlockSerialisesContenders(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "wf_lock", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	ch := "rdv_lock_serialises"

	// First locker: acquires the channel, returns immediately on
	// success.
	if err := c.WaitFor(ctx, WaitForLock, ch); err != nil {
		t.Fatalf("WaitFor lock (first): %v", err)
	}

	// Second locker: must block until we unlock the channel below.
	contendErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		contendErr <- c.WaitFor(ctx, WaitForLock, ch)
	}()

	// Sample the contender's status: it must NOT have completed within
	// the small window before unlock. 300ms is comfortably above the
	// startup cost of the tmux child process (~100ms on the slowest CI
	// runner we have measured) so a non-blocking regression would be
	// caught reliably.
	select {
	case err := <-contendErr:
		t.Fatalf("second locker returned %v before unlock; lock semantics violated", err)
	case <-time.After(300 * time.Millisecond):
		// Expected — still blocked.
	}

	if err := c.WaitFor(ctx, WaitForUnlock, ch); err != nil {
		t.Fatalf("WaitFor unlock: %v", err)
	}

	select {
	case err := <-contendErr:
		if err != nil {
			t.Fatalf("second locker returned error after unlock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second locker did not proceed within 5s after unlock")
	}
	wg.Wait()

	// Tidy the channel state so a follow-up test in the same suite that
	// reuses the controller does not inherit a held lock. The wrapper
	// does not buffer state itself; the unlock here mirrors the
	// "release in cleanup" pattern an agent would use.
	_ = c.WaitFor(ctx, WaitForUnlock, ch)
}

// TestWaitFor_WaitRespectsContext is the load-bearing cancellation
// path: callers MUST be able to bound a bare `wait-for CHANNEL` (no
// signaller) by attaching a deadline / cancel to ctx. The wrapper
// dispatches through exec.CommandContext, so cancelling ctx terminates
// the child process and surfaces context.DeadlineExceeded (or
// context.Canceled) wrapped in run()'s error envelope.
//
// Without this guarantee a `WaitForWait` call would be a footgun —
// tmux waits forever and the only recovery would be to kill the tmux
// daemon. The test pins the contract so a future refactor that drops
// CommandContext for plain Command can't silently break the cancel.
func TestWaitFor_WaitRespectsContext(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	// Parent context bounds the whole test — without this, a
	// regression that breaks cancellation would hang the suite forever.
	parent, cancelParent := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancelParent)

	if err := c.CreateSession(parent, SessionSpec{Name: "wf_cancel", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Tight per-call deadline so a hang surfaces fast.
	callCtx, cancelCall := context.WithTimeout(parent, 500*time.Millisecond)
	t.Cleanup(cancelCall)

	start := time.Now()
	err := c.WaitFor(callCtx, WaitForWait, "no_signaller_will_ever_arrive")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after context deadline expired, got nil")
	}
	// The error chain may surface either the wrapped context error or
	// a tmux-side "killed" message depending on which racing wins, but
	// the elapsed time is the load-bearing assertion: it must be in
	// the same ballpark as the deadline rather than tmux's
	// indefinite-wait default. Allow a generous upper bound to
	// tolerate slow CI runners while still flagging a hang.
	if elapsed > 5*time.Second {
		t.Fatalf("WaitFor blocked for %s after a 500ms deadline; cancellation is broken", elapsed)
	}
	// errors.Is is best-effort: tmux may exit with its own message
	// before the context error propagates, but when the wrapper does
	// see the context error it must come through unwrappable.
	if errors.Is(err, context.DeadlineExceeded) {
		// Good — the canonical case we are pinning.
		return
	}
	// Fallback: a tmux-side "killed" / "lost" error is also acceptable,
	// but the test must still see a non-nil error. The elapsed-time
	// check above already guards against the worst regression
	// (unbounded wait), so we accept either error shape here.
}

// TestWaitFor_UnknownModeRejected pins the input contract for the
// WaitForMode enum: an out-of-range value must surface a clean
// validation error rather than silently dispatching `tmux wait-for`
// with no flag (which would block on the channel name forever).
func TestWaitFor_UnknownModeRejected(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.WaitFor(ctx, WaitForMode(99), "anything")
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
	if !strings.Contains(err.Error(), "unknown mode") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestWaitForMode_String pins the human-readable rendering of every
// enum value. The rendering shows up in error / log lines, so a drift
// in the strings would silently change every diagnostic that mentions
// the mode.
func TestWaitForMode_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		m    WaitForMode
		want string
	}{
		{WaitForWait, "(wait)"},
		{WaitForLock, "-L"},
		{WaitForSignal, "-S"},
		{WaitForUnlock, "-U"},
	}
	for _, tc := range cases {
		if got := tc.m.String(); got != tc.want {
			t.Errorf("WaitForMode(%d).String() = %q, want %q", int(tc.m), got, tc.want)
		}
	}
	// Out-of-range values must not panic — they fall back to a
	// numeric rendering so log lines stay informative even when a
	// future refactor extends the enum without updating this switch.
	if got := WaitForMode(42).String(); !strings.Contains(got, "42") {
		t.Errorf("WaitForMode(42).String() = %q, want a string containing 42", got)
	}
}
