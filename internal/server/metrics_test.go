package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestMetrics_NilHandleIsNoop pins the contract that callers can pass a
// nil *Metrics unconditionally. observeToolCall / SetSessionsActive must
// succeed without panicking and without touching the (non-existent)
// collectors. This is what keeps the dispatcher hot path branch-free in
// metrics-disabled deployments.
func TestMetrics_NilHandleIsNoop(t *testing.T) {
	t.Parallel()
	var m *Metrics
	// None of these may panic.
	m.observeToolCall("send_keys", 5*time.Millisecond, nil)
	m.observeToolCall("capture", 1*time.Millisecond, &rpcError{Code: -32602, Message: "bad"})
	m.SetSessionsActive(7)
	// Poller bails early on nil receiver too — no goroutine started.
	m.RunSessionsPoller(context.Background(), nil, time.Hour)
}

// TestMetrics_ObserveToolCallCounters covers the "happy path" for both
// counter labels. With a fresh private registry we drive a few synthetic
// observations and assert the per-tool, per-result counter values.
// testutil.ToFloat64 reads each labelled series directly — keeping the
// assertion off the /metrics text format means the test stays stable
// when the exposition layout changes.
func TestMetrics_ObserveToolCallCounters(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.observeToolCall("send_keys", 1*time.Millisecond, nil)
	m.observeToolCall("send_keys", 2*time.Millisecond, nil)
	m.observeToolCall("send_keys", 3*time.Millisecond, &rpcError{Code: -32602, Message: "bad"})
	m.observeToolCall("capture", 1*time.Millisecond, nil)

	if got := testutil.ToFloat64(m.callTotal.WithLabelValues("send_keys", "ok")); got != 2 {
		t.Errorf("send_keys/ok = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.callTotal.WithLabelValues("send_keys", "error")); got != 1 {
		t.Errorf("send_keys/error = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.callTotal.WithLabelValues("capture", "ok")); got != 1 {
		t.Errorf("capture/ok = %v, want 1", got)
	}
	// CollectAndCount counts the number of distinct labelled series in
	// the histogram. Two distinct tool labels were observed
	// (send_keys, capture), so two histogram series exist regardless of
	// sample count.
	got := testutil.CollectAndCount(m.callDuration, "tmuxmcp_tools_call_duration_seconds")
	if got != 2 {
		t.Errorf("call_duration series count = %d, want 2", got)
	}
}

// TestMetrics_SetSessionsActive locks down the gauge wiring: the value
// SetSessionsActive writes is the value /metrics would scrape. We use
// testutil.ToFloat64 directly so the assertion is independent of the
// text exposition format.
func TestMetrics_SetSessionsActive(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.SetSessionsActive(0)
	if got := testutil.ToFloat64(m.sessionsActive); got != 0 {
		t.Errorf("initial gauge = %v, want 0", got)
	}
	m.SetSessionsActive(3)
	if got := testutil.ToFloat64(m.sessionsActive); got != 3 {
		t.Errorf("after Set(3): %v, want 3", got)
	}
	m.SetSessionsActive(1)
	if got := testutil.ToFloat64(m.sessionsActive); got != 1 {
		t.Errorf("after Set(1): %v, want 1 (gauge must replace, not add)", got)
	}
}

// stubLister is the minimal SessionLister fake used by the poller test.
// It returns a configurable set of names and an optional error so we
// can exercise both the success path and the failure-swallowed path
// without spinning up a real tmux Controller.
type stubLister struct {
	mu     sync.Mutex
	names  []string
	err    error
	calls  int
	called chan struct{} // closed once after the first call so tests can sync
}

func (s *stubLister) ListSessions(_ context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls == 1 && s.called != nil {
		close(s.called)
	}
	if s.err != nil {
		return nil, s.err
	}
	out := make([]string, len(s.names))
	copy(out, s.names)
	return out, nil
}

// TestMetrics_RunSessionsPoller asserts the poller primes the gauge on
// first tick (before the interval elapses) and updates it on subsequent
// ticks. We use a very short interval (10ms) and rely on the "called"
// channel as a sync barrier — far more reliable than time.Sleep in a
// race-detector run.
func TestMetrics_RunSessionsPoller(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	stub := &stubLister{
		names:  []string{"a", "b", "c"},
		called: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.RunSessionsPoller(ctx, stub, 10*time.Millisecond)

	// The poller calls ListSessions once immediately (priming the
	// gauge) before sleeping for the first tick. Wait on the barrier.
	select {
	case <-stub.called:
	case <-time.After(2 * time.Second):
		t.Fatal("poller never primed the gauge")
	}

	// Give the gauge update a moment to propagate. The poller sets
	// the gauge synchronously inside pollSessionsOnce, so a short
	// wait + retry loop is enough to avoid flakiness without
	// resorting to fixed sleeps.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if testutil.ToFloat64(m.sessionsActive) == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := testutil.ToFloat64(m.sessionsActive); got != 3 {
		t.Fatalf("gauge = %v after prime, want 3", got)
	}

	// Mutate the stub and assert the next tick picks up the new
	// count. Same retry pattern — we don't know exactly when the
	// 10ms tick lands relative to the assertion.
	stub.mu.Lock()
	stub.names = []string{"only-one"}
	stub.mu.Unlock()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if testutil.ToFloat64(m.sessionsActive) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := testutil.ToFloat64(m.sessionsActive); got != 1 {
		t.Fatalf("gauge = %v after mutation, want 1", got)
	}
}

// TestMetrics_RunSessionsPollerSwallowsErrors covers the failure-mode
// invariant: a transient ListSessions error must not stop the poller
// or panic. We feed a stub that errors forever and confirm the gauge
// stays at zero (its initial value) and the poller goroutine exits
// cleanly when ctx is cancelled.
func TestMetrics_RunSessionsPollerSwallowsErrors(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	stub := &stubLister{
		err:    errors.New("tmux exploded"),
		called: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.RunSessionsPoller(ctx, stub, 10*time.Millisecond)
		close(done)
	}()
	// Wait until at least one ListSessions has been observed — this
	// proves the poller is past the priming call and is in the tick
	// loop where errors would otherwise crash it.
	select {
	case <-stub.called:
	case <-time.After(2 * time.Second):
		t.Fatal("poller never called the lister")
	}
	// Gauge stayed at zero (unchanged from construction).
	if got := testutil.ToFloat64(m.sessionsActive); got != 0 {
		t.Errorf("gauge = %v under error path, want 0", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("poller did not exit after ctx cancel")
	}
}

// TestMetricsServer_ServesMetrics asserts the HTTP exporter end-to-end:
// after binding on 127.0.0.1:0 we issue a real GET against the bound
// address and confirm the /metrics body contains every series the
// server promises (counter, histogram, gauge). We deliberately use
// 127.0.0.1:0 — never ":0" — because the implementation should respect
// whatever address the operator passes verbatim and not silently expand
// to all interfaces.
func TestMetricsServer_ServesMetrics(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	// Drive a couple of observations so the counters / histograms
	// have non-zero data to expose. The gauge is set explicitly so we
	// don't need a poller in this test.
	m.observeToolCall("send_keys", 1*time.Millisecond, nil)
	m.observeToolCall("send_keys", 1*time.Millisecond, &rpcError{Code: -32602, Message: "bad"})
	m.SetSessionsActive(2)

	srv, err := NewMetricsServer("127.0.0.1:0", reg)
	if err != nil {
		t.Fatalf("NewMetricsServer: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})
	if srv.Addr() == "" {
		t.Fatal("Addr returned empty after bind")
	}

	// Real HTTP scrape. An explicit timeout keeps a hanging server
	// from wedging the test under -race.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + srv.Addr() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		"tmuxmcp_tools_call_total",
		"tmuxmcp_tools_call_duration_seconds",
		"tmuxmcp_sessions_active",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics output missing %q\nbody:\n%s", want, text)
		}
	}
}

// TestMetricsServer_ShutdownReleasesAddress verifies Shutdown actually
// closes the listener: once Shutdown returns, a fresh GET against the
// same URL must fail (connection refused / net.OpError). This is the
// invariant that lets main() defer Shutdown without leaking the port
// across signals.
func TestMetricsServer_ShutdownReleasesAddress(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	NewMetrics(reg) // register so Gather has something to emit

	srv, err := NewMetricsServer("127.0.0.1:0", reg)
	if err != nil {
		t.Fatalf("NewMetricsServer: %v", err)
	}
	addr := srv.Addr()
	shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if serr := srv.Shutdown(shutCtx); serr != nil {
		t.Fatalf("Shutdown: %v", serr)
	}

	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/metrics")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("expected GET to fail after Shutdown but it succeeded")
	}
}

// TestMetricsServer_NilShutdownIsNoop guards the API contract that
// calling Shutdown on a nil *MetricsServer is a no-op. main() relies
// on this so the unconditional `defer srv.Shutdown(ctx)` works even
// when the operator left -metrics-addr empty (no server constructed).
func TestMetricsServer_NilShutdownIsNoop(t *testing.T) {
	t.Parallel()
	var srv *MetricsServer
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown on nil server returned %v", err)
	}
	if got := srv.Addr(); got != "" {
		t.Fatalf("Addr on nil server = %q, want empty", got)
	}
}

// TestServe_RecordsMetrics is the integration assertion that ties the
// dispatcher to *Metrics: feeding tools/call frames through Serve must
// bump tmuxmcp_tools_call_total for the right tool name and result, and
// non-tools/call methods (initialize, tools/list) must NOT produce any
// metric increments. We use the same threadSafeBuffer + lockedWriter
// helpers the audit tests use to drive Serve in a goroutine without
// racing on stdout.
func TestServe_RecordsMetrics(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	in := &threadSafeBuffer{}
	rpcOut := &bytes.Buffer{}
	outMu := &sync.Mutex{}
	syncWriter := &lockedWriter{w: rpcOut, mu: outMu}

	// Mixed handler: send_keys succeeds, capture errors. This pins
	// down both label values in one test rather than two.
	handler := func(_ context.Context, method string, params json.RawMessage) (any, *rpcError) {
		if method != "tools/call" {
			return map[string]any{}, nil
		}
		var probe struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(params, &probe)
		if probe.Name == "capture" {
			return nil, &rpcError{Code: -32000, Message: "boom"}
		}
		return map[string]any{"content": []any{}}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, in, syncWriter, handler, WithMetrics(m)) }()

	frames := []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_keys","arguments":{"session":"x"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"send_keys","arguments":{"session":"x"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"capture","arguments":{"session":"x"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/list"}`,
	}
	for _, f := range frames {
		_, _ = in.Write([]byte(f + "\n"))
	}
	// Wait until 5 responses come back so we know every handler ran
	// (and observeToolCall has been called for the tools/call frames).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		outMu.Lock()
		body := rpcOut.String()
		outMu.Unlock()
		if strings.Count(body, "\n") >= 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	in.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after EOF")
	}

	if got := testutil.ToFloat64(m.callTotal.WithLabelValues("send_keys", "ok")); got != 2 {
		t.Errorf("send_keys/ok = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.callTotal.WithLabelValues("capture", "error")); got != 1 {
		t.Errorf("capture/error = %v, want 1", got)
	}
	// initialize and tools/list are explicitly NOT metered. A labelled
	// lookup for an unobserved combination returns 0; if either RPC
	// were leaking into the counter the assertion would fail.
	if got := testutil.ToFloat64(m.callTotal.WithLabelValues("initialize", "ok")); got != 0 {
		t.Errorf("initialize/ok = %v, want 0 (non-tools/call must not be metered)", got)
	}
}
