package tmuxctl

import (
	"context"
	"sort"
	"testing"
	"time"
)

// TestKillAllSessions_KillsEveryKnownSession creates several sessions and
// asserts that a single KillAllSessions call clears all of them and
// returns the killed names.
func TestKillAllSessions_KillsEveryKnownSession(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	want := []string{"alpha", "beta", "gamma"}
	for _, n := range want {
		if err := c.CreateSession(ctx, SessionSpec{Name: n, Command: "/bin/sh"}); err != nil {
			t.Fatalf("CreateSession(%q): %v", n, err)
		}
	}

	killed, err := c.KillAllSessions(ctx)
	if err != nil {
		t.Fatalf("KillAllSessions: %v", err)
	}
	sort.Strings(killed)
	if len(killed) != len(want) {
		t.Fatalf("killed = %v, want %v", killed, want)
	}
	for i, n := range want {
		if killed[i] != n {
			t.Fatalf("killed[%d] = %q, want %q (full slice %v)", i, killed[i], n, killed)
		}
	}

	names, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions after kill-all: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("expected zero sessions after KillAllSessions, got %v", names)
	}
}

// TestKillAllSessions_ZeroSessionsIsNoop confirms calling on a fresh
// controller returns an empty slice and a nil error rather than tripping
// on the no-server-running edge case.
func TestKillAllSessions_ZeroSessionsIsNoop(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	killed, err := c.KillAllSessions(ctx)
	if err != nil {
		t.Fatalf("KillAllSessions on empty controller: %v", err)
	}
	if len(killed) != 0 {
		t.Fatalf("expected empty slice, got %v", killed)
	}
}
