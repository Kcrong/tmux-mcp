package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestServe_ShutdownTimeout_DrainCompletes verifies the happy path: a
// handler that finishes well within the configured drain window after
// ctx cancellation is allowed to write its response, and Serve returns
// ctx.Canceled (not ErrShutdownTimedOut).
func TestServe_ShutdownTimeout_DrainCompletes(t *testing.T) {
	t.Parallel()
	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	w := &lockedWriter{w: out, mu: outMu}

	handlerStarted := make(chan struct{})
	handler := func(_ context.Context, _ string, _ json.RawMessage) (any, *rpcError) {
		close(handlerStarted)
		// Sleep well below the 2s drain budget so the response makes
		// it onto the wire before drain returns.
		time.Sleep(150 * time.Millisecond)
		return map[string]any{"ok": true}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, in, w, handler, WithShutdownTimeout(2*time.Second))
	}()

	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"slow"}` + "\n"))
	<-handlerStarted

	// Cancel the context mid-handler. The drain budget (2s) is much
	// larger than the remaining handler work (~150ms), so Serve should
	// wait for the response to land and then exit cleanly.
	cancel()
	// Closing stdin makes ReadBytes return EOF — without it the read
	// loop stays parked indefinitely waiting for the next frame.
	in.Close()

	select {
	case err := <-done:
		// ctx.Canceled is acceptable; nil (EOF after drain) is the
		// other valid outcome depending on whether the read loop
		// observed cancellation before the EOF surfaced.
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve returned unexpected error: %v", err)
		}
		if errors.Is(err, ErrShutdownTimedOut) {
			t.Fatalf("Serve should not have timed out: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return within drain window + slack")
	}

	outMu.Lock()
	body := out.String()
	outMu.Unlock()
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("expected handler response to land before Serve returned, got %q", body)
	}
}

// TestServe_ShutdownTimeout_Exceeded verifies the forced-shutdown path:
// a handler that runs longer than the drain budget causes Serve to
// return ErrShutdownTimedOut, the caller can map that to a non-zero
// exit, and the warning log fires so operators see the forced
// teardown.
func TestServe_ShutdownTimeout_Exceeded(t *testing.T) {
	t.Parallel()
	logs := withCapturedLogs(t)

	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	w := &lockedWriter{w: out, mu: outMu}

	// Handler intentionally ignores ctx and runs much longer than the
	// drain budget. We want Serve to give up and return
	// ErrShutdownTimedOut without waiting forever.
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	handler := func(_ context.Context, _ string, _ json.RawMessage) (any, *rpcError) {
		close(handlerStarted)
		<-releaseHandler
		return map[string]any{"late": true}, nil
	}
	t.Cleanup(func() { close(releaseHandler) }) // unblock the handler so its goroutine can exit

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, in, w, handler, WithShutdownTimeout(100*time.Millisecond))
	}()

	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"stuck"}` + "\n"))
	<-handlerStarted
	cancel()
	in.Close()

	select {
	case err := <-done:
		if !errors.Is(err, ErrShutdownTimedOut) {
			t.Fatalf("expected ErrShutdownTimedOut, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after drain timeout")
	}

	logs.Close()
	body, _ := readAll(t, logs)
	if !strings.Contains(body, "shutdown drain timed out") {
		t.Fatalf("expected drain-timeout warning in logs, got: %s", body)
	}
}

