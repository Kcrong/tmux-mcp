package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// CustomizeMode wraps `tmux customize-mode [-N] [-Z] [-t TARGET-PANE]
// [-F FORMAT] [-f FILTER]`. tmux's customize-mode opens the interactive
// options/key-bindings editor in the target pane: the pane enters mode
// (verifiable via `display-message #{?pane_in_mode,1,0}`) and the user
// can browse / tweak server, session, window, and pane options through
// the same UI tmux exposes from `:customize-mode` on the command line.
//
// target uses tmux's standard pane-target form (e.g. "demo:0.1",
// "demo:0", "demo") — the boundary (server tool) is responsible for
// the up-front regex/length check; the controller passes the value
// verbatim to tmux. An empty target is intentionally accepted: tmux
// resolves the missing -t to the current/active pane, so callers that
// want to pop the editor on whichever pane the server considers
// focused can simply omit the argument.
//
// Optional knobs map straight onto tmux flags:
//
//   - noClose=true → `-N` (keep the mode open after a selection
//     completes; otherwise tmux exits the editor on each commit).
//   - zoom=true    → `-Z` (zoom the target pane to fill the window
//     while the editor is up).
//   - format       → `-F FORMAT` (tmux format DSL controlling how each
//     row is rendered in the editor).
//   - filter       → `-f FILTER` (predicate string that hides rows for
//     which the expression evaluates to false; same DSL as the rest
//     of tmux's filter knobs).
//
// Empty format / filter strings are skipped so the boundary can leave
// either argument out and inherit tmux's defaults.
//
// Error mapping mirrors the rest of the surface so the JSON-RPC layer
// can translate uniformly:
//
//   - "can't find pane" / "no current target" / "can't find window" /
//     "can't find session" → wrapped errs.ErrSessionNotFound. The
//     pane the caller named (or the current pane the empty target
//     was meant to resolve to) is gone, so this is the equivalent of
//     "session not found" for a pane-targeted call.
//   - "no server running" / "error connecting" → also wrapped
//     errs.ErrSessionNotFound. Without a tmux server there is no pane
//     to operate on, so this is a hard miss rather than an
//     idempotent-empty success: a customize-mode call expects to
//     enter the editor in some pane, and "no server" means no such
//     pane exists.
func (c *Controller) CustomizeMode(ctx context.Context, target, format, filter string, noClose, zoom bool) error {
	args := []string{"customize-mode"}
	if noClose {
		args = append(args, "-N")
	}
	if zoom {
		args = append(args, "-Z")
	}
	if target != "" {
		args = append(args, "-t", target)
	}
	if format != "" {
		args = append(args, "-F", format)
	}
	if filter != "" {
		args = append(args, "-f", filter)
	}
	if _, err := c.run(ctx, args...); err != nil {
		// tmux phrases the missing-target case several ways depending on
		// the shape of the target string and the version on PATH. The
		// "can't find session" form is already wrapped by run() via the
		// isSessionMissingMsg gate; translate the rest into the same
		// typed sentinel so callers can errors.Is into
		// errs.ErrSessionNotFound regardless of which variant tmux
		// emitted.
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
