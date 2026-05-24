package snapshot

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a deterministic, monotonically-advanceable clock used
// by the TTL tests. Production code reads time.Now via the Store's
// injected clock, so wiring fakeClock.now in via withClock lets the
// tests step through wall time without sleeping. We keep it private
// to the test file so it cannot leak into production callers.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestRecord_DetectsChange(t *testing.T) {
	t.Parallel()
	s := New()
	a := s.Record("sess", "hello")
	if a.Token == "" {
		t.Fatal("token empty")
	}
	if !a.Changed {
		t.Fatal("first record should report Changed=true")
	}
	b := s.Record("sess", "hello")
	if b.Token != a.Token {
		t.Fatalf("identical body should yield identical token, got %q vs %q", b.Token, a.Token)
	}
	if b.Changed {
		t.Fatal("identical body should report Changed=false")
	}
	c := s.Record("sess", "world")
	if c.Token == a.Token {
		t.Fatal("different body should yield different token")
	}
	if !c.Changed {
		t.Fatal("different body should report Changed=true")
	}
}

func TestDiffSince_ReturnsLineDiff(t *testing.T) {
	t.Parallel()
	s := New()
	first := s.Record("sess", "alpha\nbeta\ngamma")
	if first.Token == "" {
		t.Fatal("token empty")
	}
	snap, diffs := s.DiffSince("sess", first.Token, "alpha\nBETA\ngamma")
	if !snap.Changed {
		t.Fatal("expected Changed=true")
	}
	if len(diffs) != 1 || diffs[0].Line != 1 || diffs[0].Old != "beta" || diffs[0].New != "BETA" {
		t.Fatalf("unexpected diff: %+v", diffs)
	}
}

func TestDiffSince_NoDiffWhenIdentical(t *testing.T) {
	t.Parallel()
	s := New()
	first := s.Record("sess", "x\ny")
	snap, diffs := s.DiffSince("sess", first.Token, "x\ny")
	if snap.Changed {
		t.Fatal("expected Changed=false for identical body")
	}
	if len(diffs) != 0 {
		t.Fatalf("expected no diff, got %d", len(diffs))
	}
}

func TestDiffSince_FullResetWhenTokenUnknown(t *testing.T) {
	t.Parallel()
	s := New()
	_ = s.Record("sess", "ignored")
	body := "one\ntwo\nthree"
	snap, diffs := s.DiffSince("sess", "deadbeef", body)
	if snap.Token == "" {
		t.Fatal("token empty")
	}
	if len(diffs) != 3 {
		t.Fatalf("expected 3 added lines, got %d", len(diffs))
	}
	for i, d := range diffs {
		if d.Line != i || d.Old != "" || d.New == "" {
			t.Fatalf("line %d: unexpected diff %+v", i, d)
		}
	}
}

func TestDiffSince_HandlesRemovedLines(t *testing.T) {
	t.Parallel()
	s := New()
	first := s.Record("sess", "a\nb\nc")
	_, diffs := s.DiffSince("sess", first.Token, "a")
	// Lines 1 and 2 should be reported as removed.
	if len(diffs) != 2 {
		t.Fatalf("expected 2 removed lines, got %d (%+v)", len(diffs), diffs)
	}
	for _, d := range diffs {
		if !d.Removed {
			t.Errorf("line %d should be Removed: %+v", d.Line, d)
		}
	}
}

func TestForget_DropsSessionHistory(t *testing.T) {
	t.Parallel()
	s := New()
	first := s.Record("sess", "alpha\nbeta")
	if first.Token == "" {
		t.Fatal("token empty")
	}
	if !s.Has("sess") {
		t.Fatal("Has should report true after Record")
	}

	// Sanity: prior token resolves before Forget.
	if _, diffs := s.DiffSince("sess", first.Token, "alpha\nBETA"); len(diffs) != 1 {
		t.Fatalf("pre-forget DiffSince should match prior token, got %d diffs", len(diffs))
	}

	// Forget dropped the entry, so the next access starts fresh.
	s.Forget("sess")
	if s.Has("sess") {
		t.Fatal("Forget did not remove entry")
	}

	// Reusing the old token now yields a full reset because history is gone.
	body := "one\ntwo"
	snap, diffs := s.DiffSince("sess", first.Token, body)
	if !snap.Changed {
		t.Fatal("DiffSince after Forget should report Changed=true")
	}
	if len(diffs) != 2 {
		t.Fatalf("expected full reset (2 added lines) after Forget, got %d diffs", len(diffs))
	}
	for i, d := range diffs {
		if d.Line != i || d.Old != "" || d.New == "" {
			t.Fatalf("line %d: expected added-line diff, got %+v", i, d)
		}
	}

	// Forgetting an unknown session is a no-op (must not panic).
	s.Forget("never-existed")
}

