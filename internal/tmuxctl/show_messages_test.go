package tmuxctl

import (
	"context"
	"testing"
	"time"
)

// TestShowMessages_EmptyBeforeServerStarted exercises the "no server
// running" path: a freshly-constructed controller has not started any
// tmux daemon yet, so `show-messages` cannot connect to a socket. The
// idempotent contract ("no client → empty slice") covers this case
// too — zero messages exist by definition before the server is up,
// so an agent should be able to call this without first having to
// CreateSession.
func TestShowMessages_EmptyBeforeServerStarted(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	got, err := c.ShowMessages(ctx, "", false, false)
	if err != nil {
		t.Fatalf("ShowMessages on fresh controller: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected zero messages from a controller with no server, got %d (%v)", len(got), got)
	}
}
