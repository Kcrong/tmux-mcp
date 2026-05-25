package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// NextWindow advances the session's active window pointer to the next
// window via `tmux next-window -t <target>`. tmux walks the session's
// window list in index order and wraps around at the end, so calling
// this on the last window lands on the first one. Pairs with
// [Controller.SelectWindow] (jump to a specific target) by offering the
// "step forward" idiom an agent reaches for when it does not know the
// concrete next index up front.
//
// withAlert maps to tmux's `-a` flag: when true, tmux skips past
// windows that have not raised a monitor-activity / monitor-bell alert
// and lands on the next one that has. This is the same semantics the
// interactive `next-window -a` keybinding produces and is the load-
// bearing reason an agent would steer this knob — without it a session
// with many idle windows is stepped through one-by-one, with it the
// pointer hops directly to whichever window has new activity.
//
// A missing session surfaces as a wrapped errs.ErrSessionNotFound so
// the JSON-RPC layer maps it to CodeSessionNotFound the same way every
// other window-side method does. tmux's `next-window -t <session>`
// rejects an unknown session with "can't find session", which run()
// already translates to errs.ErrSessionNotFound; we additionally
// translate the rare "can't find window" phrasing some tmux builds
// emit when the underlying server has not yet started, so callers can
// errors.Is into a single sentinel regardless of which message tmux
// happened to produce.
func (c *Controller) NextWindow(ctx context.Context, target string, withAlert bool) error {
	if target == "" {
		return errors.New("target required")
	}
	args := []string{"next-window", "-t", target}
	if withAlert {
		// -a means "skip to the next window with an activity alert". We
		// append it after -t so the argv shape mirrors the man page
		// (`next-window [-a] [-t target]`), which keeps a future diff
		// against tmux's documentation easy to scan.
		args = append(args, "-a")
	}
	if _, err := c.run(ctx, args...); err != nil {
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find window") {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}
