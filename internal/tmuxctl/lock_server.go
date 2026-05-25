package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// isNoServerRunningMsg recognises the stderr tmux emits when a command
// targets a controller whose private daemon has not yet been spawned.
// tmux phrases this several ways across versions and across the
// "socket file does not exist" vs "connect failed" code paths:
//
//   - "no server running on /path/to/sock"
//   - "error connecting to /path/to/sock (No such file or directory)"
//   - "server exited unexpectedly"
//
// All three mean the same thing at the lock_server boundary: there is
// no daemon to lock. The dispatcher maps the resulting wrapped sentinel
// onto CodeSessionNotFound (-32000) so callers can branch on a stable
// signal — same vocabulary every other "named target does not exist"
// path uses (list_clients, session_kill, lock_session, …).
//
// Detection is by message text rather than exit-code shape because
// every tmux failure exits non-zero; the message is the only signal
// that pins down which failure mode tmux hit.
func isNoServerRunningMsg(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "no server running") ||
		strings.Contains(m, "error connecting") ||
		strings.Contains(m, "server exited unexpectedly") ||
		strings.Contains(m, "no such file or directory")
}

// LockServer wraps `tmux lock-server` (alias `lock`). tmux iterates
// every attached client on this controller's private daemon and runs
// the configured `lock-command` (default `lock -np`) against each one.
// Headless servers (the common case for tmux-mcp) have nothing to lock
// — tmux still exits 0 because the iteration over attached clients is
// empty, which is the contract every operator deployment relies on for
// the "secure every screen on this server" primitive.
//
// Surface intent: this is the server-wide counterpart to lock_session
// (one named session) and lock_client (one specific TTY). When an
// operator hands a long-running multi-session daemon back to a human
// and wants every attached terminal — across every session — to require
// authentication before resuming work, lock_server is the single call
// that does it. Unlike `kill-server` it preserves every running process
// and leaves every session/window/pane targetable for follow-up tools.
//
// Takes no flags. tmux's `lock-server` is the simplest of the three
// lock primitives — there is no -t target, no client filter, nothing
// to validate at the boundary.
//
// "No server running" surfaces as a wrapped errs.ErrSessionNotFound so
// the JSON-RPC dispatcher maps it onto CodeSessionNotFound — semantically
// the same "named target (the daemon itself, in this case) does not
// exist" failure every other tmuxctl method uses for missing targets.
// Sharing the code keeps clients from having to learn a per-tool failure
// vocabulary just for this primitive.
func (c *Controller) LockServer(ctx context.Context) error {
	if _, err := c.run(ctx, "lock-server"); err != nil {
		// Defensive translation: tmux's run() already maps
		// "session not found"-shaped messages onto ErrSessionNotFound,
		// but "no server running" / "error connecting" go through a
		// different code path because they predate any session
		// resolution. Wrap them here so the JSON-RPC layer sees the
		// uniform sentinel without needing a lock_server-specific
		// branch in errs.CodeOf.
		if !errors.Is(err, errs.ErrSessionNotFound) && isNoServerRunningMsg(err.Error()) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}
