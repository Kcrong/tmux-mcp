package tmuxctl

import (
	"context"
	"testing"
	"time"
)

// TestKillServer_PopulatedServerExits anchors a session, calls
// KillServer, then asserts the daemon is gone — list-sessions on the
// same controller must come back with zero entries (the "no server
// running" / "error connecting" path inside ListSessions, which both
// degrade to a nil slice). This is the load-bearing contract: a single
// kill_server call wipes every session the controller could see.
func TestKillServer_PopulatedServerExits(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor a session so the daemon is definitely up before we
	// attempt to kill it. Without this the test would degenerate into
	// the "no server running" no-op branch and stop exercising the
	// real "tmux exits, sessions disappear" path.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.KillServer(ctx); err != nil {
		t.Fatalf("KillServer on populated daemon: %v", err)
	}

	names, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions after KillServer: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("expected zero sessions after KillServer, got %v", names)
	}
}

// TestKillServer_FreshControllerIsNoop pins the idempotent contract on
// a controller whose tmux daemon was never started. The internal
// `c.run` call surfaces tmux's "error connecting to <socket>" stderr
// (the parent directory exists, the socket file does not), and
// KillServer must swallow that into a clean nil — the goal state ("no
// tmux server listening on this socket") is already true.
func TestKillServer_FreshControllerIsNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.KillServer(ctx); err != nil {
		t.Fatalf("KillServer on fresh controller: %v", err)
	}
}

// TestKillServer_DoubleKillIsNoop guards the second branch of the
// idempotency contract: after a successful kill, the daemon has
// exited but the socket file may briefly linger and tmux reports
// "no server running on <socket>" instead of "error connecting".
// Both phrases must collapse to a nil error so an agent looping on
// kill_server (e.g. in an error-recovery dance) never sees a spurious
// failure for a state that is already correct.
func TestKillServer_DoubleKillIsNoop(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Bring the daemon up so the first KillServer hits the real
	// kill-server path, leaving a (possibly still-present) socket file
	// for the second call to trip on.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := c.KillServer(ctx); err != nil {
		t.Fatalf("first KillServer: %v", err)
	}
	if err := c.KillServer(ctx); err != nil {
		t.Fatalf("second KillServer (must be no-op): %v", err)
	}
}
