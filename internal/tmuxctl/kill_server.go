package tmuxctl

import (
	"context"
	"strings"
)

// KillServer wraps `tmux kill-server`, asking the controller's private
// tmux daemon to exit. Every session, window, and pane on this socket
// is torn down in one shot — there is no partial form, and no way to
// confine the blast radius to a subset of the controller's state. The
// boundary (the kill_server tool) carries the operator-facing caution;
// at the controller level the call is the single thunk this method is
// here to provide.
//
// Idempotent semantics. The goal state is "no tmux server is listening
// on this socket"; if the daemon is already gone the call is a no-op,
// not an error. tmux itself reports several stderr phrases for the
// "already-gone" case depending on whether the socket file exists and
// whether the client raced the daemon's exit:
//
//   - "no server running on <socket>" — when tmux exits cleanly because
//     the daemon already terminated but the socket file (or its parent
//     directory) still hangs around;
//   - "error connecting to <socket>" — when the client can't even
//     connect (the socket file is missing or the parent directory does
//     not exist), which surfaces with that exact wording on every tmux
//     ≥ 3.0 we support;
//   - "server exited unexpectedly" — when the client noticed the
//     daemon disappear mid-call, which can show up if two kill-server
//     attempts race or if the socket lingers between exits.
//
// All three phrases are recognised here so callers can rely on a nil
// error regardless of which one tmux happened to emit. Any other
// failure (tmux not on PATH, version probe failed, etc.) propagates
// verbatim so the JSON-RPC layer can map it to the standard
// internal-error code.
//
// The controller's socket file (and, when [New] created it, the
// surrounding scratch directory) is left in place — this method is the
// "kill the daemon" primitive, not the "shut the controller down"
// primitive. Cleanup of the socket / scratch directory happens in
// [Controller.Shutdown], which is the path tests and the parent process
// take when they want the whole controller torn down.
func (c *Controller) KillServer(ctx context.Context) error {
	if _, err := c.run(ctx, "kill-server"); err != nil {
		msg := err.Error()
		// Both phrases are case-sensitive in tmux's source, but match
		// case-insensitively here so a future tmux that capitalises the
		// diagnostic does not regress the no-op contract.
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "no server running") ||
			strings.Contains(lower, "error connecting") ||
			strings.Contains(lower, "server exited unexpectedly") {
			return nil
		}
		return err
	}
	return nil
}
