package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// idleReaperMaxInterval caps how often the reaper goroutine wakes up to
// scan for stale sessions. The configured timeout/4 is usually a good
// cadence — fast enough that a session is reaped within a quarter of
// the configured idle window, slow enough to stay in the noise of a
// long-running server. We also clamp at 30s so a very large timeout
// (say 24h) does not park the goroutine for hours and let the operator
// see no activity from the reaper for half a day.
const idleReaperMaxInterval = 30 * time.Second

// KillFunc is the controller-level "kill this session by name" hook the
// reaper invokes when it decides a session has been idle too long. The
// signature mirrors *tmuxctl.Controller.KillSession so callers can pass
// it as a method value (`ctl.KillSession`). Best-effort: errors are
// logged but never crash the reaper, so a single broken session does
// not strand the rest of the table.
type KillFunc func(ctx context.Context, name string) error

// Clock is the test seam that lets tests advance time deterministically
// without sleeping. Production code passes [time.Now] (or relies on
// [NewIdleReaper] which defaults to it); tests inject a fake clock so
// the table-driven cases in idle_test.go can exercise reap timing
// without flaky deadline races.
type Clock interface {
	Now() time.Time
}

// realClock is the default [Clock] the package uses when callers don't
// override it. Wrapping time.Now in a tiny struct keeps the field on
// IdleReaper a stable interface so test seams stay clean.
type realClock struct{}

// Now returns the current wall-clock time. This is the only call the
// reaper makes against the clock interface in production.
func (realClock) Now() time.Time { return time.Now() }

// IdleReaper tracks per-session "last activity" timestamps and kills
// sessions that have had no activity for at least `timeout`. Activity
// is recorded by the dispatcher via [IdleReaper.Touch] whenever a
// tools/call references a session name; methods that operate on the
// table as a whole (session_list, kill_all_sessions) are excluded by
// the dispatcher so they cannot keep dead sessions artificially alive.
//
// All methods are safe to call on a nil *IdleReaper (they are no-ops),
// which keeps the dispatcher free of branchy guards when the operator
// leaves -session-idle-timeout at the default 0 (disabled).
type IdleReaper struct {
	mu sync.Mutex
	// lastActivity records the most recent Touch wall-time per session.
	// Sessions with no entry are treated as "never seen", so the first
	// activity establishes the timer baseline.
	lastActivity map[string]time.Time
	// timeout is the idle cutoff. When elapsed >= timeout the reaper
	// kills the session on its next sweep.
	timeout time.Duration
	// kill is the controller hook the reaper calls on each victim.
	// It is wrapped to a per-call context so a wedged tmux command
	// cannot block the reaper goroutine forever.
	kill KillFunc
	// clock is the time source the reaper uses. Tests inject a fake;
	// production gets [realClock].
	clock Clock
	// logger is the slog handle reap events use. nil falls back to
	// slog.Default(); Serve injects the operator's logger via
	// SetLogger so reap diagnostics land on the same sink as the rest
	// of the server's structured logs without going through process-
	// global slog.SetDefault.
	logger *slog.Logger
}

// NewIdleReaper constructs a reaper for the given timeout. timeout <= 0
// returns nil — calling Touch / Reap on a nil reaper is a no-op, which
// matches the "flag default 0 disables the reaper entirely" contract
// declared by the CLI.
func NewIdleReaper(timeout time.Duration, kill KillFunc) *IdleReaper {
	if timeout <= 0 {
		return nil
	}
	return &IdleReaper{
		lastActivity: map[string]time.Time{},
		timeout:      timeout,
		kill:         kill,
		clock:        realClock{},
	}
}

// withClock swaps the reaper's clock for tests. It is unexported because
// production callers always see [realClock]; the seam exists so tests
// can drive time forward deterministically without sleeping in unit
// tests. Returns the same reaper for chaining.
func (r *IdleReaper) withClock(c Clock) *IdleReaper {
	if r == nil || c == nil {
		return r
	}
	r.clock = c
	return r
}

// SetLogger injects the slog handle the reaper uses for reap-event
// diagnostics. Safe on nil. Pass nil to revert to slog.Default().
//
// Call before [IdleReaper.Run] starts the goroutine — Serve() arranges
// this so the write happens-before any Reap reads logger via log(),
// which keeps the read on the hot path lock-free.
func (r *IdleReaper) SetLogger(lg *slog.Logger) {
	if r == nil {
		return
	}
	r.logger = lg
}

// log returns the configured logger or slog.Default() as fallback.
func (r *IdleReaper) log() *slog.Logger {
	if r != nil && r.logger != nil {
		return r.logger
	}
	return slog.Default()
}

// Touch records activity for the named session. Called by the
// dispatcher right before the handler runs for any tools/call that
// references a session — the reaper only needs the name, not the args.
// Empty names are ignored so a malformed call cannot poison the table
// with a phantom entry.
//
// Safe on a nil receiver (no-op), which is how the dispatcher avoids a
// branch when the reaper is disabled.
func (r *IdleReaper) Touch(name string) {
	if r == nil || name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastActivity[name] = r.clock.Now()
}