// TestServe_ShutdownTimeout_RejectsNewRequests verifies that, after the
// context is cancelled, frames still arriving on stdin are rejected
// with -32603 "shutting down" instead of being dispatched to the
// handler. This guards against a flooding client extending the drain
// window indefinitely.
func TestServe_ShutdownTimeout_RejectsNewRequests(t *testing.T) {
	t.Parallel()
	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	w := &lockedWriter{w: out, mu: outMu}

	var dispatched atomic.Int32
	dispatchAfterCancel := make(chan struct{})
	handler := func(_ context.Context, _ string, _ json.RawMessage) (any, *rpcError) {
		dispatched.Add(1)
		close(dispatchAfterCancel)
		return map[string]any{"ok": true}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, in, w, handler, WithShutdownTimeout(2*time.Second))
	}()

	// Cancel before sending the request, then write the frame. Serve
	// must NOT dispatch the handler — it should reply with -32603
	// "shutting down".
	cancel()
	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":99,"method":"do_thing"}` + "\n"))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		outMu.Lock()
		body := out.String()
		outMu.Unlock()
		if strings.Contains(body, "shutting down") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	in.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after EOF")
	}

	// Confirm: handler was never dispatched, the response was the
	// shutdown stub.
	if got := dispatched.Load(); got != 0 {
		t.Fatalf("handler should not have run after cancel, got dispatched=%d", got)
	}
	select {
	case <-dispatchAfterCancel:
		t.Fatal("handler ran despite ctx cancellation")
	default:
	}

	outMu.Lock()
	body := out.String()
	outMu.Unlock()
	var resp map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &resp); err != nil {
		t.Fatalf("decode response %q: %v", body, err)
	}
	rpcErr, _ := resp["error"].(map[string]any)
	if rpcErr == nil {
		t.Fatalf("expected error envelope, got %#v", resp)
	}
	// JSON numbers decode as float64.
	if code, _ := rpcErr["code"].(float64); int(code) != codeInternalError {
		t.Fatalf("expected code=%d (-32603), got %v", codeInternalError, rpcErr["code"])
	}
	if msg, _ := rpcErr["message"].(string); msg != "shutting down" {
		t.Fatalf("expected message=\"shutting down\", got %q", msg)
	}
}

// TestServe_ShutdownTimeout_ZeroSkipsDrain verifies the explicit opt-out:
// WithShutdownTimeout(0) makes Serve return immediately on ctx cancel
// without waiting for in-flight handlers. This is the back-compat
// escape hatch for callers (tests, scripts) that don't care about
// landing in-flight responses.
func TestServe_ShutdownTimeout_ZeroSkipsDrain(t *testing.T) {
	t.Parallel()
	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	w := &lockedWriter{w: out, mu: outMu}

	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	handler := func(_ context.Context, _ string, _ json.RawMessage) (any, *rpcError) {
		close(handlerStarted)
		<-releaseHandler
		return nil, nil
	}
	t.Cleanup(func() { close(releaseHandler) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, in, w, handler, WithShutdownTimeout(0))
	}()

	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"hang"}` + "\n"))
	<-handlerStarted
	cancel()
	in.Close()

	// With drain disabled, Serve should return promptly even though
	// the handler is still parked. The exact return value is allowed
	// to be ctx.Canceled or nil; the contract is "don't block".
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Serve did not return promptly with shutdownTimeout=0")
	}
}

// TestServe_ShutdownTimeout_NotSetWaitsForever_BackCompat verifies
// callers that don't apply WithShutdownTimeout keep the historical
// "wait indefinitely for handlers" semantics. We assert by giving the
// handler a finite (but non-trivial) sleep and confirming Serve's
// return only happens after the handler counter ticks.
func TestServe_ShutdownTimeout_NotSetWaitsForever_BackCompat(t *testing.T) {
	t.Parallel()
	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	w := &lockedWriter{w: out, mu: outMu}

	var counter atomic.Int32
	handlerStarted := make(chan struct{})
	handler := func(_ context.Context, _ string, _ json.RawMessage) (any, *rpcError) {
		close(handlerStarted)
		time.Sleep(150 * time.Millisecond)
		counter.Add(1)
		return map[string]any{"done": true}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	// Note: no WithShutdownTimeout — exercises the back-compat path.
	go func() { done <- Serve(ctx, in, w, handler) }()

	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"slow"}` + "\n"))
	<-handlerStarted
	in.Close()

	select {
	case <-done:
		if got := counter.Load(); got != 1 {
			t.Fatalf("Serve returned before handler finished: counter=%d", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return within 2s")
	}
}

// readAll is a small helper that drains a threadSafeBuffer that has
// already been Close()d. It returns the raw body and propagates any
// non-EOF read error so callers can fail the test on broken pipes.
func readAll(t *testing.T, b *threadSafeBuffer) (string, error) {
	t.Helper()
	var sb strings.Builder
	tmp := make([]byte, 4096)
	for {
		n, err := b.Read(tmp)
		if n > 0 {
			sb.Write(tmp[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return sb.String(), nil
			}
			return sb.String(), err
		}
	}
}
