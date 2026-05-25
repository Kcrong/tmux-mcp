package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	nethttppprof "net/http/pprof"
	runtimepprof "runtime/pprof"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MetricsNamespace is the prefix every Prometheus metric emitted by this
// package shares. Pinning it as a constant means a future rename ripples
// through every constructor in one place rather than scattered string
// literals.
const MetricsNamespace = "tmuxmcp"

// callDurationBuckets is the histogram bucket layout for tool-call
// latency. The range covers everything from "near-zero local capture"
// (1 ms) up to "client-perceptible stall" (10 s) on a roughly
// power-of-two cadence so dashboards see useful resolution at every
// order of magnitude without exploding cardinality.
var callDurationBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// Metrics is the optional Prometheus exporter handle. It owns the three
// metric series the server publishes (tool-call counter, tool-call
// duration histogram, active-sessions gauge) and is registered against
// a caller-supplied prometheus.Registerer so tests can use a private
// registry instead of the global default.
//
// All recording methods are safe to call on a nil receiver: when the
// operator leaves -metrics-addr empty no Metrics is constructed and the
// dispatcher's hooks become no-ops, keeping the metrics-disabled path
// branch-free.
type Metrics struct {
	callTotal      *prometheus.CounterVec
	callDuration   *prometheus.HistogramVec
	sessionsActive prometheus.Gauge
	// healthy flips to true exactly once, when main() finishes its
	// one-shot startup probe ([Metrics.MarkHealthy]). The /healthz
	// handler reads it on every request so a slow tmux env shows up as
	// 503 Service Unavailable until the probe completes — k8s readiness
	// gates and load balancers can wait on that signal instead of
	// declaring the pod ready the moment the listener binds.
	healthy atomic.Bool
	// logger is the slog handle poller / exporter diagnostics use.
	// nil falls back to slog.Default(); Serve injects the operator's
	// logger via SetLogger so failures land on the same sink as the
	// rest of the server's structured logs without going through
	// process-global slog.SetDefault.
	logger *slog.Logger
}

// NewMetrics constructs a [*Metrics] and registers its collectors against
// reg. Passing a private registry is the supported test pattern — the
// production main() passes a fresh prometheus.Registry so /metrics on
// the exporter HTTP server scrapes process / Go collectors alongside
// ours without polluting prometheus.DefaultRegisterer.
//
// MustRegister is intentional: a registration collision (duplicate
// metric name in the same process) is a programming error, not a
// runtime condition we want to recover from.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		callTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: MetricsNamespace,
				Subsystem: "tools",
				Name:      "call_total",
				Help:      "Total number of MCP tools/call invocations, labelled by tool name and ok/error result.",
			},
			[]string{"tool", "result"},
		),
		callDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: MetricsNamespace,
				Subsystem: "tools",
				Name:      "call_duration_seconds",
				Help:      "Wall-clock duration of MCP tools/call invocations, labelled by tool name.",
				Buckets:   callDurationBuckets,
			},
			[]string{"tool"},
		),
		sessionsActive: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: MetricsNamespace,
				Name:      "sessions_active",
				Help:      "Current number of tmux sessions tracked by this server, refreshed by the metrics poller.",
			},
		),
	}
	reg.MustRegister(m.callTotal, m.callDuration, m.sessionsActive)
	return m
}

// SetLogger injects the slog handle the poller / exporter use for
// diagnostic logs. Safe on nil. Pass nil to revert to slog.Default().
//
// Call before [Metrics.RunSessionsPoller] starts the goroutine — Serve()
// arranges this so the write happens-before any pollSessionsOnce reads
// logger via log(), keeping the read on the hot path lock-free.
func (m *Metrics) SetLogger(lg *slog.Logger) {
	if m == nil {
		return
	}
	m.logger = lg
}

// log returns the configured logger or slog.Default() as fallback.
func (m *Metrics) log() *slog.Logger {
	if m != nil && m.logger != nil {
		return m.logger
	}
	return slog.Default()
}

// observeToolCall records one tools/call observation: it bumps the
// labelled counter and the duration histogram. result is normalised to
// "ok" or "error" before being used as a label so we never explode the
// label cardinality with handler-specific strings.
//
// A nil receiver is a no-op — the dispatcher invokes this
// unconditionally and the metrics-disabled fast-path is "Metrics is
// nil → branch-free skip".
func (m *Metrics) observeToolCall(tool string, dur time.Duration, rerr *rpcError) {
	if m == nil {
		return
	}
	result := "ok"
	if rerr != nil {
		result = "error"
	}
	m.callTotal.WithLabelValues(tool, result).Inc()
	m.callDuration.WithLabelValues(tool).Observe(dur.Seconds())
}

// SetSessionsActive sets the gauge to the supplied count. The poller
// goroutine ([Metrics.RunSessionsPoller]) is the production caller, but
// tests use this directly to assert gauge wiring without spinning up a
// real tmux Controller. Safe on a nil receiver.
func (m *Metrics) SetSessionsActive(n int) {
	if m == nil {
		return
	}
	m.sessionsActive.Set(float64(n))
}

