package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// LockSession wraps `tmux lock-session -t SESSION`. tmux locks every
// client currently attached to the named session by running the
// configured `lock-command` (default `lock -np`); the running processes
// inside the session are left untouched, only the attached clients see
// the lock screen until the user authenticates. Headless servers (the
// common case for tmux-mcp) have nothing to lock — tmux still exits 0
// against an existing session because the iteration over attached
// clients is empty — which is exactly what the integration test
// exercises.
//
// Surface intent: this is the canonical "secure the screen" primitive
// when an agent is handing a long-running session back to a human and
// wants attached terminals to require authentication before resuming
// work. Unlike `kill-session` it preserves every running process and
// leaves the session targetable for follow-up tools.
//
// A missing session surfaces as a wrapped errs.ErrSessionNotFound so
// the JSON-RPC dispatcher maps it to CodeSessionNotFound — same
// contract every other tmuxctl method upholds. tmux's stderr for the
// missing case ("can't find session: <name>") is already recognised by
// run()'s isSessionMissingMsg, so the wrapping happens automatically.
// We do not need a fallback "can't find window/pane" branch the way
// pane-targeted commands do — `lock-session -t` only accepts a session
// reference, so tmux's diagnostic always names the session directly.
func (c *Controller) LockSession(ctx context.Context, session string) error {
	if session == "" {
		return errors.New("session required")
	}
	if _, err := c.run(ctx, "lock-session", "-t", session); err != nil {
		// Defensive translation: a future tmux version that phrases the
		// missing-session error differently from isSessionMissingMsg's
		// known set should still map onto the typed sentinel so the
		// JSON-RPC layer stays consistent. The current matcher already
		// covers every spelling tmux 3.x emits for `lock-session`, so
		// the branch below is a no-op for today's binaries.
		if !errors.Is(err, errs.ErrSessionNotFound) {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "session not found") ||
				strings.Contains(msg, "can't find session") {
				return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
			}
		}
		return err
	}
	return nil
}