func TestDiffSince_PreservesPriorOnlyForOneStep(t *testing.T) {
	t.Parallel()
	// History keeps two entries — the snapshot you just took and the one
	// before it. After three captures, the original token must be too
	// stale to diff against.
	s := New()
	first := s.Record("sess", "v1")
	_ = s.Record("sess", "v2")
	_ = s.Record("sess", "v3")
	_, diffs := s.DiffSince("sess", first.Token, "v4")
	// We don't have v1 anymore; expect a full reset.
	if !strings.Contains(diffs[0].New, "v4") {
		t.Fatalf("expected reset diff containing v4, got %+v", diffs)
	}
}

func TestTTL_EvictsIdleSessionOnLazyCleanup(t *testing.T) {
	t.Parallel()
	// A session whose last touch is older than the TTL should be
	// pruned the next time anything else hits the store. We wire in a
	// fake clock and a zero cleanup interval so the lazy sweep on
	// Record always runs — same code path production uses, just
	// without wall-clock waits.
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	s := New(
		WithTTL(10*time.Millisecond),
		WithCleanupInterval(0),
		withClock(clock.Now),
	)

	_ = s.Record("idle", "first capture")
	if !s.Has("idle") {
		t.Fatal("Record should have inserted history for idle")
	}

	// Move past the TTL boundary, then record on a *different* session
	// to trigger lazy cleanup. The idle session's history should now
	// be gone; the active session's should be present.
	clock.Advance(50 * time.Millisecond)
	_ = s.Record("active", "fresh")

	if s.Has("idle") {
		t.Fatal("idle session history should have been evicted by TTL cleanup")
	}
	if !s.Has("active") {
		t.Fatal("active session must remain in the store after cleanup")
	}
}

func TestTTL_DiffSinceTreatsExpiredTokenAsFullReset(t *testing.T) {
	t.Parallel()
	// When a caller hands us a token that belonged to an expired
	// session, DiffSince should behave the same as the
	// "session was Forgotten" path: record the new body fresh and
	// emit a full added-line diff. That keeps client logic simple
	// (one shape for "history gone" instead of two).
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	s := New(
		WithTTL(10*time.Millisecond),
		WithCleanupInterval(0),
		withClock(clock.Now),
	)

	first := s.Record("sess", "alpha\nbeta")
	clock.Advance(50 * time.Millisecond)
	// Touch a different session to drive lazy cleanup; otherwise
	// the next call on "sess" itself would refresh lastTouched
	// before the cleanup pass evaluates it.
	_ = s.Record("other", "x")
	if s.Has("sess") {
		t.Fatal("expected sess to be evicted before DiffSince runs")
	}

	body := "one\ntwo\nthree"
	snap, diffs := s.DiffSince("sess", first.Token, body)
	if snap.Token == "" {
		t.Fatal("token empty")
	}
	if !snap.Changed {
		t.Fatal("DiffSince after TTL eviction should report Changed=true")
	}
	if len(diffs) != 3 {
		t.Fatalf("expected 3 added-line diffs after eviction, got %d (%+v)", len(diffs), diffs)
	}
	for i, d := range diffs {
		if d.Line != i || d.Old != "" || d.New == "" {
			t.Fatalf("line %d: expected added-line diff, got %+v", i, d)
		}
	}
}

func TestTTL_ZeroDisablesCleanup(t *testing.T) {
	t.Parallel()
	// TTL=0 is the back-compat / "I'll manage cleanup myself" mode.
	// Even after advancing well past any sane TTL, the entry must
	// survive — same behaviour as the pre-TTL Store.
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	s := New(
		WithTTL(0),
		WithCleanupInterval(0),
		withClock(clock.Now),
	)
	_ = s.Record("sess", "v1")
	clock.Advance(24 * time.Hour)
	_ = s.Record("other", "v2")
	if !s.Has("sess") {
		t.Fatal("TTL=0 should retain history indefinitely")
	}
	// And an explicit CleanupExpired call must also be a no-op.
	if removed := s.CleanupExpired(); removed != 0 {
		t.Fatalf("CleanupExpired with TTL=0 should remove nothing, got %d", removed)
	}
}

func TestTTL_NegativeTTLClampsToZero(t *testing.T) {
	t.Parallel()
	// A negative TTL is a programming bug; we clamp it to 0 (cleanup
	// disabled) rather than panic so a typo in main.go can't take
	// down the server.
	s := New(WithTTL(-5 * time.Minute))
	_ = s.Record("sess", "body")
	if removed := s.CleanupExpired(); removed != 0 {
		t.Fatalf("negative TTL must behave like 0; got removed=%d", removed)
	}
	if !s.Has("sess") {
		t.Fatal("negative TTL must not evict anything")
	}
}