// MarkHealthy flips the readiness flag observed by the /healthz handler.
// main() calls this exactly once after its one-shot startup probe
// ([tmuxctl.ProbeVersion]) succeeds, so a slow tmux env keeps /healthz at
// 503 until the binary has actually proven it can talk to tmux. There is
// no inverse — once a process is healthy we stay healthy for the
// lifetime of the listener; per-request liveness checks belong on a
// different surface. Safe on a nil receiver.
func (m *Metrics) MarkHealthy() {
	if m == nil {
		return
	}
	m.healthy.Store(true)
}

// Healthy reports whether [Metrics.MarkHealthy] has fired. The /healthz
// handler is the production caller; tests use it to assert the
// before/after transition without poking the atomic directly. A nil
// receiver reports false so callers can use the same handle in the
// metrics-disabled fast path.
func (m *Metrics) Healthy() bool {
	if m == nil {
		return false
	}
	return m.healthy.Load()
}

// SessionLister narrows the [tmuxctl.Controller] surface used by the
// poller to the single method we need, so tests can substitute a stub
// without standing up a real tmux server. The exported name lets
// callers in cmd/ pass a Controller (which satisfies the interface)
// without referencing tmuxctl from inside the server package.
type SessionLister interface {
	ListSessions(ctx context.Context) ([]string, error)
}

// RunSessionsPoller refreshes the sessions_active gauge every interval
// until ctx is cancelled. Each tick lists sessions through the supplied
// [SessionLister] and updates the gauge with len(names). Errors are
// logged at warn level (one line per failure) but never propagated —
// metrics are best-effort by design and a transient tmux blip must not
// take the exporter down.
//
// interval <= 0 falls back to a 5-second cadence to match the tuning
// the README recommends. Safe on a nil receiver: when metrics are
// disabled the goroutine never starts.
func (m *Metrics) RunSessionsPoller(ctx context.Context, ctl SessionLister, interval time.Duration) {
	if m == nil || ctl == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// Prime the gauge immediately so /metrics doesn't return a zero
	// for the entire first interval after the exporter starts.
	m.pollSessionsOnce(ctx, ctl)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.pollSessionsOnce(ctx, ctl)
		}
	}
}

// pollSessionsOnce performs a single ListSessions call and updates the
// gauge. Failures are logged but swallowed so a transient tmux error
// can't crash the exporter loop.
func (m *Metrics) pollSessionsOnce(ctx context.Context, ctl SessionLister) {
	names, err := ctl.ListSessions(ctx)
	if err != nil {
		// Cancellation during shutdown isn't a failure — the parent
		// context just fired. Don't pollute the log with a warning
		// about it.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		m.log().Warn("metrics: ListSessions failed", "err", err)
		return
	}
	m.sessionsActive.Set(float64(len(names)))
}

// WithMetrics installs an optional [*Metrics] handle on the dispatcher.
// When non-nil, every tools/call records its tool name, result, and
// duration into the supplied collectors. A nil handle keeps the
// metrics-disabled fast-path (no Inc/Observe, no allocations).
func WithMetrics(m *Metrics) ServeOption {
	return func(c *serveConfig) { c.metrics = m }
}

// MetricsServer wraps the HTTP server that exposes /metrics so main()
// can hand its Shutdown back to a defer in run() without leaking
// implementation details (the chosen mux, the listener, the registry).
type MetricsServer struct {
	addr   string
	server *http.Server
}

// NewMetricsServer binds a TCP listener on addr, mounts the supplied
// [prometheus.Gatherer] at /metrics plus a /healthz handler driven by
// the supplied [*Metrics] readiness flag, and starts serving on a
// private goroutine. The actual bound address is exposed via
// [MetricsServer.Addr] so tests can use ":0" / "127.0.0.1:0" and
// discover the kernel-chosen port.
//
// Co-locating /metrics and /healthz on the same listener avoids opening
// a second port for k8s liveness/readiness or load balancer health
// checks while keeping both observability surfaces under a single
// shutdown handle.
//
// Two non-obvious choices:
//   - We bind eagerly (net.Listen) before returning so configuration
//     errors (port already in use, address malformed) surface as a
//     startup failure instead of a half-running goroutine that nobody
//     notices.
//   - The handler is constructed against the supplied gatherer, not
//     prometheus.DefaultGatherer, so callers can inject a private
//     registry for tests without leaking metrics into the global default.
//
// m may be nil — the /healthz handler is still mounted, but it stays at
// 503 forever (since there is no flag to flip). main() always passes
// the same *Metrics it constructed for the dispatcher hooks, so the
// production path always has a real readiness signal.
//
// enablePprof opts in to the runtime profiling endpoints under
// /debug/pprof/* on this same listener. It is wired to the operator's
// -pprof flag and defaults to disabled because heap / goroutine
// profiles can leak sensitive in-memory state. The handlers are
// registered explicitly on the private mux so the operator's
// -metrics-addr is the only network surface that ever exposes them;
// see [mountPprof] for the security rationale.
func NewMetricsServer(addr string, gatherer prometheus.Gatherer, m *Metrics, enablePprof bool) (*MetricsServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{
		// EnableOpenMetrics keeps the response compatible with newer
		// Prometheus + OpenTelemetry scrapers without forcing the
		// classic text format on operators who don't ask for it.
		EnableOpenMetrics: true,
	}))
	mux.Handle("/healthz", healthzHandler(m))
	if enablePprof {
		mountPprof(mux)
	}
	srv := &http.Server{
		Handler: mux,
		// ReadHeaderTimeout protects the exporter from slowloris-style
		// header reads. /metrics requests are tiny GETs, so anything
		// over a few seconds is a misbehaving client.
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		// Serve consumes the listener until Shutdown closes it; the
		// errServerClosed path is the clean teardown signal and is
		// not worth logging.
		if serr := srv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			m.log().Error("metrics server failed", "err", serr)
		}
	}()
	return &MetricsServer{addr: ln.Addr().String(), server: srv}, nil
}

