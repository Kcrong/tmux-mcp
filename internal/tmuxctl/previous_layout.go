package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// PreviousLayout cycles the targeted window's pane arrangement one
// step BACKWARD through tmux's preset ring via
// `tmux previous-layout -t <target>`. tmux ships five presets
// (even-horizontal, even-vertical, main-horizontal, main-vertical,
// tiled) and previous-layout walks them in reverse — wrapping from
// the first preset to the last so the call never refuses on a
// position-edge. Sibling of NextLayout: the two are deliberately
// symmetric so an agent that drives one does not need to relearn the
// contract for the other. Distinct from SelectLayout (which takes a
// concrete preset name or a stored dump): previous_layout is the
// "step backward through presets" affordance and intentionally has no
// payload beyond `target`.
//
// target identifies the window. tmux accepts the standard
// `<session>:<window>` form here — the -t flag on `previous-layout`
// is a target-window in spirit. An empty target is rejected up-front
// so a stray `tmux previous-layout -t ""` is never issued; tmux would
// otherwise interpret it against the current/global state, which is
// almost never what an agent meant.
//
// A missing session/window surfaces as a wrapped errs.ErrSessionNotFound
// so the JSON-RPC dispatcher maps it to CodeSessionNotFound the same
// way every other window-bearing tool does. tmux phrases the failure
// as "can't find window" on most builds (and "can't find pane" on
// some, since the underlying target resolves to a pane), so we
// translate both phrasings into the typed sentinel here — run()
// itself only catches the "can't find session" form.
func (c *Controller) PreviousLayout(ctx context.Context, target string) error {
	if target == "" {
		return errors.New("target required")
	}
	if _, err := c.run(ctx, "previous-layout", "-t", target); err != nil {
		// Fold both "can't find window" and "can't find pane" message
		// shapes into errs.ErrSessionNotFound so callers can errors.Is
		// regardless of the exact phrasing tmux emitted across
		// versions. Mirrors SelectLayout / SelectWindow / SwapWindow.
		if !errors.Is(err, errs.ErrSessionNotFound) {
			low := strings.ToLower(err.Error())
			if strings.Contains(low, "can't find window") ||
				strings.Contains(low, "can't find pane") {
				return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
			}
		}
		return err
	}
	return nil
}
