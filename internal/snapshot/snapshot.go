// Package snapshot tracks pane captures so callers can ask for diffs
// rather than the full screen.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// DefaultTTL is the cutoff applied when [New] is called without
// [WithTTL]. Sessions whose last activity is older than this are
// dropped during the next lazy cleanup pass. One hour is generous
// enough that a noisy agent never trips it accidentally and tight
// enough that a long-lived server with churn-y session traffic does
// not balloon its memory footprint over days of uptime.
const DefaultTTL = time.Hour

// minCleanupInterval is the throttle floor for the lazy cleanup pass:
// we never sweep history more than once per this window even if every
// Record/DiffSince call would otherwise opt in. The map walk is cheap
// (a few hundred sessions max in realistic deployments) but doing it
// on every single call would still add noise to hot paths, so we
// amortise it. Tests can shrink this via [WithCleanupInterval].
const defaultCleanupInterval = 30 * time.Second

// Store keeps the two most recent captures per session. Callers pass a
// token returned from a previous capture; if we still have the body for
// that token, DiffSince returns a per-line diff against the new capture.
// Otherwise the caller gets a full reset (every line marked as new).
//
// Each session's history carries a last-touched timestamp. When the
// store is configured with a TTL (the default — see [DefaultTTL]),
// lazy cleanup runs piggy-backed on Record/DiffSince and drops session
// entries whose last touch is older than the TTL. Callers using
// long-lived servers that churn through many short-lived session names
// rely on this to bound memory growth without needing an explicit
// Forget call for every dead session.
type Store struct {
	mu              sync.Mutex
	entries         map[string]history
	ttl             time.Duration // 0 disables cleanup entirely.
	cleanupInterval time.Duration // throttle window for the lazy sweep.
	lastCleanup     time.Time     // wall time of the previous successful sweep.
	now             func() time.Time
}

// history holds the latest capture and the one before it. Two slots is
// enough for the "snapshot now → mutate → diff_since(token)" usage; if
// the caller waits longer between calls, they see a full reset, which
// is the right behaviour anyway.
type history struct {
	current     entry
	prior       entry
	lastTouched time.Time // updated on every Record/DiffSince that lands on this session.
}

type entry struct {
	token string
	body  string
}

// Option configures a [Store] at construction time.
type Option func(*Store)

// WithTTL sets the maximum idle time a session's history may sit in
// the store before being pruned by lazy cleanup. A non-positive
// duration disables cleanup entirely (history is retained until
// [Store.Forget] is called explicitly), which is the right knob to
// reach for in tests that want fully deterministic state.
func WithTTL(d time.Duration) Option {
	return func(s *Store) {
		if d < 0 {
			d = 0
		}
		s.ttl = d
	}
}

// WithCleanupInterval overrides the throttle window between lazy
// cleanup passes. It exists primarily so tests can drive cleanup on
// every Record without waiting [defaultCleanupInterval] of wall time.
// Production code should rely on the default.
func WithCleanupInterval(d time.Duration) Option {
	return func(s *Store) {
		if d < 0 {
			d = 0
		}
		s.cleanupInterval = d
	}
}

// withClock injects a custom now function. Unexported because it is a
// test seam — production callers always see [time.Now]. We keep it on
// the Option type so tests stay in this package.
func withClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// New returns an empty Store. By default it applies [DefaultTTL] to
// stale sessions; pass [WithTTL] to override (including the special
// value 0 which disables cleanup).
func New(opts ...Option) *Store {
	s := &Store{
		entries:         map[string]history{},
		ttl:             DefaultTTL,
		cleanupInterval: defaultCleanupInterval,
		now:             time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.lastCleanup = s.now()
	return s
}

// Snapshot is the result of recording a capture.
type Snapshot struct {
	Token   string // content-addressed handle the caller passes back to DiffSince.
	Body    string // captured pane body, exactly as recorded.
	Changed bool   // true when Body differs from the previous capture for this session.
}

// Forget drops any history we kept for session. Callers should invoke
// it after a session is killed so long-running servers don't leak
// per-session entries across many session_create / session_kill cycles.
// Forgetting an unknown session is a no-op.
func (s *Store) Forget(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, session)
}

// Has reports whether the store currently retains any history for
// session. It is intended for tests and diagnostics.
func (s *Store) Has(session string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[session]
	return ok
}