// Forget drops the per-session entry without killing the session. The
// dispatcher calls this when a session has already been removed
// (session_kill, kill_all_sessions) so the reaper does not try to kill
// a session that is already gone — and so the next session_create that
// happens to reuse the name starts with a fresh timer.
//
// Safe on a nil receiver.
func (r *IdleReaper) Forget(name string) {
	if r == nil || name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.lastActivity, name)
}

// Reap walks the activity table and kills any session whose last touch
// is at least `timeout` old relative to the supplied `now`. A separate
// `now` argument (rather than r.clock.Now()) lets tests advance time
// deterministically and exercise the boundary condition without racing
// the wall clock.
//
// Each kill runs against a per-session context bounded by `timeout` so
// a wedged tmux command cannot block the reaper goroutine for the rest
// of process lifetime. Errors are logged at info level (the same level
// the reap is logged at, so the pair stays correlated) and otherwise
// swallowed — best-effort is the explicit policy.
//
// On a nil receiver Reap is a no-op so the goroutine launcher in Serve
// can call it unconditionally.
func (r *IdleReaper) Reap(ctx context.Context, now time.Time) {
	if r == nil {
		return
	}
	// Snapshot the candidates under the mutex so we don't hold it
	// across the (potentially blocking) kill calls. Doing the kill
	// outside the lock keeps Touch / Forget responsive even when a
	// reap is in flight against a slow tmux server.
	type victim struct {
		name string
		idle time.Duration
	}
	r.mu.Lock()
	victims := make([]victim, 0, len(r.lastActivity))
	for name, ts := range r.lastActivity {
		elapsed := now.Sub(ts)
		if elapsed < r.timeout {
			continue
		}
		victims = append(victims, victim{name: name, idle: elapsed})
		// Drop the entry up front: if the kill below fails, the next
		// activity Touch will re-establish a fresh timer; if it
		// succeeds, the entry was already going away. Either way we
		// don't want to retry-kill a session every tick on a tmux
		// error path.
		delete(r.lastActivity, name)
	}
	r.mu.Unlock()
	lg := r.log()
	for _, v := range victims {
		lg.Info("reaping idle session", "session", v.name, "idle", v.idle)
		// Per-victim context so a wedged kill cannot block the next
		// one. The reaper's own ctx (Serve's lifetime) bounds the
		// outer loop; this just bounds an individual call.
		killCtx, cancel := context.WithTimeout(ctx, r.timeout)
		err := r.kill(killCtx, v.name)
		cancel()
		if err != nil {
			lg.Info("reaping idle session failed",
				"session", v.name, "err", err)
		}
	}
}

// Run is the long-lived goroutine the dispatcher launches when the
// reaper is enabled. It wakes every reapInterval(timeout) and calls
// Reap with the clock's current time, exiting when ctx is cancelled.
//
// On a nil receiver Run is a no-op so the dispatcher can launch it
// unconditionally without an outer "if reaper != nil" guard.
func (r *IdleReaper) Run(ctx context.Context) {
	if r == nil {
		return
	}
	interval := reapInterval(r.timeout)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Reap(ctx, r.clock.Now())
		}
	}
}

// reapInterval picks the cadence for the reaper goroutine. We sweep
// roughly four times per timeout window so a session is reaped within
// a quarter of the configured idle period, but cap at
// idleReaperMaxInterval so a multi-hour timeout doesn't park the
// goroutine indefinitely. Floor of 1s keeps a pathologically tiny
// timeout (e.g. 100ms) from busy-looping.
func reapInterval(timeout time.Duration) time.Duration {
	d := timeout / 4
	if d > idleReaperMaxInterval {
		d = idleReaperMaxInterval
	}
	if d < time.Second {
		d = time.Second
	}
	return d
}

// sessionFromArgs returns the session name an activity Touch should
// register against, given a `tools/call` method and its raw arguments.
// It returns "" when the method is not session-bearing OR when the
// extraction fails (malformed args, missing field, …) — the dispatcher
// treats "" as "skip Touch" so there is no risk of a phantom entry.
//
// The mapping mirrors the spec:
//   - send_keys / capture / wait_for_stable / wait_for_text / resize /
//     snapshot_diff / send_signal / list_panes:
//     {"session": "..."}
//   - session_create / session_kill / session_describe / session_rename:
//     {"name": "..."}
//   - pane_select / clear_history: {"target": "session:window.pane"} —
//     split on ":" to recover the session name.
//   - session_list / kill_all_sessions: explicitly excluded so they
//     can never extend an idle session's lifetime.
func sessionFromArgs(method string, args json.RawMessage) string {
	switch method {
	case "session_list", "kill_all_sessions":
		return ""
	case "session_create", "session_kill", "session_describe", "session_rename":
		var probe struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(args, &probe); err != nil {
			return ""
		}
		return probe.Name
	case "pane_select", "clear_history":
		var probe struct {
			Target string `json:"target"`
		}
		if err := json.Unmarshal(args, &probe); err != nil {
			return ""
		}
		name, _, _ := strings.Cut(probe.Target, ":")
		return name
	default:
		var probe struct {
			Session string `json:"session"`
		}
		if err := json.Unmarshal(args, &probe); err != nil {
			return ""
		}
		return probe.Session
	}
}