// mountPprof registers the net/http/pprof handlers explicitly on the
// supplied private mux. The verbose enumeration is the operative
// security choice: it scopes the routes to the metrics listener (the
// only network surface the operator opted in to via -metrics-addr) so
// a future contributor adding a second http.Server with Handler nil
// cannot accidentally inherit /debug/pprof/* via http.DefaultServeMux.
//
// Caveat about net/http/pprof: importing the package — in any form,
// including aliased — runs its init() which unconditionally registers
// pprof handlers on http.DefaultServeMux. There is no Go-level
// workaround. The pollution is harmless in tmux-mcp because every
// http.Server we construct sets Handler explicitly, so the fallback
// to DefaultServeMux never fires. The regression test
// TestPprof_MetricsListenerNotPollutedWhenDisabled pins that property
// by issuing a real GET against the metrics listener with pprof off
// and asserting 404 — that is the actual security invariant
// (DefaultServeMux pollution doesn't matter as long as nothing serves
// it).
//
// The set mirrors what net/http/pprof installs on its own:
//   - /debug/pprof/         (Index — the HTML browser)
//   - /debug/pprof/cmdline  (process command line)
//   - /debug/pprof/profile  (CPU profile, default 30s sample)
//   - /debug/pprof/symbol   (symbol resolver)
//   - /debug/pprof/trace    (execution tracer)
//   - /debug/pprof/<name>   (one entry per [runtime/pprof.Lookup]
//     profile: goroutine, heap, allocs, block, mutex, threadcreate)
//
// The runtime-profile loop calls [runtime/pprof.Lookup] before
// mounting so a typo or a name dropped in a future Go release
// surfaces as "route not registered" rather than serving a 200 with
// an empty body.
func mountPprof(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", nethttppprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", nethttppprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", nethttppprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", nethttppprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", nethttppprof.Trace)
	for _, name := range []string{
		"goroutine", "heap", "allocs", "block", "mutex", "threadcreate",
	} {
		// Lookup before mount: nethttppprof.Handler(name) is willing
		// to register a handler for any string and 404 at request
		// time. Failing fast here keeps the surface honest — if a
		// future Go release renames or drops a profile, /debug/pprof/
		// just won't list it instead of serving it as a broken
		// endpoint.
		if runtimepprof.Lookup(name) == nil {
			continue
		}
		mux.Handle("/debug/pprof/"+name, nethttppprof.Handler(name))
	}
}

// healthzHandler returns the readiness handler installed at /healthz.
// It is intentionally tiny so it stays cheap on every k8s probe tick:
// no allocations beyond the constant body slice, no logging, no
// dependency on the slog handler. Behaviour:
//
//   - method != GET → 405 Method Not Allowed (with an Allow: GET hint
//     so a curious operator running `curl -X POST` sees the contract).
//   - GET before [Metrics.MarkHealthy] → 503 Service Unavailable +
//     "unhealthy\n", so a slow tmux env doesn't trick a load balancer
//     into routing traffic to a not-yet-ready pod.
//   - GET after MarkHealthy → 200 OK + "ok\n".
//
// The bodies are deliberately short, plain-text, and trailed with a
// newline so `curl -s host/healthz` looks right when piped into a shell
// test. No JSON envelope — k8s liveness/readiness probes only inspect
// the status code, and a human eyeballing the endpoint gets a clean
// one-word answer.
func healthzHandler(m *Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if m.Healthy() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unhealthy\n"))
	})
}

// Addr returns the actual address the listener is bound to. Useful for
// tests that pass ":0" or "127.0.0.1:0" and need to discover the
// kernel-chosen port before issuing a request.
func (s *MetricsServer) Addr() string {
	if s == nil {
		return ""
	}
	return s.addr
}

// Shutdown gracefully drains in-flight requests within ctx's deadline
// and closes the listener. Safe on a nil receiver. The error is the
// underlying http.Server.Shutdown error so callers can log it without
// re-wrapping.
func (s *MetricsServer) Shutdown(ctx context.Context) error {
	if s == nil || s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}
