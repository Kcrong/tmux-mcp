// Package snapshot tracks pane captures so callers can ask for diffs
// rather than the full screen.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

// Store keeps the two most recent captures per session. Callers pass a
// token returned from a previous capture; if we still have the body for
// that token, DiffSince returns a per-line diff against the new capture.
// Otherwise the caller gets a full reset (every line marked as new).
type Store struct {
	mu      sync.Mutex
	entries map[string]history
}

// history holds the latest capture and the one before it. Two slots is
// enough for the "snapshot now → mutate → diff_since(token)" usage; if
// the caller waits longer between calls, they see a full reset, which
// is the right behaviour anyway.
type history struct {
	current entry
	prior   entry
}

type entry struct {
	token string
	body  string
}

// New returns an empty Store.
func New() *Store {
	return &Store{entries: map[string]history{}}
}

// Snapshot is the result of recording a capture.
type Snapshot struct {
	Token   string
	Body    string
	Changed bool
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

// Record stores body under session and returns a token derived from the
// content. Changed is true when the body differs from the prior call.
func (s *Store) Record(session, body string) Snapshot {
	tok := tokenize(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.entries[session]
	cur := entry{token: tok, body: body}
	if ok {
		s.entries[session] = history{current: cur, prior: prev.current}
	} else {
		s.entries[session] = history{current: cur}
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
func (s *Store) DiffSince(session, priorToken, body string) (Snapshot, []Diff) {
	tok := tokenize(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	hist, hadAny := s.entries[session]
	cur := entry{token: tok, body: body}
	prior, found := matchPrior(hist, priorToken)
	// Update history before we leave the lock.
	if hadAny {
		s.entries[session] = history{current: cur, prior: hist.current}
	} else {
		s.entries[session] = history{current: cur}
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
