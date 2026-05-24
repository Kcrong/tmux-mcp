package server

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeClock is a deterministic, monotonically-advanceable clock used by
// the IdleReaper tests. The reaper itself only reads the clock from
// Touch — Reap takes `now` as an explicit argument — so the table-driven
// tests below can drive elapsed-time scenarios without sleeping or
// racing wall time.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// newFakeClock pins a fake clock at the supplied start time. Tests use
// a fixed offset (Unix 1_700_000_000) for a stable, reproducible timeline.
func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

// Now returns the clock's current pinned time. Reads run under a mutex
// so concurrent Touches in the multi-session test don't race on the
// underlying time.Time field.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance bumps the clock forward by d. There's no rewind: tests that
// want a fresh baseline construct a new clock.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// recordingKill is the test-side stand-in for *tmuxctl.Controller.KillSession.
// It records every call (in the order it received them) so tests can
// assert exactly which sessions the reaper decided to terminate.
// errFor lets a test inject failures for specific session names so the
// best-effort log-and-continue path is exercised too.
type recordingKill struct {
	mu     sync.Mutex
	called []string
	errFor map[string]error // optional: error to return for a given session.
}

// newRecordingKill returns a fresh recorder. Tests that don't need to
// inject errors can leave errFor as the zero map (every call succeeds).
func newRecordingKill() *recordingKill {
	return &recordingKill{errFor: map[string]error{}}
}

// kill is the KillFunc the reaper invokes. It appends the session name
// (so we can assert order + exact set), then returns whatever the test
// stuffed into errFor for that name (default: nil).
func (r *recordingKill) kill(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.called = append(r.called, name)
	return r.errFor[name]
}

// names returns the recorded session names sorted alphabetically. We
// don't promise iteration order over the activity map, so order-tolerant
// equality is the only assertion shape that doesn't rely on map
// internals.
func (r *recordingKill) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.called))
	copy(out, r.called)
	sort.Strings(out)
	return out
}

// TestIdleReaper_NoOpWhenDisabled covers the contract documented on
// NewIdleReaper / WithSessionIdleTimeout: timeout <= 0 returns nil, and
// every method is a no-op on a nil receiver. Operators leaving the
// flag at the default 0 must pay no goroutine cost, no map allocation,
// and no kills.
func TestIdleReaper_NoOpWhenDisabled(t *testing.T) {
	t.Parallel()

	for _, d := range []time.Duration{0, -1, -time.Hour} {
		if r := NewIdleReaper(d, nil); r != nil {
			t.Fatalf("NewIdleReaper(%v): expected nil, got %p", d, r)
		}
	}

	// nil-receiver no-ops should not panic. We deliberately exercise
	// every public method so a future refactor that forgets the nil
	// guard fails loudly here instead of in production.
	var nilR *IdleReaper
	nilR.Touch("demo")
	nilR.Forget("demo")
	nilR.Reap(context.Background(), time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Run exits immediately on a cancelled ctx.
	nilR.Run(ctx)
}

// TestIdleReaper_TouchAndReapBelowTimeout asserts the basic boundary:
// a session touched < timeout ago must NOT be reaped. We pin the
// fake clock, Touch, advance by less than the timeout, then call Reap
// and assert the kill hook was never invoked.
func TestIdleReaper_TouchAndReapBelowTimeout(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	rec := newRecordingKill()
	r := NewIdleReaper(time.Minute, rec.kill).withClock(clock)

	r.Touch("demo")
	// 30s < 1m timeout → no reap.
	clock.Advance(30 * time.Second)
	r.Reap(context.Background(), clock.Now())

	if got := rec.names(); len(got) != 0 {
		t.Fatalf("expected zero kills, got %v", got)
	}
}

// TestIdleReaper_ReapsAfterTimeout is the matched positive case:
// crossing the timeout threshold by even one nanosecond is enough to
// trigger a reap. We use timeout+1ns so the test does not depend on
// any imprecise "comfortably past" margin.
func TestIdleReaper_ReapsAfterTimeout(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	rec := newRecordingKill()
	timeout := time.Minute
	r := NewIdleReaper(timeout, rec.kill).withClock(clock)

	r.Touch("demo")
	clock.Advance(timeout + time.Nanosecond)
	r.Reap(context.Background(), clock.Now())

	got := rec.names()
	if len(got) != 1 || got[0] != "demo" {
		t.Fatalf("expected exactly [demo], got %v", got)
	}
	// After a successful reap the entry must be gone so a stale Reap
	// call seconds later doesn't double-kill the same name. Touching
	// again should re-establish the entry.
	r.Reap(context.Background(), clock.Now().Add(timeout))
	if got := rec.names(); len(got) != 1 {
		t.Fatalf("entry should be cleared after first reap, got %v", got)
	}
}

// TestIdleReaper_OnlyIdleSessionsKilled verifies the per-session timer
// is independent: with one Touch on "alpha" and a later Touch on
// "beta", advancing past timeout from the first Touch must reap only
// "alpha". This pins down the contract that the reaper does NOT use
// a single global timer — each session has its own.
func TestIdleReaper_OnlyIdleSessionsKilled(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	rec := newRecordingKill()
	timeout := time.Minute
	r := NewIdleReaper(timeout, rec.kill).withClock(clock)

	r.Touch("alpha")
	// 30s later beta is touched. alpha is now 30s old, beta is 0s old.
	clock.Advance(30 * time.Second)
	r.Touch("beta")
	r.Touch("gamma") // gamma touched the same instant as beta.
	// Advance another 31s: alpha = 61s (>1m), beta/gamma = 31s (<1m).
	clock.Advance(31 * time.Second)
	r.Reap(context.Background(), clock.Now())

	got := rec.names()
	if len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("expected only [alpha] reaped, got %v", got)
	}
}

