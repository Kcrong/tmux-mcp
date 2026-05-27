package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// RotateWindow cycles every pane in the targeted window through the
// existing layout slots via `tmux rotate-window [-D|-U] -t <target>`.
// Unlike next_layout / previous_layout (which switch between the
// preset layouts: even-horizontal, main-vertical, tiled, …),
// rotate-window keeps the current layout *shape* fixed and only
// rotates which pane occupies which slot. With three panes A B C in a
// horizontal row, `-U` (the tmux default) shifts every pane "up" one
// slot so the layout becomes B C A; `-D` rotates the other way and
// produces C A B.
//
// target is the tmux window target the agent wants rotated. Both
// "session" (rotates the active window of that session) and
// "session:window" forms are accepted — tmux resolves them uniformly.
// An empty target is rejected up front rather than letting tmux fall
// back to "the current window of the current client", which is almost
// never what an agent meant.
//
// downward=true emits `-D`; downward=false (the default) emits the
// tmux-default `-U`. Mirroring SwapWindow's noSelect-style boolean
// keeps the boundary signature simple — agents just flip one bit to
// pick the direction.
//
// A missing session/window surfaces as a wrapped errs.ErrSessionNotFound
// for the same reason described on SwapWindow: tmux's rotate-window
// emits "can't find window" when the target does not exist, which
// run() does not translate by itself, so we fold it into the typed
// sentinel here so the JSON-RPC dispatcher maps the failure to
// CodeSessionNotFound. Some tmux builds also emit "no current target"
// when no -t was passed; we never reach that branch (we always pass
// -t) but defensively translate it too in case a future tmux version
// rephrases the missing-target error.
func (c *Controller) RotateWindow(ctx context.Context, target string, downward bool) error {
	if target == "" {
		return errors.New("target required")
	}
	// -D means "rotate downward" (panes shift the other way through the
	// slots); the tmux default -U is what an interactive
	// `prefix C-o` produces. Selecting the flag here keeps the argv
	// order easy to diff against the man page (`rotate-window [-D|-U] -t target`).
	flag := "-U"
	if downward {
		flag = "-D"
	}
	if _, err := c.run(ctx, "rotate-window", flag, "-t", target); err != nil {
		// rotate-window against a missing window emits "can't find
		// window: <name>"; some older tmux builds emit "no current
		// target". run() does not translate either by itself — fold them
		// into errs.ErrSessionNotFound so callers can errors.Is into the
		// sentinel regardless of which message the local tmux happened
		// to produce.
		if !errors.Is(err, errs.ErrSessionNotFound) {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "can't find window") ||
				strings.Contains(msg, "no current target") {
				return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
			}
		}
		return err
	}
	return nil
}
