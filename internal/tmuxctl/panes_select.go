package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// SelectPaneOptions captures the optional flags `tmux select-pane`
// understands beyond the bare `-t TARGET`. The boundary's `select_pane`
// tool wraps this struct one-to-one so a caller can mark / unmark the
// target, jump back to the last-active pane, walk to a directional
// neighbour, toggle input, or zoom the window — all in a single call.
//
// Semantics mirror tmux's own flag set:
//
//   - Mark / Unmark:   `-m` / `-M` — set or clear this pane's "marked"
//     state, used as the implicit source for swap-pane / join-pane.
//   - Last:            `-l` — jump back to the most recently active pane.
//   - Direction:       `-U` / `-D` / `-L` / `-R` — walk one step toward
//     the named neighbour. The string forms ("up" / "down" / "left" /
//     "right") match the rest of the surface (pane_resize) so callers
//     don't need a second vocabulary.
//   - EnableInput / DisableInput: `-e` / `-d` — toggle whether the pane
//     accepts keyboard input. Useful for parking a side-car build pane
//     while the agent types into the foreground pane.
//   - Zoom:            `-Z` — toggle the window's zoom state on the
//     selected pane. Combines with the rest of the flags so a caller can
//     "select pane X and zoom it" in one round trip.
//
// The controller refuses obviously-conflicting combinations
// (Mark+Unmark, EnableInput+DisableInput) and an empty Target — tmux's
// own diagnostics for those cases land far from the operator's mistake.
type SelectPaneOptions struct {
	Target       string
	Mark         bool
	Unmark       bool
	Last         bool
	Direction    string
	EnableInput  bool
	DisableInput bool
	Zoom         bool
}

// selectPaneDirectionFlag maps the public direction string onto the tmux
// flag that selects the directional neighbour. Mirrors
// resizeDirectionFlag's contract: empty means "no directional flag" and
// any other value is rejected so a future alias cannot slip through.
func selectPaneDirectionFlag(direction string) (string, error) {
	switch direction {
	case "":
		return "", nil
	case "up":
		return "-U", nil
	case "down":
		return "-D", nil
	case "left":
		return "-L", nil
	case "right":
		return "-R", nil
	default:
		return "", fmt.Errorf("direction %q must be \"up\", \"down\", \"left\", or \"right\"", direction)
	}
}

// SelectPaneAdvanced wraps `tmux select-pane` with the full set of
// optional flags exposed by SelectPaneOptions. Compared to the simpler
// SelectPane(target) entry point, this method lets the caller mark /
// unmark the pane, jump to the last-active or a directional neighbour,
// toggle input, and zoom the window — all atomic on tmux's side. The
// boundary tool (`select_pane`) is the only intended consumer; legacy
// callers that just want "make this pane active" should keep using
// SelectPane.
//
// A missing pane / session surfaces as a wrapped errs.ErrSessionNotFound
// so the JSON-RPC dispatcher maps it to CodeSessionNotFound — same
// contract every other pane-scoped tmuxctl method upholds. tmux phrases
// the missing-target case as "can't find pane" rather than the
// "session not found" run() already maps; translate that explicitly so
// callers can errors.Is into the typed sentinel regardless of which
// variant tmux emitted.
func (c *Controller) SelectPaneAdvanced(ctx context.Context, opts SelectPaneOptions) error {
	if opts.Target == "" {
		return errors.New("target required")
	}
	if opts.Mark && opts.Unmark {
		return errors.New("mark and unmark are mutually exclusive")
	}
	if opts.EnableInput && opts.DisableInput {
		return errors.New("enable_input and disable_input are mutually exclusive")
	}
	dirFlag, err := selectPaneDirectionFlag(opts.Direction)
	if err != nil {
		return err
	}

	args := []string{"select-pane", "-t", opts.Target}
	if opts.Mark {
		args = append(args, "-m")
	}
	if opts.Unmark {
		args = append(args, "-M")
	}
	if opts.Last {
		args = append(args, "-l")
	}
	if dirFlag != "" {
		args = append(args, dirFlag)
	}
	if opts.EnableInput {
		args = append(args, "-e")
	}
	if opts.DisableInput {
		args = append(args, "-d")
	}
	if opts.Zoom {
		args = append(args, "-Z")
	}

	if _, err := c.run(ctx, args...); err != nil {
		// tmux select-pane against a missing pane says "can't find pane"
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