// TestIdleReaper_TouchResetsTimer confirms the Touch-resets-elapsed
// contract: a fresh Touch on a session that was about to be reaped
// must protect it for another full timeout window. Without this
// behaviour, a session mid-burst of activity could be killed under
// the agent's feet.
func TestIdleReaper_TouchResetsTimer(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	rec := newRecordingKill()
	timeout := time.Minute
	r := NewIdleReaper(timeout, rec.kill).withClock(clock)

	r.Touch("demo")
	// Advance 59s (still safe — under timeout).
	clock.Advance(59 * time.Second)
	r.Touch("demo") // timer reset.
	// Advance 30s (89s since first Touch, only 30s since reset → safe).
	clock.Advance(30 * time.Second)
	r.Reap(context.Background(), clock.Now())
	if got := rec.names(); len(got) != 0 {
		t.Fatalf("Touch should reset the timer; got kills %v", got)
	}
	// Now drift past the timeout from the *reset* point and verify the
	// reap fires.
	clock.Advance(timeout)
	r.Reap(context.Background(), clock.Now())
	if got := rec.names(); len(got) != 1 || got[0] != "demo" {
		t.Fatalf("expected reap after timer drift, got %v", got)
	}
}

// TestIdleReaper_ForgetSuppressesReap covers the explicit-removal path.
// session_kill / kill_all_sessions use Forget to drop the activity
// record so a subsequent reap doesn't try to kill an already-gone
// session (which would surface as a controller error and noise in the
// log). Forget on a never-seen name is a silent no-op.
func TestIdleReaper_ForgetSuppressesReap(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	rec := newRecordingKill()
	timeout := time.Minute
	r := NewIdleReaper(timeout, rec.kill).withClock(clock)

	r.Touch("demo")
	r.Forget("demo")
	r.Forget("never-seen") // tolerated, no panic.
	clock.Advance(timeout * 2)
	r.Reap(context.Background(), clock.Now())
	if got := rec.names(); len(got) != 0 {
		t.Fatalf("Forget should drop the entry; got kills %v", got)
	}
}

// TestIdleReaper_KillErrorIsLoggedNotFatal exercises the best-effort
// error path: when KillFunc returns an error, the reaper must still
// drop the entry (so it does not retry-kill on every tick) and must
// continue to reap the *other* victims in the same sweep. We
// deliberately mix a failing session with a succeeding one and assert
// both were attempted.
func TestIdleReaper_KillErrorIsLoggedNotFatal(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	rec := newRecordingKill()
	rec.errFor["broken"] = errors.New("synthetic kill failure")
	timeout := time.Minute
	r := NewIdleReaper(timeout, rec.kill).withClock(clock)

	r.Touch("broken")
	r.Touch("ok")
	clock.Advance(timeout + time.Second)
	r.Reap(context.Background(), clock.Now())

	got := rec.names()
	if len(got) != 2 {
		t.Fatalf("both sessions must be attempted, got %v", got)
	}
	// And the failing entry must NOT be retried on the next reap —
	// dropping the entry on attempt is the policy.
	clock.Advance(timeout * 2)
	r.Reap(context.Background(), clock.Now())
	if names := rec.names(); len(names) != 2 {
		t.Fatalf("failing entry must not be retried; got %v", names)
	}
}

