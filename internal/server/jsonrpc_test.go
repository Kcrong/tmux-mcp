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
