package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// httpGet is the shared helper every /healthz assertion uses: it issues
// a GET against the given URL, fails the test on transport error, and
// returns (status, body). Centralising the boilerplate keeps the
// per-case assertions short and forces a real HTTP round-trip on every
// test (rather than calling the handler in-process) so we exercise the
// mux + listener exactly the way k8s and curl will.
func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// startMetricsServer is the per-test fixture that binds 127.0.0.1:0,
// returns the bound URL prefix and the *Metrics handle so assertions
// can flip Healthy() between requests, and wires Shutdown into
// t.Cleanup. We deliberately use 127.0.0.1:0 instead of ":0" so the
// listener never accidentally binds to all interfaces during a test
// run.
func startMetricsServer(t *testing.T) (urlBase string, m *Metrics) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m = NewMetrics(reg)
	srv, err := NewMetricsServer("127.0.0.1:0", reg, m)
	if err != nil {
		t.Fatalf("NewMetricsServer: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})
	return "http://" + srv.Addr(), m
}

// TestHealthz_BeforeMarkHealthy pins the readiness contract: until
// main() calls MarkHealthy after the startup probe, /healthz must
// return 503 with the exact "unhealthy\n" body. k8s readiness gates
// rely on this — a freshly-bound listener on a slow tmux env should
// not yet be considered ready.
func TestHealthz_BeforeMarkHealthy(t *testing.T) {
	t.Parallel()
	base, _ := startMetricsServer(t)

	status, body := httpGet(t, base+"/healthz")
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", status, http.StatusServiceUnavailable)
	}
	if body != "unhealthy\n" {
		t.Errorf("body = %q, want %q", body, "unhealthy\n")
	}
}

// TestHealthz_AfterMarkHealthy is the post-probe path: once
// MarkHealthy fires, every subsequent GET /healthz returns 200 with
// "ok\n". MarkHealthy is one-way by design (no UnmarkHealthy), so this
// test also locks down the "stays healthy for the lifetime of the
// listener" invariant via two consecutive requests.
func TestHealthz_AfterMarkHealthy(t *testing.T) {
	t.Parallel()
	base, m := startMetricsServer(t)
	m.MarkHealthy()

	for i := 0; i < 2; i++ {
		status, body := httpGet(t, base+"/healthz")
		if status != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, status)
		}
		if body != "ok\n" {
			t.Errorf("request %d: body = %q, want %q", i, body, "ok\n")
		}
	}
}

// TestHealthz_NonGetReturns405 covers the contract that /healthz only
// answers GET. Anything else (POST, PUT, DELETE, …) returns 405 with
// an Allow: GET header so a curious operator running `curl -X POST`
// sees a structured rejection instead of a confusing 200/503. We
// exercise POST as the canonical "wrong method" probe — it is what
// healthcheckers occasionally fall back to when GET is rate-limited
// upstream and we want to fail loudly rather than silently succeed.
func TestHealthz_NonGetReturns405(t *testing.T) {
	t.Parallel()
	base, m := startMetricsServer(t)
	// Mark healthy so a regression that ignores method-checking would
	// produce 200 — we want to assert "method beats health", not
	// "health beats method".
	m.MarkHealthy()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(base+"/healthz", "text/plain", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
	if got := resp.Header.Get("Allow"); got != http.MethodGet {
		t.Errorf("Allow header = %q, want %q", got, http.MethodGet)
	}
}

// TestHealthz_CoexistsWithMetrics is the "both routes share one
// listener" assertion. After MarkHealthy, a single server must serve
// both GET /healthz and GET /metrics over the same socket. This is
// the property that lets operators run a single -metrics-addr port
// behind a k8s service / load balancer and reuse it for liveness +
// scrape.
func TestHealthz_CoexistsWithMetrics(t *testing.T) {
	t.Parallel()
	base, m := startMetricsServer(t)
	// Drive a synthetic observation so /metrics has data to expose
	// alongside /healthz.
	m.observeToolCall("send_keys", 1*time.Millisecond, nil)
	m.MarkHealthy()

	// /healthz path
	status, body := httpGet(t, base+"/healthz")
	if status != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", status)
	}
	if body != "ok\n" {
		t.Errorf("/healthz body = %q, want %q", body, "ok\n")
	}

	// /metrics path on the same listener
	mStatus, mBody := httpGet(t, base+"/metrics")
	if mStatus != http.StatusOK {
		t.Errorf("/metrics status = %d, want 200", mStatus)
	}
	if !strings.Contains(mBody, "tmuxmcp_tools_call_total") {
		t.Errorf("/metrics body missing counter; got:\n%s", mBody)
	}
}

// TestMetrics_HealthyTransition is the unit-level lockdown for the
// Healthy()/MarkHealthy() pair without an HTTP server in the loop:
// the flag starts false, MarkHealthy flips it to true, and a nil
// receiver reports false (so the metrics-disabled fast path doesn't
// blow up if a future caller forgets the nil-guard).
func TestMetrics_HealthyTransition(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	if m.Healthy() {
		t.Fatal("Healthy() = true before MarkHealthy, want false")
	}
	m.MarkHealthy()
	if !m.Healthy() {
		t.Fatal("Healthy() = false after MarkHealthy, want true")
	}
	// MarkHealthy is idempotent — a second call must not panic and
	// must leave the flag set.
	m.MarkHealthy()
	if !m.Healthy() {
		t.Fatal("Healthy() = false after second MarkHealthy, want true")
	}

	// Nil receiver: must report false and not panic.
	var nilM *Metrics
	if nilM.Healthy() {
		t.Fatal("nil-receiver Healthy() = true, want false")
	}
	nilM.MarkHealthy() // no panic
}
