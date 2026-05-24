package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestServe_EOFCancelsInFlightHandlerCtx is the regression guard for the
// "client disconnect should cancel in-flight tool calls" contract. The
// real bug it pins: before the dispatchCtx wiring landed, a handler
// that polled its own ctx (the way wait_for_text does inside
// tmuxctl.Controller.WaitForText) would keep running until its per-call
// timeout fired even after the client closed stdin. With timeout_ms set
// to 10_000, that meant the goroutine, the tmux IPC, and the response
// were all wasted for 10 seconds with nobody on the other end of the
// pipe to read the eventual reply.
//
// Test shape:
//  1. Dispatch a single "fake wait" handler that mimics wait_for_text's
//     poll loop — it watches ctx.Done() vs a 10-second timer that
//     stands in for timeout_ms, and on ctx cancellation surfaces the
//     same ctx.Err() the real tool would.
//  2. Wait until the handler is actually parked in its select.
//  3. Close stdin (EOF) without cancelling the parent ctx — the
//     scenario when an MCP client process exits or closes the pipe
//     without sending SIGTERM to tmux-mcp.
//  4. Assert the handler returns within ~100 ms with code -32003
//     (CodeContextCancelled), NOT after the 10 s "timeout" budget.
//
// We also assert Serve itself returns promptly so a hung dispatch loop
// doesn't get masked by the per-handler check.
func TestServe_EOFCancelsInFlightHandlerCtx(t *testing.T) {
	t.Parallel()

	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	w := &lockedWriter{w: out, mu: outMu}

	const fakeTimeoutMs = 10_000
	handlerStarted := make(chan struct{})
	// handlerDuration captures wall-clock time spent inside the handler;
	// we assert it is well under the 10s "timeout_ms" budget so a
	// future regression that re-introduces the bug fails loudly.
	var (
		handlerDuration time.Duration
		handlerCtxErr   error
		handlerMu       sync.Mutex
	)

	handler := func(ctx context.Context, _ string, _ json.RawMessage) (any, *rpcError) {
		close(handlerStarted)
		started := time.Now()
		// Mirror wait_for_text's poll select: a step ticker plus the
		// per-call timeout plus ctx.Done(). The bug surfaces when ctx
		// never fires and we have to ride out the entire timeout.
		t := time.NewTimer(time.Duration(fakeTimeoutMs) * time.Millisecond)
		defer t.Stop()
		step := time.NewTicker(50 * time.Millisecond)
		defer step.Stop()
		for {
			select {
			case <-ctx.Done():
				handlerMu.Lock()
				handlerDuration = time.Since(started)
				handlerCtxErr = ctx.Err()
				handlerMu.Unlock()
				// Return the same shape the real tool would: an
				// error wrapping context.Canceled / DeadlineExceeded
				// so errs.CodeOf maps it to CodeContextCancelled.
				return nil, internalError(ctx.Err())
			case <-t.C:
				// Per-call timeout: this is the "bug surfaced" path.
				// Map to ErrTimeout so the dispatcher emits CodeTimeout
				// and the test can distinguish it from cancellation.
				handlerMu.Lock()
				handlerDuration = time.Since(started)
				handlerMu.Unlock()
				return nil, internalError(errs.ErrTimeout)
			case <-step.C:
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, in, w, handler)
	}()

	// Kick off the long-running call.
	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"fake_wait"}` + "\n"))

	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}

	// Simulate client disconnect: close stdin WITHOUT cancelling ctx.
	// This is the scenario the fix targets — the parent process is
	// still alive, but the MCP client closed its end of the pipe.
	disconnectAt := time.Now()
	in.Close()

	// The handler must observe ctx.Done() within a small budget. 1s is
	// generous enough to ride out CI jitter while still being three
	// orders of magnitude below the fake 10s timeout the bug would
	// have made us wait for.
	select {
	case err := <-done:
		// Serve returned: either nil (clean drain) or context.Canceled.
		// What we care about is the *handler* completing promptly via
		// ctx, which the assertions below verify.
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return within 2s of stdin close")
	}

	totalSinceDisconnect := time.Since(disconnectAt)
	handlerMu.Lock()
	gotDur := handlerDuration
	gotCtxErr := handlerCtxErr
	handlerMu.Unlock()

	// Primary assertion: the handler bailed via ctx, not via the
	// 10-second timeout. The bug would land here with handlerCtxErr
	// nil and handlerDuration approximately 10s.
	if gotCtxErr == nil {
		t.Fatalf("handler did not observe ctx cancellation; duration=%s", gotDur)
	}
	if !errors.Is(gotCtxErr, context.Canceled) {
		t.Fatalf("handler ctx error = %v, want context.Canceled", gotCtxErr)
	}

	// Secondary assertion: the handler returned well before the
	// timeout budget. We allow 1s of slack vs the 10s timeout to keep
	// the test stable on slow CI runners — the actual runtime should
	// be one step (50ms) plus scheduling jitter.
	const slack = 1 * time.Second
	if gotDur > slack {
		t.Fatalf("handler ran for %s after EOF; want <%s (the 10s "+
			"timeout_ms budget shouldn't have been exhausted)", gotDur, slack)
	}
	if totalSinceDisconnect > 2*time.Second {
		t.Fatalf("handler took %s after stdin close to drain; want fast cancel", totalSinceDisconnect)
	}

	// On-the-wire assertion: the response that landed before Serve
	// drained must carry CodeContextCancelled (-32003). This protects
	// the contract callers see, not just the in-process ctx behaviour.
	outMu.Lock()
	body := strings.TrimSpace(out.String())
	outMu.Unlock()
	if body == "" {
		// Drain may have abandoned the response; that's still a valid
		// outcome under the default unbounded-drain back-compat path.
		// The handler-level assertion above is what really pins the
		// contract.
		return
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode response %q: %v", body, err)
	}
	rpcErr, _ := resp["error"].(map[string]any)
	if rpcErr == nil {
		t.Fatalf("expected error envelope, got %#v", resp)
	}
	if code, _ := rpcErr["code"].(float64); int(code) != errs.CodeContextCancelled {
		t.Fatalf("expected code=%d (-32003), got %v", errs.CodeContextCancelled, rpcErr["code"])
	}
}

// TestServe_EOFAlsoCancelsCtxBoundChildren is a smaller second test that
// pins the propagation rule directly: every Handler invocation must
// receive a context whose Done channel fires on stdin EOF, even though
// the parent ctx passed to Serve is still alive. The first test asserts
// the user-visible behaviour ("waiter exits fast"); this one asserts
// the underlying mechanism so a future refactor that derives reqCtx
// from the parent ctx again gets caught immediately.
func TestServe_EOFAlsoCancelsCtxBoundChildren(t *testing.T) {
	t.Parallel()

	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	w := &lockedWriter{w: out, mu: outMu}

	type captured struct {
		ctx context.Context
	}
	gotCh := make(chan captured, 1)
	handler := func(ctx context.Context, _ string, _ json.RawMessage) (any, *rpcError) {
		gotCh <- captured{ctx: ctx}
		// Park until ctx fires — same poll shape as the real tools,
		// minus the per-call timeout (we want EOF cancellation to be
		// the only exit path so a regression that breaks it shows up
		// as a test hang, not a slow timeout success).
		<-ctx.Done()
		return nil, internalError(ctx.Err())
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, in, w, handler) }()

	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":7,"method":"park"}` + "\n"))

	var got captured
	select {
	case got = <-gotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never received its ctx")
	}

	// Sanity: parent ctx is still alive when the handler is parked.
	if got.ctx.Err() != nil {
		t.Fatalf("handler ctx already cancelled before EOF: %v", got.ctx.Err())
	}

	in.Close()

	select {
	case <-got.ctx.Done():
		// Expected: EOF flowed through dispatchCtx into the handler's
		// reqCtx and tripped Done().
	case <-time.After(2 * time.Second):
		t.Fatal("handler ctx did not cancel within 2s of stdin close — " +
			"either Serve isn't cancelling dispatchCtx on EOF, or reqCtx " +
			"is still derived from the parent ctx instead of dispatchCtx")
	}

	if !errors.Is(got.ctx.Err(), context.Canceled) {
		t.Fatalf("handler ctx error = %v, want context.Canceled", got.ctx.Err())
	}

	// Parent ctx must remain unaffected — the fix is scoped to the
	// dispatch ctx, not the user-supplied one. A regression that
	// cancels the parent would leak into other call sites that share
	// the same ctx.
	if ctx.Err() != nil {
		t.Fatalf("parent ctx was cancelled by EOF; should only affect dispatch ctx (got %v)", ctx.Err())
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return within 2s of EOF")
	}
}
