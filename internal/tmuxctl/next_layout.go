package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// NextLayout cycles the targeted window onto the next preset layout
// via `tmux next-layout -t <target>`. tmux walks the ordered preset
// ring (even-horizontal → even-vertical → main-horizontal →
// main-vertical → tiled) and wraps around at the end, so calling this
// repeatedly always lands on a valid arrangement. Pairs with
// [Controller.SelectLayout] (PR #115) — that one takes a SPECIFIC
// layout name, this one is the "give me the next preset" affordance an
// agent reaches for when it doesn't care which layout, just wants to
// rotate.
//
// target identifies the window the layout applies to. tmux accepts
// either a session name (in which case the session's active window is
// used) or the full `<session>:<window>` form. The boundary keeps the
// argument shape simple and forwards target verbatim — validation of
// the regex/length policy lives at the JSON-RPC layer.
//
// A missing session/window surfaces as a wrapped errs.ErrSessionNotFound
// so the JSON-RPC layer maps it to CodeSessionNotFound the same way
// every other window-bearing controller method does. tmux's
// `next-layout -t <target>` rejects an unknown session/window with
// "can't find session" (which run() already translates) or sometimes
// the rarer "can't find window" / "no current target" phrasing —
// translate the latter forms here so callers can errors.Is into a
// single sentinel regardless of which message tmux emitted.
func (c *Controller) NextLayout(ctx context.Context, target string) error {
	if target == "" {
		return errors.New("target required")
	}
	if _, err := c.run(ctx, "next-layout", "-t", target); err != nil {
		// tmux's next-layout -t <missing> emits either "can't find
		// window" (when -t names a window target) or "no current
		// target" (when there is no active window/session pointer to
		// resolve). run() already translates "can't find session"; the
		// other two phrasings need folding here so the JSON-RPC layer
		// maps the failure consistently with select_layout / next_window.
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
