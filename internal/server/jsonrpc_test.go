package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// threadSafeBuffer is a tiny buffered pipe — the server reads requests
// from it; the test writes them in.
type threadSafeBuffer struct {
	mu   sync.Mutex
	cond *sync.Cond
	buf  bytes.Buffer
	done bool
}

func (b *threadSafeBuffer) lazyInit() {
	if b.cond == nil {
		b.cond = sync.NewCond(&b.mu)
	}
}

func (b *threadSafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lazyInit()
	n, err := b.buf.Write(p)
	b.cond.Broadcast()
	return n, err
}

func (b *threadSafeBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lazyInit()
	for b.buf.Len() == 0 && !b.done {
		b.cond.Wait()
	}
	if b.buf.Len() == 0 && b.done {
		return 0, io.EOF
	}
	return b.buf.Read(p)
}

func (b *threadSafeBuffer) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lazyInit()
	b.done = true
	b.cond.Broadcast()
}

func TestServe_DispatchesAndReplies(t *testing.T) {
	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: out, mu: outMu}

	handler := func(_ context.Context, method string, params json.RawMessage) (any, *rpcError) {
		switch method {
		case "ping":
			return map[string]any{"pong": true}, nil
		case "boom":
			return nil, &rpcError{Code: -32000, Message: "boom"}
		}
		return nil, methodNotFound(method)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, in, syncWriter, handler) }()

	// Send a request.
	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"))

	// Wait until the response shows up.
	var resp map[string]any
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		outMu.Lock()
		body := out.String()
		outMu.Unlock()
		if strings.Contains(body, "pong") {
			if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &resp); err != nil {
				t.Fatalf("decode: %v body=%q", err, body)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resp == nil {
		t.Fatal("no response received")
	}
	if resp["jsonrpc"] != "2.0" {
		t.Fatalf("missing jsonrpc field: %#v", resp)
	}
	res, _ := resp["result"].(map[string]any)
	if res["pong"] != true {
		t.Fatalf("unexpected result: %#v", resp)
	}

	in.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after EOF")
	}
}

func TestServe_RejectsMalformedJSON(t *testing.T) {
	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	mu := &sync.Mutex{}
	w := &lockedWriter{w: out, mu: mu}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, in, w, func(context.Context, string, json.RawMessage) (any, *rpcError) {
			return nil, nil
		})
	}()
	_, _ = in.Write([]byte("not json\n"))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		body := out.String()
		mu.Unlock()
		if strings.Contains(body, "-32700") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	got := out.String()
	mu.Unlock()
	if !strings.Contains(got, "-32700") {
		t.Fatalf("expected parse error -32700, got %q", got)
	}
	in.Close()
	<-done
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// withCapturedLogs swaps slog.Default() for a JSON-handler writing to
// the returned synchronised buffer for the duration of the test, so
// assertions can inspect emitted fields. The original default logger
// is restored on cleanup.
func withCapturedLogs(t *testing.T) *threadSafeBuffer {
	t.Helper()
	buf := &threadSafeBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func TestServe_LogsCarryRequestID(t *testing.T) {
	logs := withCapturedLogs(t)

	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: out, mu: outMu}

	// Handler also emits a log line via the request-scoped logger; we
	// assert the same rid shows up there as well as in the start/end
	// pair from Serve itself.
	handler := func(ctx context.Context, _ string, _ json.RawMessage) (any, *rpcError) {
		LoggerFrom(ctx).Debug("handler ran")
		return map[string]any{"ok": true}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, in, syncWriter, handler) }()

	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":42,"method":"trace_me"}` + "\n"))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		outMu.Lock()
		body := out.String()
		outMu.Unlock()
		if strings.Contains(body, `"ok":true`) {
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

	var (
		startRid, endRid, handlerRid string
		sawMethod                    bool
		sawDurMs                     bool
	)
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		rid, _ := rec["rid"].(string)
		if rec["method"] == "trace_me" {
			sawMethod = true
		}
		switch rec["msg"] {
		case "rpc start":
			startRid = rid
		case "rpc end":
			endRid = rid
			if _, ok := rec["dur_ms"]; ok {
				sawDurMs = true
			}
		case "handler ran":
			handlerRid = rid
		}
	}
	if startRid == "" {
		t.Fatalf("expected rpc start log line with non-empty rid, logs=%s", raw)
	}
	if len(startRid) != 8 {
		t.Fatalf("rid expected to be 8 hex chars, got %q", startRid)
	}
	if endRid != startRid {
		t.Fatalf("rpc end rid %q did not match rpc start rid %q", endRid, startRid)
	}
	if handlerRid != startRid {
		t.Fatalf("handler rid %q did not match rpc start rid %q", handlerRid, startRid)
	}
	if !sawMethod {
		t.Fatalf("expected method=trace_me on log lines, logs=%s", raw)
	}
	if !sawDurMs {
		t.Fatalf("expected rpc end to carry dur_ms, logs=%s", raw)
	}
}

func TestServe_LogsRpcErrorWithRequestID(t *testing.T) {
	logs := withCapturedLogs(t)

	in := &threadSafeBuffer{}
	out := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: out, mu: outMu}

	handler := func(_ context.Context, _ string, _ json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{Code: -32000, Message: "boom"}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, in, syncWriter, handler) }()

	_, _ = in.Write([]byte(`{"jsonrpc":"2.0","id":7,"method":"explode"}` + "\n"))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		outMu.Lock()
		body := out.String()
		outMu.Unlock()
		if strings.Contains(body, "-32000") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	in.Close()
	<-done
	logs.Close()
	raw, _ := io.ReadAll(logs)

	var startRid, errRid string
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		rid, _ := rec["rid"].(string)
		switch rec["msg"] {
		case "rpc start":
			startRid = rid
		case "rpc error":
			errRid = rid
		}
	}
	if startRid == "" || errRid == "" {
		t.Fatalf("missing start or error log: logs=%s", raw)
	}
	if errRid != startRid {
		t.Fatalf("error rid %q did not match start rid %q", errRid, startRid)
	}
}
