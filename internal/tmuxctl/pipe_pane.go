package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// PipePane wraps `tmux pipe-pane [-IO] -t <target> [shell-command]`. The
// canonical use case is logging a pane's output through a long-running
// shell pipeline — `tmux pipe-pane -t demo:0 'cat > /tmp/demo.log'`
// hands every byte tmux writes to the pty over to the operator-supplied
// command without disturbing the running pane.
//
// shellCommand is the optional pipeline to run. When empty, tmux receives
// a bare `pipe-pane` (with only the target flag and the orientation
// flags), which is tmux's documented way to tear down any existing pipe
// on that pane — i.e. "stop logging". When non-empty, tmux executes it
// via /bin/sh -c on the tmux side, so shell quoting rules apply on the
// receiving side. The COMMAND ITSELF IS NOT SANDBOXED by us — that is a
// documented operator-trust boundary; the boundary layer is responsible
// for length / control-char hygiene up front.
//
// outputOnly maps to `-O`: pipe only output written by tmux (i.e. the
// pty's stdout), not input typed at the pane. alsoInput maps to `-I`:
// also pipe input. The two flags can be combined; tmux interprets the
// combination as "both directions". When neither is set, the default
// tmux semantics apply — output-only piping (the surface most agents
// reach for when they want a build log).
//
// A missing target surfaces as a wrapped errs.ErrSessionNotFound so the
// JSON-RPC dispatcher maps it to CodeSessionNotFound — the same contract
// every other tmuxctl method upholds. tmux phrases the missing-target
// case as "can't find pane" or "no current target" depending on the
// shape of target; translate both into the same typed sentinel run()
// emits for "session not found" so callers can errors.Is into
// errs.ErrSessionNotFound regardless of which variant tmux happened to
// emit.
func (c *Controller) PipePane(ctx context.Context, target, shellCommand string, outputOnly, alsoInput bool) error {
	if target == "" {
		return errors.New("target required")
	}
	args := []string{"pipe-pane"}
	if outputOnly {
		args = append(args, "-O")
	}
	if alsoInput {
		args = append(args, "-I")
	}
	args = append(args, "-t", target)
	if shellCommand != "" {
		// Forward shellCommand as a single trailing argv. tmux's man page
		// describes `[shell-command]` as the optional last positional
		// argument, and pipe-pane wraps it in /bin/sh -c on the tmux
		// side — we deliberately do not pre-quote it here so a command
		// like `cat > /tmp/out` lands at tmux exactly the way the caller
		// wrote it. The server boundary already rejects newlines / NUL /
		// other control chars so a single argv entry maps cleanly.
		args = append(args, shellCommand)
	}
	if _, err := c.run(ctx, args...); err != nil {
		msg := strings.ToLower(err.Error())
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			(strings.Contains(msg, "can't find pane") ||
				strings.Contains(msg, "no current target")) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}
