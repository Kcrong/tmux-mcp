package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// LastPaneOptions controls a [Controller.LastPane] invocation. It maps
// one-for-one onto the surface of `tmux last-pane` so the boundary
// (server tool) is the single point of truth for which combinations are
// well-formed; this layer only wires the flags into argv.
type LastPaneOptions struct {
	// TargetWindow is the tmux `-t` target the last-pane toggle is
	// scoped to. When empty, tmux operates on its idea of the current
	// window — fine for an interactive client, but the server tool
	// always passes a session-qualified target so headless deployments
	// get deterministic behaviour.
	TargetWindow string
	// DisableInput maps to tmux's `-d` flag: when true, input is
	// disabled on the pane that becomes active after the toggle.
	// Mutually exclusive with EnableInput; the boundary rejects callers
	// that set both, so this layer just trusts the input.
	DisableInput bool
	// EnableInput maps to tmux's `-e` flag: when true, input is
	// re-enabled on the pane that becomes active after the toggle.
	// Mutually exclusive with DisableInput.
	EnableInput bool
	// ZoomToggle maps to tmux's `-Z` flag: when true, tmux toggles the
	// zoomed state of the pane that becomes active after the switch.
	ZoomToggle bool
}

// LastPane wraps `tmux last-pane`, switching the active pane of a window
// to whichever pane was previously active. The flags surfaced by
// [LastPaneOptions] mirror tmux's own:
//
//   - `-d` disables input on the newly-selected pane;
//   - `-e` re-enables input on the newly-selected pane;
//   - `-Z` toggles the pane's zoom state;
//   - `-t <target>` scopes the operation to a specific window (default
//     is "the current window of the current client").
//
// The boundary layer enforces the (DisableInput, EnableInput)
// mutually-exclusive contract; this method just trusts the input and
// emits whichever of `-d` / `-e` was requested.
//
// A missing window/session surfaces as a wrapped errs.ErrSessionNotFound
// so the JSON-RPC dispatcher maps it to CodeSessionNotFound — same
// contract as SelectWindow / SwapWindow / SwapPane. tmux's stderr is
// "can't find window" when the `-t` target is unknown, which run() does
// not translate by itself; we fold it into the typed sentinel here so
// callers can errors.Is into the same single error type regardless of
// which exact phrase tmux emitted.
func (c *Controller) LastPane(ctx context.Context, opts LastPaneOptions) error {
	args := []string{"last-pane"}
	if opts.DisableInput {
		args = append(args, "-d")
	}
	if opts.EnableInput {
		args = append(args, "-e")
	}
	if opts.ZoomToggle {
		args = append(args, "-Z")
	}
	if opts.TargetWindow != "" {
		args = append(args, "-t", opts.TargetWindow)
	}
	if _, err := c.run(ctx, args...); err != nil {
		// `tmux last-pane -t <target>` rejects an unknown target with
		// "can't find window" / "can't find pane" depending on the
		// build. Translate either phrasing into the typed
		// errs.ErrSessionNotFound run() uses elsewhere so the JSON-RPC
		// dispatcher maps every "missing" surface to
		// CodeSessionNotFound uniformly.
		if !errors.Is(err, errs.ErrSessionNotFound) {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "can't find window") ||
				strings.Contains(msg, "can't find pane") {
				return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
			}
		}
		return err
	}
	return nil
}
