package tmuxctl

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestStartServer_FreshController exercises the warm-the-daemon flow:
// a freshly-created controller has no tmux server listening on its
// socket, StartServer must spawn one, and ListSessions must then return
// cleanly (an empty slice — the daemon is up but no sessions exist).
//
// The fresh-controller branch is the load-bearing path for callers
// pre-spawning the daemon ahead of session_create, so a regression
// here would silently force the first session_create back to paying
// the spawn cost.
func TestStartServer_FreshController(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.StartServer(ctx); err != nil {
		t.Fatalf("StartServer on fresh controller: %v", err)
	}
	names, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions after StartServer: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("expected zero sessions after StartServer, got %v", names)
	}
}

// TestStartServer_Idempotent pins the no-op-on-second-call contract.
// tmux's `start-server` itself is idempotent, but the wrapper has its
// own error-mapping path (see Controller.run) and an oversight there
// would surface as a spurious failure on the second StartServer call —
// exactly the path agents pre-warming the daemon hit when their
// startup hook fires twice.
func TestStartServer_Idempotent(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.StartServer(ctx); err != nil {
		t.Fatalf("StartServer first call: %v", err)
	}
	if err := c.StartServer(ctx); err != nil {
		t.Fatalf("StartServer second call (must be no-op): %v", err)
	}
}

// TestStartServer_SocketFileExists asserts the visible side effect of a
// successful StartServer call: tmux materialises the unix-domain socket
// at the configured path. Future code that looks for the socket as a
// "is the daemon up?" probe relies on this — without the existence
// check we'd have no way to tell a successful warm-up apart from one
// that silently failed.
func TestStartServer_SocketFileExists(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.StartServer(ctx); err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	info, err := os.Stat(c.Socket())
	if err != nil {
		t.Fatalf("stat socket %q after StartServer: %v", c.Socket(), err)
	}
	// tmux creates a unix-domain socket; ModeSocket is the cross-platform
	// way to assert the daemon actually bound (rather than e.g. leaving
	// a stale regular file from a previous run).
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket %q exists but mode is %s, want a unix-domain socket",
			c.Socket(), info.Mode())
	}
}
