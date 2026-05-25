package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// SendPrefix wraps `tmux send-prefix [-2] -t TARGET`. target uses tmux's
// standard pane-target form (e.g. "demo:0.1", "demo:0", "demo") — the
// boundary (server tool) is responsible for the up-front regex/length
// check; the controller passes the value verbatim to tmux. When
// secondary is true the call passes `-2` so tmux delivers the configured
// secondary prefix key instead of the primary one.
//
// Use this when an inner TUI (vim, htop, weechat, …) running inside a
// tmux pane has captured C-b for its own purposes and an agent needs to
// forward the literal prefix keystroke through to that inner program —
// `tmux send-prefix` is the canonical way to deliver the configured
// prefix without typing it manually, and it respects whatever the
// running tmux server has bound as the prefix (so a deployment that
// remapped to C-a still works).
//
// A missing session/pane surfaces as a wrapped errs.ErrSessionNotFound
// so the JSON-RPC dispatcher maps it to CodeSessionNotFound — same
// contract every other pane-scoped tmuxctl method upholds. tmux phrases
// the missing-target case as "can't find pane" rather than the
// "session not found" form run() already maps; translate that
// explicitly so callers can errors.Is into the typed sentinel
// regardless of which variant tmux emitted.
func (c *Controller) SendPrefix(ctx context.Context, target string, secondary bool) error {
	if target == "" {
		return errors.New("target required")
	}
	args := []string{"send-prefix"}
	if secondary {
		// `-2` selects the secondary prefix key (configured via
		// `prefix2`); without it tmux delivers the primary prefix.
		args = append(args, "-2")
	}
	args = append(args, "-t", target)
	if _, err := c.run(ctx, args...); err != nil {
		// tmux send-prefix against a missing pane says "can't find pane"
		// rather than the "session not found" form run() already maps.
		// Translate it so callers can errors.Is into errs.ErrSessionNotFound
		// regardless of which exact phrase tmux emitted.
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find pane") {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}
