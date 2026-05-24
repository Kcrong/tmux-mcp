package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// SwapPane wraps `tmux swap-pane -s <src> -t <dst>`. tmux exchanges the
// two panes in place: the contents, processes, and tmux pane ids stay
// put while the layout slots are switched, so callers chaining
// pane_select / send_keys against either target see the new occupant
// immediately.
//
// Both src and dst must be tmux pane targets (e.g. "demo:0.0" or just
// "demo:0" to mean "the active pane of window 0"). The boundary regex
// catches stray quoting / shell metachars before we get here, so this
// method only enforces the two-required-arg contract and the typed
// session-not-found mapping.
//
// A missing session surfaces as a wrapped errs.ErrSessionNotFound (via
// run() or the can't-find-pane translation below) so the JSON-RPC
// dispatcher maps it to CodeSessionNotFound — the same contract every
// other pane-scoped method upholds.
func (c *Controller) SwapPane(ctx context.Context, src, dst string) error {
	if src == "" {
		return errors.New("src required")
	}
	if dst == "" {
		return errors.New("dst required")
	}
	_, err := c.run(ctx, "swap-pane", "-s", src, "-t", dst)
	if err != nil {
		// tmux swap-pane against a missing pane says "can't find pane"
		// instead of the "session not found" form that run() already
		// maps. Translate it so callers can errors.Is into
		// errs.ErrSessionNotFound regardless of which message tmux
		// emitted.
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find pane") {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}
