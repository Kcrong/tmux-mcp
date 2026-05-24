package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestServe_RecoversFromHandlerPanic verifies that a panic inside the
// user handler does not crash Serve, does not strand the in-flight
// WaitGroup (which would hang Shutdown), and produces a generic
// JSON-RPC error response with code -32603 ("internal server error").
// The panic value/stack must not leak to the client.
func TestServe_RecoversFromHandlerPanic(t *testing.T) {
	t.Parallel()
	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: out, mu: outMu}

	// Handler that panics on the "explode" method and otherwise replies
	// normally — lets us verify Serve keeps working after the panic.
	const secret = "totally-not-leaked-stack-frame"
	handler := func(_ context.Context, method string, _ json.RawMessage) (any, *rpcError) {
		switch method {
		case "explode":
			panic(secret)
		case "ping":
			return map[string]any{"pong": true}, nil
		}
		return nil, methodNotFound(method)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, in, syncWriter, handler) }()

	// 1. Panicking call.
	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"explode"}` + "\n"))

	// 2. Subsequent normal call — proves Serve loop still alive.
	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"))

	// Wait until both responses land in the output buffer.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		outMu.Lock()
		body := out.String()
		outMu.Unlock()
		if strings.Contains(body, "-32603") && strings.Contains(body, "pong") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	outMu.Lock()
	body := out.String()
	outMu.Unlock()

	// Parse each line and find the panic response (id=1) plus the ping
	// response (id=2).
	var (
		panicResp map[string]any
		pingResp  map[string]any
	)
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode response %q: %v", line, err)
		}
		// JSON numbers decode as float64.
		switch rec["id"] {
		case float64(1):
			panicResp = rec
		case float64(2):
			pingResp = rec
		}
	}
	if panicResp == nil {
		t.Fatalf("no panic response (id=1) found in output: %q", body)
	}
	if pingResp == nil {
		t.Fatalf("no ping response (id=2) found in output: %q", body)
	}

	// Panic response must carry an error with code -32603 and a
	// generic message — not the panic value, not the stack.
	rpcErrField, ok := panicResp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error field on panic response, got %#v", panicResp)
	}
	if got := rpcErrField["code"]; got != float64(-32603) {
		t.Fatalf("expected code -32603, got %#v", got)
	}
	msg, _ := rpcErrField["message"].(string)
	if msg != "internal server error" {
		t.Fatalf("expected generic message, got %q", msg)
	}
	// CRITICAL: ensure neither the panic value nor stack frames leaked.
	if strings.Contains(body, secret) {
		t.Fatalf("panic value leaked to client: %q", body)
	}
	if strings.Contains(body, "goroutine ") || strings.Contains(body, "runtime.gopanic") {
		t.Fatalf("stack trace leaked to client: %q", body)
	}

	// Ping response should be a normal success.
	pingResult, _ := pingResp["result"].(map[string]any)
	if pingResult["pong"] != true {
		t.Fatalf("expected ping pong=true, got %#v", pingResp)
	}

	// Closing stdin must let Serve's wg.Wait() return — if the panic
	// recovery had skipped wg.Done, this would deadlock.
	in.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after stdin close — wg.Wait() likely hung after handler panic")
	}
}

// TestServe_PanicInNotificationDoesNotReply asserts that a panic in a
// notification (no id) handler is recovered without sending any
// response — JSON-RPC notifications must never produce a reply, even
// for errors.
func TestServe_PanicInNotificationDoesNotReply(t *testing.T) {
	t.Parallel()
	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: out, mu: outMu}

	handlerInvoked := make(chan struct{}, 1)
	handler := func(_ context.Context, _ string, _ json.RawMessage) (any, *rpcError) {
		handlerInvoked <- struct{}{}
		panic("boom in notification")
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, in, syncWriter, handler) }()

	// Notification: no "id" field.
	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","method":"notify"}` + "\n"))

	select {
	case <-handlerInvoked:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never ran")
	}

	// Close stdin and wait for Serve to drain. If the wg leaked, this
	// would deadlock.
	in.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve hung after notification panic — wg.Done was likely skipped")
	}

	outMu.Lock()
	body := out.String()
	outMu.Unlock()
	if strings.TrimSpace(body) != "" {
		t.Fatalf("notification panic must not produce a response, got %q", body)
	}
}

// TestServe_PanicLogsStackToLogger verifies the panic + stack make it
// into the structured logs (operators need them) but never into the
// wire output (clients must not).
func TestServe_PanicLogsStackToLogger(t *testing.T) {
	logs := withCapturedLogs(t)

	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: out, mu: outMu}

	const sentinel = "panic-sentinel-9f8e7d6c"
	handler := func(_ context.Context, _ string, _ json.RawMessage) (any, *rpcError) {
		panic(sentinel)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, in, syncWriter, handler) }()

	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":99,"method":"crash"}` + "\n"))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		outMu.Lock()
		body := out.String()
		outMu.Unlock()
		if strings.Contains(body, "-32603") {
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

	logs.Close()
	raw, err := io.ReadAll(logs)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}

	var sawPanicLog, sawStack, sawRid bool
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec["msg"] == "handler panic" {
			sawPanicLog = true
			if level, _ := rec["level"].(string); level != "ERROR" {
				t.Fatalf("expected ERROR level on panic log, got %q", level)
			}
			if pv, _ := rec["panic"].(string); !strings.Contains(pv, sentinel) {
				t.Fatalf("expected panic value in log, got %q", pv)
			}
			if stk, _ := rec["stack"].(string); strings.Contains(stk, "runtime") || strings.Contains(stk, "panic") {
				sawStack = true
			}
			if rid, _ := rec["rid"].(string); rid != "" {
				sawRid = true
			}
		}
	}
	if !sawPanicLog {
		t.Fatalf("expected a 'handler panic' log line, logs=%s", raw)
	}
	if !sawStack {
		t.Fatalf("expected stack trace in log, logs=%s", raw)
	}
	if !sawRid {
		t.Fatalf("expected request id on panic log, logs=%s", raw)
	}

	// Wire output must NOT contain the panic value or stack.
	outMu.Lock()
	body := out.String()
	outMu.Unlock()
	if strings.Contains(body, sentinel) {
		t.Fatalf("panic value leaked to client output: %q", body)
	}
	if strings.Contains(body, "goroutine ") {
		t.Fatalf("stack trace leaked to client output: %q", body)
	}
}
