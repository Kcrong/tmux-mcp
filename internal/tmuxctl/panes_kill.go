package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// KillPane wraps `tmux kill-pane -t TARGET`. target uses tmux's standard
// pane-target form (e.g. "demo:0.1", "demo:0", "demo") — the boundary
// (server tool) is responsible for the up-front regex/length check; the
// controller passes the value verbatim to tmux.
//
// We deliberately do *not* refuse the "kill the last pane of a window"
// case here. tmux's own behaviour is to also tear down the window (and,
// if it was the last window, the session): preserving that semantics
// keeps kill-pane composable with the rest of the surface and matches
// what an interactive `tmux kill-pane` would do. Callers that want to
// guard against accidental session teardown can pre-check with
// list_panes / list_windows before calling.
//
// A missing session/pane surfaces as a wrapped errs.ErrSessionNotFound
// so the JSON-RPC dispatcher maps it to CodeSessionNotFound — same
// contract every other tmuxctl method upholds.
func (c *Controller) KillPane(ctx context.Context, target string) error {
	if target == "" {
		return errors.New("target required")
	}
	if _, err := c.run(ctx, "kill-pane", "-t", target); err != nil {
		// tmux phrases the missing-target case as "can't find pane:" or
		// "no current target" depending on the form of the target string
		// (and the version on PATH). Translate both into the same typed
		// sentinel run() emits for "session not found", so callers can
		// errors.Is into errs.ErrSessionNotFound regardless of which
		// variant tmux happened to emit.
		msg := strings.ToLower(err.Error())
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			(strings.Contains(msg, "can't find pane") ||
				strings.Contains(msg, "no current target")) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}
