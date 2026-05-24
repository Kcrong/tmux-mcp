package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestCallLimiter_NoOpWhenLimitZero asserts the "limit unlimited"
// contract: when newCallLimiter is constructed with limit <= 0 we get
// nil back, and calling Acquire/Release on the nil receiver is a no-op
// — every Acquire returns immediately with a nil error and never
// blocks, regardless of how many goroutines pile in. This preserves
// the pre-flag behaviour for operators who pass -max-concurrent-calls=0.
func TestCallLimiter_NoOpWhenLimitZero(t *testing.T) {
	t.Parallel()

	for _, limit := range []int{0, -1, -100} {
		l := newCallLimiter(limit)
		if l != nil {
			t.Fatalf("newCallLimiter(%d): expected nil, got %p", limit, l)
		}
	}

	l := newCallLimiter(0)

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	// 200ms is generous: a no-op limiter must not block any of the
	// 10 callers. If even one Acquire stalled, the deadline would
	// elapse and the test would fail with a clear "did not return".
	deadline := time.Now().Add(200 * time.Millisecond)
	errCh := make(chan error, goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			if err := l.Acquire(ctx, "tools/call"); err != nil {
				errCh <- err
				return
			}
			l.Release()
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("Acquire on nil limiter must never error, got %v", err)
	}
}

// TestCallLimiter_EnforcesConcurrencyCeiling pins down the load-bearing
// invariant of the limiter: with limit=2 and 5 goroutines competing
// for a slot, no more than 2 may sit inside the critical section
// simultaneously. We use atomic.Int32 to track current/peak in-flight
// counts; the goroutine bumps the counter under the slot, sleeps long
// enough that contention is real, then decrements. After all 5 finish,
// the peak must be <= 2 and we must have observed all 5 successful
// holds.
func TestCallLimiter_EnforcesConcurrencyCeiling(t *testing.T) {
	t.Parallel()

	const (
		limit          = 2
		goroutines     = 5
		holdDuration   = 50 * time.Millisecond
		acquireTimeout = 5 * time.Second
	)

	l := newCallLimiter(limit)

	var (
		current atomic.Int32
		peak    atomic.Int32
		held    atomic.Int32
		wg      sync.WaitGroup
	)
	wg.Add(goroutines)
	// Release-the-hounds barrier: every goroutine waits on this
	// channel before the first Acquire so all 5 contend at once,
	// rather than serialising in startup-jitter order.
	start := make(chan struct{})

	for range goroutines {
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), acquireTimeout)
			defer cancel()
			if err := l.Acquire(ctx, "tools/call"); err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			defer l.Release()

			now := current.Add(1)
			// CompareAndSwap loop: bump peak only if our `now` is
			// the new high-water mark. Reading peak.Load() and
			// blindly Storing would race with another goroutine
			// updating it between the read and the write.
			for {
				p := peak.Load()
				if now <= p {
					break
				}
				if peak.CompareAndSwap(p, now) {
					break
				}
			}
			held.Add(1)
			time.Sleep(holdDuration)
			current.Add(-1)
		}()
	}
	close(start)
	wg.Wait()

	if got := peak.Load(); got > int32(limit) {
		t.Fatalf("peak in-flight %d exceeded limit %d", got, limit)
	}
	if got := held.Load(); got != int32(goroutines) {
		t.Fatalf("expected %d goroutines to acquire successfully, got %d",
			goroutines, got)
	}
	// Sanity: with 5 goroutines fighting over 2 slots and a 50ms
	// hold, the peak should actually *reach* the limit. If it
	// doesn't, the test is too lax — it would pass even if the
	// limiter incorrectly serialised everything to 1 at a time.
	if got := peak.Load(); got != int32(limit) {
		t.Fatalf("expected peak %d to reach limit %d (test would otherwise be too lax)",
			got, limit)
	}
}

