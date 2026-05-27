package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// SelectLayoutOpts collects the optional flags `tmux select-layout`
// understands beyond the layout-name positional. Each flag maps onto
// exactly one knob in the underlying CLI so the boundary tool can stay
// a thin schema-validated wrapper without growing controller-side
// policy.
type SelectLayoutOpts struct {
	// Next maps to `select-layout -n` (equivalent to next-layout):
	// rotate to the next preset layout in the ordered ring of presets.
	// Mutually exclusive with Previous at the boundary; the controller
	// itself does not enforce that — it just translates flags.
	Next bool
	// Previous maps to `select-layout -p` (equivalent to
	// previous-layout): the inverse cycle direction of Next.
	Previous bool
	// Spread maps to `select-layout -E` ("spread the current pane and
	// any panes next to it out evenly"). Useful as a follow-up to a
	// preset layout when the agent wants tmux to even out a region
	// rather than pick a wholly new layout.
	Spread bool
}

// SelectLayout runs `tmux select-layout -t <target> [flags] [layout]`.
// target is required and identifies the window the layout applies to —
// tmux accepts the standard `<session>:<window>` form here. layout is
// either one of the five preset names ("even-horizontal",
// "even-vertical", "main-horizontal", "main-vertical", "tiled") or a
// previously-stored layout dump string (the value `list-windows`
// surfaces in the `#{window_layout}` format variable). When
// opts.Next/Previous is true the layout argument is typically empty —
// tmux cycles through the preset ring without consulting it — but the
// boundary leaves that policy to the JSON-RPC layer; the controller
// just forwards what it was handed.
//
// A missing session/window surfaces as a wrapped errs.ErrSessionNotFound
// so the JSON-RPC dispatcher maps it to CodeSessionNotFound the same
// way every other window-bearing tool does. tmux phrases the failure
// as "can't find pane" because select-layout's -t names a target-pane
// (any pane in the targeted window suffices), so we translate that
// phrasing into the typed sentinel here — run() itself only catches
// the "can't find session" form.
func (c *Controller) SelectLayout(ctx context.Context, target, layout string, opts SelectLayoutOpts) error {
	if target == "" {
		return errors.New("target required")
	}
	args := []string{"select-layout", "-t", target}
	if opts.Next {
		// -n cycles forward through the preset ring; appending it before
		// any layout positional matches tmux's own argv ordering in the
		// man page, which keeps the diff against the CLI documentation
		// easy to read.
		args = append(args, "-n")
	}
	if opts.Previous {
		// -p cycles backward; same ordering rationale as -n.
		args = append(args, "-p")
	}
	if opts.Spread {
		// -E spreads the current pane and its neighbours out evenly.
		// Independent of -n/-p — tmux happily accepts -nE / -pE — so
		// we don't gate it on the cycle flags here.
		args = append(args, "-E")
	}
	if layout != "" {
		// The layout positional must come after every flag because tmux's
		// option parser stops at the first non-flag token.
		args = append(args, layout)
	}
	if _, err := c.run(ctx, args...); err != nil {
		// `select-layout -t <missing>` emits "can't find pane: <name>"
		// (the -t flag names a pane target on this command), which
		// run() does not translate by itself. Fold it into
		// errs.ErrSessionNotFound so callers can errors.Is regardless
		// of the exact message tmux emitted across versions.
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find pane") {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}
