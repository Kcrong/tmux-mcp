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
	return startMetricsServerWithPprof(t, false)
}

// startMetricsServerWithPprof is the pprof-aware fixture. The two
// codepaths share construction so the existing /metrics + /healthz
// assertions stay byte-identical when pprof is off, and the pprof
// tests can flip the bit without duplicating the bind / cleanup
// boilerplate.
func startMetricsServerWithPprof(t *testing.T, enablePprof bool) (urlBase string, m *Metrics) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m = NewMetrics(reg)
	srv, err := NewMetricsServer("127.0.0.1:0", reg, m, enablePprof)
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
	t.Cleanup(func() { _ = resp.Body.Close() })
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

// TestPprof_EnabledServesIndex is the smoke test for opt-in pprof: with
// enablePprof=true, GET /debug/pprof/ returns the stdlib's HTML index
// (recognisable by the "Profile Descriptions" heading
// net/http/pprof.Index renders). Asserting on the rendered body — not
// just the status — keeps a regression that swaps in a 200-but-empty
// handler honest.
func TestPprof_EnabledServesIndex(t *testing.T) {
	t.Parallel()
	base, _ := startMetricsServerWithPprof(t, true)

	status, body := httpGet(t, base+"/debug/pprof/")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", status, body)
	}
	// "Profile Descriptions" is the section header net/http/pprof.Index
	// emits at the bottom of its HTML page; it has been stable across
	// every supported Go release. Matching on it (rather than a more
	// brittle title string) keeps the assertion forward-compatible while
	// still locking down "this is the real pprof index, not an empty
	// 200".
	if !strings.Contains(body, "Profile Descriptions") {
		t.Errorf("body missing pprof index marker; got:\n%s", body)
	}
}

// TestPprof_EnabledServesHeapProfile asserts the runtime-driven path:
// /debug/pprof/heap streams a non-empty body via net/http/pprof.Handler
// ("heap") so an operator can `curl host/debug/pprof/heap > heap.pb.gz`
// and feed it to `go tool pprof`. We deliberately do not parse the
// gzip-protobuf payload — that's covered by stdlib tests and would
// brittle-couple us to its on-wire format. A non-zero body length is
// enough to prove the handler ran and the runtime emitted a snapshot.
func TestPprof_EnabledServesHeapProfile(t *testing.T) {
	t.Parallel()
	base, _ := startMetricsServerWithPprof(t, true)

	status, body := httpGet(t, base+"/debug/pprof/heap")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if len(body) == 0 {
		t.Errorf("heap profile body was empty, expected non-empty payload")
	}
}

// TestPprof_DisabledReturns404 is the regression guard for the default
// (opt-out) policy: with enablePprof=false, none of the /debug/pprof/*
// routes are mounted, so a request returns 404 from the mux. This locks
// down the contract that pprof exposure requires explicit -pprof
// opt-in even if -metrics-addr is set, so an operator who forgets to
// pass -pprof never accidentally exposes a heap profile on a
// loopback-only metrics port. We probe both /debug/pprof/ (Index) and
// /debug/pprof/heap so a future regression that mounts only the
// runtime profiles (or only the index) trips a test.
func TestPprof_DisabledReturns404(t *testing.T) {
	t.Parallel()
	base, _ := startMetricsServerWithPprof(t, false)

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap"} {
		status, _ := httpGet(t, base+path)
		if status != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, status)
		}
	}
}

// TestPprof_CoexistsWithMetricsAndHealthz pins the "single listener,
// three surfaces" contract: with enablePprof=true the metrics
// listener still serves /metrics and /healthz exactly the way it did
// before this flag existed, plus the new /debug/pprof/* tree. This
// guards against a regression that mounts pprof on top of one of the
// existing routes (mux.Handle would panic, but a future refactor
// could plausibly use HandleFunc with overlapping prefixes and lose
// /metrics under /debug/pprof/ patterns).
func TestPprof_CoexistsWithMetricsAndHealthz(t *testing.T) {
	t.Parallel()
	base, m := startMetricsServerWithPprof(t, true)
	// Drive a synthetic observation so /metrics has data to expose.
	m.observeToolCall("send_keys", 1*time.Millisecond, nil)
	m.MarkHealthy()

	// /metrics
	mStatus, mBody := httpGet(t, base+"/metrics")
	if mStatus != http.StatusOK {
		t.Errorf("/metrics status = %d, want 200", mStatus)
	}
	if !strings.Contains(mBody, "tmuxmcp_tools_call_total") {
		t.Errorf("/metrics body missing counter; got:\n%s", mBody)
	}

	// /healthz
	hStatus, hBody := httpGet(t, base+"/healthz")
	if hStatus != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", hStatus)
	}
	if hBody != "ok\n" {
		t.Errorf("/healthz body = %q, want %q", hBody, "ok\n")
	}

	// /debug/pprof/
	pStatus, pBody := httpGet(t, base+"/debug/pprof/")
	if pStatus != http.StatusOK {
		t.Errorf("/debug/pprof/ status = %d, want 200", pStatus)
	}
	if !strings.Contains(pBody, "Profile Descriptions") {
		t.Errorf("/debug/pprof/ body missing pprof marker; got:\n%s", pBody)
	}
}

// TestPprof_MetricsListenerNotPollutedWhenDisabled is the
// security-property regression guard.
//
// Background: every form of `import "net/http/pprof"` (blank, aliased,
// or named) triggers that package's init() function, which
// unconditionally registers pprof handlers on http.DefaultServeMux.
// There is no Go-level way to import the package without that side
// effect. The actual security property we care about is therefore not
// "DefaultServeMux is empty" — it cannot be — but the stronger
// invariant that *no http.Server in tmux-mcp serves DefaultServeMux
// as its handler*. http.Server only falls back to DefaultServeMux
// when Handler is nil, so as long as every Server we construct sets
// its own Handler, the pollution is unreachable over the network.
//
// We pin that invariant by issuing a real GET against the metrics
// listener (which uses a private mux). With pprof disabled, the
// listener must reject /debug/pprof/ as 404 — proving that even
// though net/http/pprof.init() has polluted DefaultServeMux at
// process start, the pollution is unreachable through our HTTP
// surface. A future regression that swapped our private mux for
// http.DefaultServeMux (or added a second http.Server with Handler
// nil) would surface as a 200 here and trip the test.
//
// The TestPprof_DisabledReturns404 case above already exercises the
// 404 path at a higher level; this case is the explicit
// security-rationale lockdown so the intent doesn't disappear if a
// future refactor reorganises the test layout.
func TestPprof_MetricsListenerNotPollutedWhenDisabled(t *testing.T) {
	t.Parallel()
	base, _ := startMetricsServerWithPprof(t, false)

	// The /debug/pprof/* tree must be unreachable through our
	// listener. Even though net/http/pprof.init() has registered
	// these routes on http.DefaultServeMux at process start, our
	// listener serves a private *http.ServeMux and must never expose
	// them.
	for _, path := range []string{
		"/debug/pprof/",
		"/debug/pprof/heap",
		"/debug/pprof/cmdline",
	} {
		status, _ := httpGet(t, base+path)
		if status != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404 — metrics listener must not "+
				"expose pprof unless -pprof is set", path, status)
		}
	}
}
