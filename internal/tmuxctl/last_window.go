package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// LastWindow switches the named session back to its previously-active
// window via `tmux last-window -t <session>`. tmux remembers the last
// active window per session and toggles between the current and the
// remembered slot — the equivalent of the interactive `prefix + l` (or
// custom `Alt-a`) hot key, which agents reach for to flip between two
// related contexts (editor / build, code / repl) without having to
// remember the destination's index or name.
//
// `tmux last-window` rejects an unknown session with "can't find
// session", which the run() helper already translates into a wrapped
// errs.ErrSessionNotFound — but tmux also rejects a session that has
// never seen a second window with "no last window" (i.e. there is
// nothing to toggle to). We pass that case through verbatim so the
// JSON-RPC layer surfaces it as a generic CodeInternal: it is not a
// "session does not exist" failure, just a "the requested toggle is
// undefined here" one, and the caller can decide whether to fall back
// to window_select with an explicit target.
func (c *Controller) LastWindow(ctx context.Context, target string) error {
	if target == "" {
		return errors.New("target required")
	}
	if _, err := c.run(ctx, "last-window", "-t", target); err != nil {
		// `last-window -t <session>` rejects an unknown session/target
		// with "can't find session" (run() already translates that) but
		// some tmux builds emit "can't find window" instead, which run()
		// does not pick up by itself. Translate it so callers can rely
		// on errors.Is(err, errs.ErrSessionNotFound) regardless of which
		// phrasing tmux chose. Importantly, a "no last window" error is
		// *not* folded into the sentinel — that one means the session
		// exists but has never had a second window to toggle to, which
		// is a different failure mode and should surface as-is.
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find window") {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}