// TestCallLimiter_ContextCancellationUnblocksCaller asserts the
// cancellation contract. With limit=1 and the slot already held, a
// second Acquire must block — but if the caller's context is cancelled
// while it waits, Acquire must promptly return a non-nil error that:
//  1. wraps errs.ErrTimeout (so the dispatcher's CodeOf maps it to the
//     existing CodeTimeout JSON-RPC code, no new sentinel needed), and
//  2. wraps the underlying ctx.Err() so callers can still distinguish
//     context.Canceled from context.DeadlineExceeded via errors.Is.
//
// The blocked caller MUST NOT call Release() — it never acquired the
// slot. The test verifies this implicitly: if the held goroutine
// released and the cancelled caller then erroneously released,
// subsequent Acquires would race and the limiter's Release-without-
// Acquire panic guard would fire.
func TestCallLimiter_ContextCancellationUnblocksCaller(t *testing.T) {
	t.Parallel()

	l := newCallLimiter(1)

	// Hold the only slot from a separate goroutine so the test goroutine
	// can run the cancellation path in the foreground.
	holderRelease := make(chan struct{})
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := l.Acquire(ctx, "tools/call"); err != nil {
			t.Errorf("holder Acquire: %v", err)
			return
		}
		<-holderRelease
		l.Release()
	}()
	// Brief settle: ensure the holder has actually claimed the slot
	// before we issue the contended Acquire below. Without this we'd
	// be racing the holder's own Acquire and could occasionally test
	// the uncontended fast path, which doesn't exercise cancellation.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(l.sem) == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(l.sem) != 1 {
		t.Fatalf("holder never claimed slot; len(sem)=%d", len(l.sem))
	}

	// Contended Acquire: cancel the context after ~10ms and assert the
	// blocked Acquire returns within a small grace window.
	ctx, cancel := context.WithCancel(context.Background())
	acquireResult := make(chan error, 1)
	go func() { acquireResult <- l.Acquire(ctx, "tools/call") }()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-acquireResult:
		if err == nil {
			t.Fatal("expected non-nil error after ctx cancel, got nil — Release would underflow")
		}
		if !errors.Is(err, errs.ErrTimeout) {
			t.Fatalf("expected error wrapping errs.ErrTimeout, got %v", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected error wrapping context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not return after ctx cancel within 2s — limiter is not respecting cancellation")
	}

	// Tidy up: let the holder release, then wait for it. If the
	// cancelled goroutine had wrongly called Release(), this final
	// Release() would panic via the guard in callLimiter.Release —
	// catching the bug at test time rather than in production.
	close(holderRelease)
	<-holderDone
}

// TestServe_GatesOnlyToolsCall is the dispatcher-side integration
// counterpart to the unit tests above. It locks down two contractual
// promises:
//
//  1. tools/call frames are gated by WithMaxConcurrentCalls — at most
//     `limit` may run concurrently no matter how many the client sends
//     at once.
//  2. initialize / tools/list / notifications are NOT gated — they
//     stay snappy even when the limiter is fully saturated by
//     long-running tool calls. (A naive "gate everything" would cause
//     a tools/list issued during a backlog to block until a slot
//     opened up.)
//
// We drive Serve with limit=1 and a handler that:
//   - blocks on a release channel for tools/call (so the slot stays held)
//   - returns immediately for everything else
//
// then verify that initialize/tools/list responses arrive *while* a
// tools/call is still parked in the limiter.
func TestServe_GatesOnlyToolsCall(t *testing.T) {
	t.Parallel()

	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: out, mu: outMu}

	var (
		toolStarted = make(chan struct{}, 1)
		release     = make(chan struct{})
		toolCalls   atomic.Int32
	)
	handler := func(ctx context.Context, method string, _ json.RawMessage) (any, *rpcError) {
		switch method {
		case "tools/call":
			n := toolCalls.Add(1)
			// First call signals "I'm holding the slot" and waits on
			// release; later calls are also held until release fires
			// so the slot stays continuously claimed for the duration
			// of the test.
			if n == 1 {
				toolStarted <- struct{}{}
			}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, internalError(ctx.Err())
			}
			return map[string]any{"ok": true}, nil
		case "initialize":
			return map[string]any{"protocolVersion": "test"}, nil
		case "tools/list":
			return map[string]any{"tools": []any{}}, nil
		}
		return nil, methodNotFound(method)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, in, syncWriter, handler, WithMaxConcurrentCalls(1))
	}()

	// Frame 1: tools/call that will park inside the limiter (slot is now held).
	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}` + "\n"))
	select {
	case <-toolStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first tools/call never reached handler")
	}

	// Frame 2: another tools/call — must NOT reach the handler yet
	// because the limiter is full. We assert this by checking
	// toolCalls is still 1 after a brief wait.
	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{}}` + "\n"))

	// Frames 3 + 4: initialize and tools/list — must NOT be gated.
	// They should produce responses while the tools/call slot is held.
	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":3,"method":"initialize"}` + "\n"))
	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":4,"method":"tools/list"}` + "\n"))

	// Wait for the two ungated responses.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		outMu.Lock()
		body := out.String()
		outMu.Unlock()
		if strings.Contains(body, `"id":3`) && strings.Contains(body, `"id":4`) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	outMu.Lock()
	body := out.String()
	outMu.Unlock()
	if !strings.Contains(body, `"id":3`) {
		t.Fatalf("initialize response (id=3) never arrived: %q", body)
	}
	if !strings.Contains(body, `"id":4`) {
		t.Fatalf("tools/list response (id=4) never arrived: %q", body)
	}
	// At this point the gated tools/call (id=2) must still be parked,
	// otherwise the limiter is leaking concurrency.
	if got := toolCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 tool call to have entered handler while slot held, got %d", got)
	}
	// id=1 response should NOT be in the output yet either.
	if strings.Contains(body, `"id":1`) {
		t.Fatalf("first tools/call (id=1) responded before release: %q", body)
	}

	// Release: now both queued tools/call frames can drain.
	close(release)

	// Wait for both tools/call responses.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		outMu.Lock()
		body := out.String()
		outMu.Unlock()
		if strings.Contains(body, `"id":1`) && strings.Contains(body, `"id":2`) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := toolCalls.Load(); got != 2 {
		t.Fatalf("expected both tools/call frames to have entered handler after release, got %d", got)
	}

	in.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not exit after stdin close")
	}
}

