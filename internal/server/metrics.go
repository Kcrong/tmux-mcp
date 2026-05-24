package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
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
		slog.Warn("metrics: ListSessions failed", "err", err)
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
// [prometheus.Gatherer] at /metrics, and starts serving on a private
// goroutine. The actual bound address is exposed via [MetricsServer.Addr]
// so tests can use ":0" / "127.0.0.1:0" and discover the kernel-chosen
// port.
//
// Two non-obvious choices:
//   - We bind eagerly (net.Listen) before returning so configuration
//     errors (port already in use, address malformed) surface as a
//     startup failure instead of a half-running goroutine that nobody
//     notices.
//   - The handler is constructed against the supplied gatherer, not
//     prometheus.DefaultGatherer, so callers can inject a private
//     registry for tests without leaking metrics into the global default.
func NewMetricsServer(addr string, gatherer prometheus.Gatherer) (*MetricsServer, error) {
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
			slog.Error("metrics server failed", "err", serr)
		}
	}()
	return &MetricsServer{addr: ln.Addr().String(), server: srv}, nil
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
