package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// MovePane wraps `tmux move-pane -s <src> -t <dst>` (with `-h`, `-b`, and
// `-d` selected by the boolean knobs). Unlike SwapPane (which exchanges
// two existing panes in place) and BreakPane (which detaches a pane into
// its own new window), MovePane *relocates* a single pane onto a target
// slot — the destination may live in another window or even another
// session, and tmux splits the destination pane to make room.
//
// Both src and dst must be tmux pane targets (e.g. "demo:0.0", "demo:1",
// or the bare `%N` pane id). The boundary regex catches stray quoting /
// shell metachars before we get here, so this method only enforces the
// two-required-arg contract and the typed session-not-found mapping.
//
// The boolean knobs map one-for-one onto the underlying tmux flags:
//
//   - horizontal: when true tmux splits the destination left/right (-h);
//     the default (false) splits top/bottom, matching tmux's interactive
//     default.
//   - before: when true tmux places the source pane *before* the
//     destination in the layout (-b); the default (false) places it
//     after, which is what `tmux move-pane` does without the flag.
//   - noFocus: when true tmux does not change the active pane after the
//     move (-d). Most agents want this so a chained send_keys / capture
//     stays deterministic.
//
// A missing session/pane surfaces as a wrapped errs.ErrSessionNotFound
// (via run() or the can't-find-pane / can't-find-window translation
// below) so the JSON-RPC dispatcher maps it to CodeSessionNotFound — the
// same contract every other tmuxctl pane-scoped method upholds.
func (c *Controller) MovePane(ctx context.Context, src, dst string, horizontal, before, noFocus bool) error {
	if src == "" {
		return errors.New("src required")
	}
	if dst == "" {
		return errors.New("dst required")
	}
	args := []string{"move-pane", "-s", src, "-t", dst}
	if horizontal {
		// -h splits the destination pane left/right; the default (no
		// flag) splits top/bottom, matching tmux's interactive default.
		args = append(args, "-h")
	}
	if before {
		// -b inserts the moved pane before the destination in the
		// resulting split, instead of after (the tmux default).
		args = append(args, "-b")
	}
	if noFocus {
		// -d means "do not change the active pane after the move",
		// keeping focus deterministic for callers chaining send_keys /
		// capture against the original pane.
		args = append(args, "-d")
	}
	if _, err := c.run(ctx, args...); err != nil {
		// tmux move-pane against a missing pane / window says "can't
		// find pane" or "can't find window" instead of the
		// "session not found" form that run() already maps. Translate
		// either variant so callers can errors.Is into
		// errs.ErrSessionNotFound regardless of which message tmux
		// emitted.
		if !errors.Is(err, errs.ErrSessionNotFound) {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "can't find pane") ||
				strings.Contains(msg, "can't find window") {
				return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
			}
		}
		return err
	}
	return nil
}
