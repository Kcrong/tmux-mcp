package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// JoinPane wraps `tmux join-pane -s <src> -t <dst>` (with `-h` when
// horizontal is true). tmux moves the source pane out of its current
// window and re-attaches it to the destination window as a new split:
// `-h` produces a horizontal (left/right) split, the default is vertical
// (top/bottom). The source pane keeps its `#{pane_id}`, contents, and
// running process — only the layout slot changes — so callers chaining
// pane_select / send_keys against the moved pane see the new placement
// immediately.
//
// Both src and dst must be tmux targets. src is the pane to move, in
// the canonical "session:window.pane" form (e.g. "demo:1.0"); dst is
// the destination window in "session:window" form (e.g. "demo:0").
// tmux accepts the broader pane-target shapes too — the boundary regex
// at the JSON-RPC layer catches stray quoting / shell metachars before
// we get here, so this method only enforces the two-required-arg
// contract and the typed session-not-found mapping.
//
// A missing session/window/pane surfaces as a wrapped
// errs.ErrSessionNotFound (via run() or the can't-find-pane translation
// below) so the JSON-RPC dispatcher maps it to CodeSessionNotFound —
// the same contract every other pane-scoped method upholds. tmux
// phrases the missing-target case as "can't find pane" rather than the
// "session not found" run() already maps; translate that explicitly so
// callers can errors.Is into the typed sentinel regardless of which
// variant tmux emitted.
func (c *Controller) JoinPane(ctx context.Context, src, dst string, horizontal bool) error {
	if src == "" {
		return errors.New("src required")
	}
	if dst == "" {
		return errors.New("dst required")
	}
	args := []string{"join-pane", "-s", src, "-t", dst}
	if horizontal {
		// -h splits the destination pane left/right; the default (no
		// flag) splits top/bottom, matching tmux's interactive default.
		args = append(args, "-h")
	}
	if _, err := c.run(ctx, args...); err != nil {
		// tmux join-pane against a missing pane / window says "can't
		// find pane" instead of the "session not found" form that run()
		// already maps. Translate it so callers can errors.Is into
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