// TestIdleReaper_RunReapsViaTicker is the integration smoke test. It
// drives the goroutine end-to-end: Run starts the ticker, we Touch a
// session, advance the fake clock past the timeout, and assert that
// within a short real-time deadline the kill hook was invoked. The
// reaper's Run uses time.NewTicker which we cannot fake, so the test
// uses a small (1s) timeout — the reaper's reapInterval floor — and
// polls the recorder briefly.
func TestIdleReaper_RunReapsViaTicker(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	rec := newRecordingKill()
	// 1s is the minimum reapInterval so the goroutine wakes after at
	// most 1s — fast enough for a CI test without being so tight that
	// a slow runner flakes.
	timeout := time.Second
	r := NewIdleReaper(timeout, rec.kill).withClock(clock)

	r.Touch("demo")
	// Pre-advance the clock past the timeout so the very first tick
	// observes the session as expired. Without the pre-advance the
	// test would have to wait `timeout` of real wall-clock for the
	// elapsed delta to cross the threshold, doubling the runtime.
	clock.Advance(timeout + 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	// Give the ticker a generous deadline (3× reapInterval) to fire.
	deadline := time.After(3 * time.Second)
	for {
		if names := rec.names(); len(names) == 1 && names[0] == "demo" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("Run did not reap demo within deadline; got %v", rec.names())
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Run did not exit within 1s after cancel")
	}
}

// TestSessionFromArgs covers the dispatch table inside sessionFromArgs.
// It is the only piece of logic that decides whether a tools/call
// counts as activity for a particular session, so a regression here
// would silently break the reaper's accounting. The cases mirror the
// spec list in the function docstring.
func TestSessionFromArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		method string
		args   string
		want   string
	}{
		{"send_keys uses session", "send_keys", `{"session":"demo","keys":["x"]}`, "demo"},
		{"capture uses session", "capture", `{"session":"demo"}`, "demo"},
		{"wait_for_text uses session", "wait_for_text", `{"session":"demo","pattern":"x"}`, "demo"},
		{"resize uses session", "resize", `{"session":"demo","width":80,"height":24}`, "demo"},
		{"send_signal uses session", "send_signal", `{"session":"demo","signal":"TERM"}`, "demo"},
		{"snapshot_diff uses session", "snapshot_diff", `{"session":"demo"}`, "demo"},
		{"list_panes scoped uses session", "list_panes", `{"session":"demo"}`, "demo"},
		{"list_panes unscoped is empty", "list_panes", `{}`, ""},

		{"session_create uses name", "session_create", `{"name":"demo"}`, "demo"},
		{"session_kill uses name", "session_kill", `{"name":"demo"}`, "demo"},
		{"session_describe uses name", "session_describe", `{"name":"demo"}`, "demo"},
		{"session_rename uses name (old)", "session_rename", `{"name":"demo","new_name":"shiny"}`, "demo"},

		{"pane_select extracts session from target", "pane_select", `{"target":"demo:0.1"}`, "demo"},
		{"pane_select with bare session", "pane_select", `{"target":"demo"}`, "demo"},
		{"pane_select with leading colon is empty", "pane_select", `{"target":":foo"}`, ""},

		{"session_list is excluded", "session_list", `{}`, ""},
		{"kill_all_sessions is excluded", "kill_all_sessions", `{}`, ""},

		{"unknown method falls back to session", "future_tool", `{"session":"demo"}`, "demo"},
		{"malformed args yield empty", "send_keys", `{not-json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionFromArgs(tc.method, []byte(tc.args))
			if got != tc.want {
				t.Fatalf("sessionFromArgs(%q, %s) = %q; want %q",
					tc.method, tc.args, got, tc.want)
			}
		})
	}
}

// TestWithSessionIdleTimeout_DisabledIsNoOp asserts the option setter
// itself respects the "0 disables" contract — when the operator leaves
// the flag at the default the serveConfig's reaper field stays nil so
// Serve never launches the reaper goroutine. Same coverage exists at
// the NewIdleReaper layer; this test pins the wiring at the option
// boundary so a future refactor that splits the two paths still keeps
// the no-op default.
func TestWithSessionIdleTimeout_DisabledIsNoOp(t *testing.T) {
	t.Parallel()

	for _, d := range []time.Duration{0, -1} {
		var cfg serveConfig
		WithSessionIdleTimeout(d, nil)(&cfg)
		if cfg.reaper != nil {
			t.Fatalf("WithSessionIdleTimeout(%v): expected nil reaper, got %p", d, cfg.reaper)
		}
	}

	var cfg serveConfig
	WithSessionIdleTimeout(time.Minute, func(context.Context, string) error { return nil })(&cfg)
	if cfg.reaper == nil {
		t.Fatal("WithSessionIdleTimeout(positive): expected non-nil reaper, got nil")
	}
}

// TestReapInterval pins the cadence-selection rules — pathologically
// small timeouts floor at 1s (avoids busy-looping); large timeouts cap
// at idleReaperMaxInterval (so a 24h idle window doesn't park the
// goroutine for hours). Mid-range values get timeout/4.
func TestReapInterval(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{"sub-second floors at 1s", 100 * time.Millisecond, time.Second},
		{"exactly 1s also 1s", time.Second, time.Second},
		{"1m → 15s", time.Minute, 15 * time.Second},
		{"large caps at 30s", time.Hour, idleReaperMaxInterval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reapInterval(tc.timeout); got != tc.want {
				t.Fatalf("reapInterval(%v) = %v; want %v", tc.timeout, got, tc.want)
			}
		})
	}
}
