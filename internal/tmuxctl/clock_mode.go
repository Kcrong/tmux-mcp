package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// ClockMode wraps `tmux clock-mode [-t TARGET]`. clock-mode is tmux's
// built-in screensaver: it puts the targeted pane (or the current one
// when target is empty) into a special mode that renders a large
// digital clock until the next key arrives. The pane process keeps
// running underneath — clock-mode only takes over the pane's display.
//
// Surface intent: a thin pass-through for agents that want to "park"
// a pane visually (demo recording, status board, idle indicator)
// without typing keys into the running program.
//
// Target rules: target uses tmux's standard pane-target form (e.g.
// "demo:0.1", "demo:0", "demo", or a pane id like "%5"). The boundary
// (server tool) is responsible for the up-front regex/length check;
// the controller passes the value verbatim to tmux. An empty target
// is permitted — tmux then targets whichever pane is currently active
// on the server's "current" client/session.
//
// Error contract: a missing pane/session surfaces as a wrapped
// errs.ErrSessionNotFound so the JSON-RPC dispatcher maps it to
// CodeSessionNotFound — same contract every other tmuxctl method
// upholds. tmux phrases the missing-target case several ways across
// versions ("can't find pane", "can't find session", "no current
// target"); translate all of them into the typed sentinel run()
// emits for "session not found" so callers can errors.Is into
// errs.ErrSessionNotFound regardless of which phrasing tmux happened
// to produce.
//
// A headless controller (the tmux server has not started yet, or the
// socket is gone) also resolves to the same typed sentinel: with no
// server there is no pane to enter clock-mode on, which is a "hard
// miss" from the caller's point of view rather than a generic
// internal failure.
func (c *Controller) ClockMode(ctx context.Context, target string) error {
	args := []string{"clock-mode"}
	if target != "" {
		args = append(args, "-t", target)
	}
	if _, err := c.run(ctx, args...); err != nil {
		msg := strings.ToLower(err.Error())
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			(strings.Contains(msg, "can't find pane") ||
				strings.Contains(msg, "can't find window") ||
				strings.Contains(msg, "no current target") ||
				strings.Contains(msg, "no server running") ||
				strings.Contains(msg, "error connecting")) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}