// TestServe_LimitZeroIsUnbounded confirms that passing
// WithMaxConcurrentCalls(0) restores pre-flag behaviour: many
// concurrent tools/call frames all run at once instead of being
// serialised. With limit=0 the limiter is nil and Acquire/Release are
// no-ops, so 5 simultaneous frames should all reach the handler
// before any of them returns.
func TestServe_LimitZeroIsUnbounded(t *testing.T) {
	t.Parallel()

	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: out, mu: outMu}

	const callers = 5
	var inFlight atomic.Int32
	allArrived := make(chan struct{})
	release := make(chan struct{})
	handler := func(_ context.Context, method string, _ json.RawMessage) (any, *rpcError) {
		if method != "tools/call" {
			return nil, methodNotFound(method)
		}
		// Bump counter; once all callers are in flight, broadcast.
		if inFlight.Add(1) == callers {
			close(allArrived)
		}
		<-release
		return map[string]any{"ok": true}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		// limit=0 → no-op limiter.
		done <- Serve(ctx, in, syncWriter, handler, WithMaxConcurrentCalls(0))
	}()

	for i := 1; i <= callers; i++ {
		// We don't actually parse params in the handler so empty {} is fine.
		frame := []byte(`{"jsonrpc":"2.0","id":` + intStr(i) + `,"method":"tools/call","params":{}}` + "\n")
		_, _ = in.Write(frame)
	}

	// All 5 must reach the handler concurrently. If the limiter were
	// erroneously gating this case (limit=0 but treated as 1, say),
	// allArrived would never close and the test would time out.
	select {
	case <-allArrived:
	case <-time.After(3 * time.Second):
		t.Fatalf("expected %d concurrent handler invocations with limit=0, got %d in flight", callers, inFlight.Load())
	}

	close(release)
	in.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not exit after stdin close")
	}
}

// intStr converts a small positive int to its decimal string form
// without dragging in fmt for a single sprintf in tests.
func intStr(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for n > 0 {
		pos--
		b[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(b[pos:])
}

// TestCallLimiter_DeadlineCancellationWrapsDeadlineExceeded covers the
// "deadline elapsed while waiting" path. Same shape as the cancellation
// test but uses a context.WithDeadline so the returned error must wrap
// context.DeadlineExceeded rather than context.Canceled. Both should
// still classify as errs.ErrTimeout for the JSON-RPC code mapping.
func TestCallLimiter_DeadlineCancellationWrapsDeadlineExceeded(t *testing.T) {
	t.Parallel()

	l := newCallLimiter(1)

	holderRelease := make(chan struct{})
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := l.Acquire(ctx, "tools/call"); err != nil {
			t.Errorf("holder Acquire: %v", err)
			return
		}
		<-holderRelease
		l.Release()
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(l.sem) == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	t.Cleanup(cancel)
	err := l.Acquire(ctx, "tools/call")
	if err == nil {
		t.Fatal("expected non-nil error after deadline elapsed, got nil")
	}
	if !errors.Is(err, errs.ErrTimeout) {
		t.Fatalf("expected error wrapping errs.ErrTimeout, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected error wrapping context.DeadlineExceeded, got %v", err)
	}

	close(holderRelease)
	<-holderDone
}
