package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// PreviousWindow moves the targeted session's active window pointer one
// slot backward via `tmux previous-window -t <target>`. tmux wraps from
// index 0 to the highest-numbered window so a session sitting on its
// first window does not refuse the call — it lands on the last one
// instead. Useful for an agent stepping backward through a sequence of
// sibling windows without having to enumerate them via list_windows
// first.
//
// withAlert maps to tmux's `-a` flag: when true, tmux skips windows that
// are not flagged with an alert (`#{window_silence_flag}` /
// `#{window_bell_flag}` / `#{window_activity_flag}` are off) and lands
// on the previous one that *does* carry an alert. Most agents leave
// this false; the flag exists for parity with the underlying CLI so a
// future "step to the previous noisy window" use case has a path.
//
// A missing session surfaces as a wrapped errs.ErrSessionNotFound (via
// run() / the same "can't find window/session" translation other window
// methods perform) so the JSON-RPC layer maps it to
// CodeSessionNotFound. Mirrors NextWindow's contract — the two are
// siblings and an agent that stitches them together sees the same
// error shapes.
func (c *Controller) PreviousWindow(ctx context.Context, target string, withAlert bool) error {
	if target == "" {
		return errors.New("target required")
	}
	args := []string{"previous-window", "-t", target}
	if withAlert {
		// -a means "step to the previous *alert-flagged* window". Append
		// at the end so the argv order stays easy to diff against tmux's
		// man page (`previous-window [-a] [-t target-session]`).
		args = append(args, "-a")
	}
	if _, err := c.run(ctx, args...); err != nil {
		// `tmux previous-window -t <missing>` rejects an unknown session
		// with "can't find session" (translated by run()) on most builds,
		// but some emit "can't find window" because -t is a window-target
		// in spirit. Translate the latter so callers can errors.Is into
		// errs.ErrSessionNotFound regardless of which message tmux
		// emitted, mirroring SelectWindow / SwapWindow.
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find window") {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}