func TestTTL_RecordRefreshesLastTouched(t *testing.T) {
	t.Parallel()
	// Re-recording on a session must refresh its lastTouched so it
	// survives subsequent cleanup passes. This is the common
	// "active session keeps streaming" case — TTL is for *idle*
	// sessions, not busy ones.
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	s := New(
		WithTTL(20*time.Millisecond),
		WithCleanupInterval(0),
		withClock(clock.Now),
	)

	_ = s.Record("busy", "v1")
	// Advance to 15ms — under the 20ms TTL — and write again.
	clock.Advance(15 * time.Millisecond)
	_ = s.Record("busy", "v2")
	// Advance another 15ms (total 30ms since v1, but only 15ms since
	// v2). v2's write should have refreshed lastTouched, so the
	// session must still be present.
	clock.Advance(15 * time.Millisecond)
	_ = s.Record("trigger", "x")
	if !s.Has("busy") {
		t.Fatal("re-recording should refresh lastTouched and keep the session alive")
	}

	// Now go fully idle past the TTL and confirm the session is finally pruned.
	clock.Advance(100 * time.Millisecond)
	_ = s.Record("trigger", "y")
	if s.Has("busy") {
		t.Fatal("busy session should be evicted once it actually goes idle past TTL")
	}
}

func TestTTL_CleanupInterval_ThrottlesSweeps(t *testing.T) {
	t.Parallel()
	// With a non-zero cleanup interval, lazy cleanup should not run
	// on every Record — it should be amortised. We verify this by
	// observing that an idle session is still present immediately
	// after a Record on a different session, but disappears once we
	// cross the throttle window and trigger another Record.
	start := time.Unix(1_700_000_000, 0)
	clock := newFakeClock(start)
	s := New(
		WithTTL(10*time.Millisecond),
		WithCleanupInterval(time.Second),
		withClock(clock.Now),
	)

	_ = s.Record("idle", "v1")
	// Advance just past the TTL but well within the cleanup
	// interval. The next Record should *not* sweep yet.
	clock.Advance(50 * time.Millisecond)
	_ = s.Record("other", "x")
	if !s.Has("idle") {
		t.Fatal("cleanup ran inside the throttle window — should be deferred")
	}

	// Advance past the cleanup interval and Record again to force the sweep.
	clock.Advance(2 * time.Second)
	_ = s.Record("other", "y")
	if s.Has("idle") {
		t.Fatal("cleanup should have run after exceeding cleanupInterval")
	}
}

func TestTTL_CleanupExpired_ReturnsRemovedCount(t *testing.T) {
	t.Parallel()
	// CleanupExpired is the explicit-sweep entry point. It should
	// drop every session whose lastTouched is older than the TTL and
	// return the count, so tests / diagnostics can verify behaviour
	// without poking internals.
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	s := New(
		WithTTL(time.Minute),
		WithCleanupInterval(time.Hour), // disable lazy sweep so we measure the explicit call alone.
		withClock(clock.Now),
	)
	_ = s.Record("a", "1")
	_ = s.Record("b", "2")
	clock.Advance(30 * time.Second)
	_ = s.Record("c", "3") // c gets a fresher timestamp.

	clock.Advance(45 * time.Second) // a, b are now > 60s idle; c is 45s.
	removed := s.CleanupExpired()
	if removed != 2 {
		t.Fatalf("expected 2 sessions evicted, got %d (entries=%d)", removed, s.Len())
	}
	if s.Has("a") || s.Has("b") {
		t.Fatal("a/b should be evicted")
	}
	if !s.Has("c") {
		t.Fatal("c should still be present")
	}
}

func TestTTL_ConcurrentRecordsRaceClean(t *testing.T) {
	t.Parallel()
	// Sanity check under -race: lazy cleanup is invoked from
	// Record/DiffSince while holding the store mutex, so concurrent
	// callers should not data-race even though sessions are being
	// added and removed. We do not assert exact eviction counts here
	// — the race detector is the assertion. Real wall time so this
	// test is also a smoke test of the production code path with
	// time.Now wired in.
	s := New(
		WithTTL(time.Millisecond),
		WithCleanupInterval(0),
	)

	const goroutines = 8
	const opsPerG = 200
	var wg sync.WaitGroup
	var totalRecords atomic.Int64
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < opsPerG; i++ {
				name := "sess-" + strings.Repeat("x", id+1)
				_ = s.Record(name, "body")
				totalRecords.Add(1)
				if i%3 == 0 {
					_, _ = s.DiffSince(name, "", "body-"+strings.Repeat("y", i%5))
				}
			}
		}(g)
	}
	wg.Wait()

	if totalRecords.Load() != int64(goroutines*opsPerG) {
		t.Fatalf("unexpected record count: %d", totalRecords.Load())
	}
	// Drive one more sweep to make sure CleanupExpired is also safe
	// to call after a flurry of writes.
	_ = s.CleanupExpired()
}
