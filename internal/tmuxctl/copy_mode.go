package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// CopyMode wraps `tmux copy-mode [-Hu] [-q] [-M] [-s SRC_PANE]
// [-t TARGET_PANE]`. Entering copy-mode puts the target pane into tmux's
// scrollback / selection mode so a subsequent send_keys call can drive
// the copy-mode key bindings (cursor motion, search, copy-selection,
// …); leaving copy-mode (`-q`) returns the pane to its normal "type
// commands at the shell" state.
//
// target is required: tmux otherwise resolves the empty form to whatever
// pane it considers current, which is almost never what the caller
// wanted. The pane-target shape is validated at the server tool boundary
// (the same conservative regex applied across the pane surface), so this
// method only enforces the bare "non-empty target" invariant.
//
// srcPane is optional. When non-empty tmux clones the source pane's
// scrollback into the target pane before entering copy-mode (-s); this
// is what `Prefix + ;` does interactively when copy-mode is used to
// inspect another pane's history without making it active.
//
// The boolean knobs map one-for-one onto tmux flags:
//
//   - exit: when true tmux quits copy-mode immediately instead of
//     entering it (-q). The two cases share one tool because the wire
//     verb tmux uses ("copy-mode") is the same — only the flag toggles
//     between "go in" and "come back out".
//   - scrollDown: when true tmux scrolls the cursor to the bottom (-u)
//     so the visible region tracks the most recent output rather than
//     freezing at the moment of entry.
//   - mouse: when true tmux starts copy-mode in mouse-drag selection
//     (-M), the same state a click-and-drag with mouse mode enabled
//     would have produced.
//   - dragMode: when true tmux enters HALFLINE drag-mode (-H), the
//     equivalent of pressing `H` after entering copy-mode interactively.
//
// `-e` (exit when status-bar drag finishes) is intentionally not
// surfaced — tmux only honours it under the very specific status-bar
// drag scenario, which the agent surface does not exercise. If a future
// caller actually needs it, it can be added without breaking the
// existing knob shape.
//
// A missing session/pane surfaces as a wrapped errs.ErrSessionNotFound
// (via run() or the can't-find-pane / can't-find-window translation
// below) so the JSON-RPC dispatcher maps it to CodeSessionNotFound —
// the same contract every other tmuxctl pane-scoped method upholds.
func (c *Controller) CopyMode(ctx context.Context, target, srcPane string, exit, scrollDown, mouse, dragMode bool) error {
	if target == "" {
		return errors.New("target required")
	}
	args := []string{"copy-mode"}
	if exit {
		// -q quits copy-mode immediately if the target pane is in it; a
		// plain copy-mode call against a pane already in copy-mode is a
		// no-op, so this flag is the only way to leave copy-mode via the
		// command (versus the interactive `q` keybinding).
		args = append(args, "-q")
	}
	if scrollDown {
		// -u moves the cursor to the bottom of the visible region so
		// follow-up navigation starts at "newest output" rather than
		// frozen at the entry point. tmux's man page calls this "scroll
		// up", but the practical behaviour is "anchor at the bottom".
		args = append(args, "-u")
	}
	if mouse {
		// -M starts copy-mode already in mouse-drag selection. Useful
		// when an agent wants to pre-prime a click-drag region before
		// dispatching mouse events.
		args = append(args, "-M")
	}
	if dragMode {
		// -H enters HALFLINE drag-mode — the same state a user would
		// see after pressing H interactively after entering copy-mode.
		args = append(args, "-H")
	}
	if srcPane != "" {
		// -s SRC_PANE clones the source pane's scrollback into the
		// target pane. tmux still puts the target in copy-mode; the
		// scrollback content is what changes.
		args = append(args, "-s", srcPane)
	}
	args = append(args, "-t", target)
	if _, err := c.run(ctx, args...); err != nil {
		// tmux phrases the missing-target case as "can't find pane" /
		// "can't find window" depending on which component is absent
		// and which version is on PATH. run() only auto-wraps the
		// "session not found" phrasing, so translate the pane / window
		// variants here so callers can errors.Is into
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