// Len returns the number of sessions currently tracked. Intended for
// tests and operational diagnostics.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Record stores body under session and returns a token derived from the
// content. Changed is true when the body differs from the prior call.
func (s *Store) Record(session, body string) Snapshot {
	tok := tokenize(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.maybeCleanupLocked(now)
	prev, ok := s.entries[session]
	cur := entry{token: tok, body: body}
	if ok {
		s.entries[session] = history{current: cur, prior: prev.current, lastTouched: now}
	} else {
		s.entries[session] = history{current: cur, lastTouched: now}
	}
	return Snapshot{
		Token:   tok,
		Body:    body,
		Changed: !ok || prev.current.token != tok,
	}
}

// Diff is a per-line change record.
type Diff struct {
	Line    int    // zero-indexed line number in the new body.
	Old     string // previous content; empty if the line was added.
	New     string // current content; empty if the line was removed.
	Removed bool   // true when New is empty because the line is gone.
}

// DiffSince records body and returns the diff vs the prior capture
// matching priorToken. When priorToken is empty or no longer in our
// short history, every line is reported as new.
//
// If priorToken came from a session whose history was pruned by TTL
// cleanup since the caller last looked, DiffSince behaves the same as
// when the token is unknown: it records the new body fresh and reports
// every line as added. The stale-token shape is identical to the
// session-was-Forgotten path, which keeps client logic simple.
func (s *Store) DiffSince(session, priorToken, body string) (Snapshot, []Diff) {
	tok := tokenize(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.maybeCleanupLocked(now)
	hist, hadAny := s.entries[session]
	cur := entry{token: tok, body: body}
	prior, found := matchPrior(hist, priorToken)
	// Update history before we leave the lock.
	if hadAny {
		s.entries[session] = history{current: cur, prior: hist.current, lastTouched: now}
	} else {
		s.entries[session] = history{current: cur, lastTouched: now}
	}
	snap := Snapshot{
		Token:   tok,
		Body:    body,
		Changed: !hadAny || hist.current.token != tok,
	}
	if !found {
		return snap, addedLines(body)
	}
	if prior.token == tok {
		return snap, nil
	}
	return snap, lineDiff(prior.body, body)
}

// CleanupExpired walks every session and drops any whose last-touched
// timestamp is older than the configured TTL. It is safe to call
// concurrently with Record / DiffSince and is exposed for callers that
// want a deterministic sweep (tests, an explicit shutdown hook). With
// TTL=0 it is a no-op so the back-compat "no cleanup" mode stays
// honest. Returns the number of sessions removed.
func (s *Store) CleanupExpired() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanupExpiredLocked(s.now())
}

// maybeCleanupLocked runs cleanupExpiredLocked at most once per
// cleanupInterval. The caller must hold s.mu. We piggy-back on
// Record/DiffSince so TTL pruning happens without a background
// goroutine — simpler lifecycle, same memory bound for any store that
// is actually being used.
func (s *Store) maybeCleanupLocked(now time.Time) {
	if s.ttl <= 0 {
		return
	}
	if !s.lastCleanup.IsZero() && now.Sub(s.lastCleanup) < s.cleanupInterval {
		return
	}
	s.cleanupExpiredLocked(now)
	s.lastCleanup = now
}

// cleanupExpiredLocked removes any session whose lastTouched is older
// than ttl. Callers must hold s.mu. We tolerate the unsynchronised
// "iterate and delete" pattern because Go's map runtime explicitly
// allows it — see https://go.dev/ref/spec#For_statements ("The
// iteration order over maps is not specified … may delete map entries
// that have not yet been reached").
func (s *Store) cleanupExpiredLocked(now time.Time) int {
	if s.ttl <= 0 {
		return 0
	}
	cutoff := now.Add(-s.ttl)
	var removed int
	for name, h := range s.entries {
		// A zero lastTouched should not happen in practice (every
		// write path stamps it), but if it does we treat it as "never
		// touched" and drop the entry — that matches the user-visible
		// promise that nothing sits in the store forever once a TTL
		// is configured.
		if h.lastTouched.Before(cutoff) || h.lastTouched.IsZero() {
			delete(s.entries, name)
			removed++
		}
	}
	return removed
}

// matchPrior looks for an entry in the recent history whose token
// matches priorToken. We accept either the current or the prior entry
// because the caller's token came from one of the last two captures.
func matchPrior(h history, priorToken string) (entry, bool) {
	if priorToken == "" {
		return entry{}, false
	}
	if h.current.token == priorToken {
		return h.current, true
	}
	if h.prior.token == priorToken {
		return h.prior, true
	}
	return entry{}, false
}

func tokenize(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:8])
}

func addedLines(body string) []Diff {
	lines := strings.Split(body, "\n")
	out := make([]Diff, 0, len(lines))
	for i, ln := range lines {
		out = append(out, Diff{Line: i, New: ln})
	}
	return out
}

// lineDiff returns the lines that differ between old and new, indexed
// in the new body. Removed lines past the end of the new body are
// reported with Removed=true and Line=index in old.
func lineDiff(oldBody, newBody string) []Diff {
	oldLines := strings.Split(oldBody, "\n")
	newLines := strings.Split(newBody, "\n")
	var out []Diff
	maxLen := len(oldLines)
	if len(newLines) > maxLen {
		maxLen = len(newLines)
	}
	for i := 0; i < maxLen; i++ {
		var o, n string
		if i < len(oldLines) {
			o = oldLines[i]
		}
		if i < len(newLines) {
			n = newLines[i]
		}
		if o == n {
			continue
		}
		switch {
		case i >= len(newLines):
			out = append(out, Diff{Line: i, Old: o, Removed: true})
		default:
			out = append(out, Diff{Line: i, Old: o, New: n})
		}
	}
	return out
}
