package tmuxctl

import (
	"context"
	"errors"
	"fmt"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// isNoCurrentClientMsg / isClientMissingMsg are shared with the
// detach-client matcher in detach_client.go — identical semantics, and
// the comment block in that file is the canonical reference. Defining
// them once at package scope avoids the symbol redeclaration this
// branch hits when it lands after PR #143 (detach_client) on main.

// RefreshClient drives `tmux refresh-client [-S] [-t <client>]`. Without
// a client target the command runs against every attached client; with
// statusOnly=true the `-S` flag asks tmux to redraw only the status
// line (cheaper than a full screen redraw).
//
// Surface intent: this is the primitive an agent uses after rewriting
// a client-rendered option (status-format, status-style, window-status-
// format, …) so the change takes effect on the visible terminal
// immediately rather than waiting for the next tmux render tick.
//
// Empty `client` is the common case for the headless tmux servers
// tmux-mcp owns — there are typically no attached terminals at all,
// and `refresh-client` with no -t emits "no current client" stderr
// in that case. We treat that exact stderr as a successful no-op so
// callers can fire-and-forget the refresh without having to first run
// `list-clients` to know whether there is anything to refresh.
//
// A non-empty `client` that does not match an attached terminal
// surfaces as a wrapped errs.ErrSessionNotFound — the same typed
// sentinel list_clients / session_kill use for "named target does not
// exist" — so the JSON-RPC layer maps the failure to CodeSessionNotFound
// (-32000) without needing a refresh-specific error vocabulary.
func (c *Controller) RefreshClient(ctx context.Context, client string, statusOnly bool) error {
	args := []string{"refresh-client"}
	if statusOnly {
		args = append(args, "-S")
	}
	if client != "" {
		args = append(args, "-t", client)
	}
	if _, err := c.run(ctx, args...); err != nil {
		// Empty client + no attached terminals is the "nothing to do"
		// case for a headless server; map it to nil so the caller sees
		// a clean success rather than having to match stderr text.
		if client == "" && isNoCurrentClientMsg(err.Error()) {
			return nil
		}
		// Named-but-missing client: wrap into errs.ErrSessionNotFound
		// so the dispatcher's existing CodeSessionNotFound path catches
		// it. Guard against double-wrapping in case run() ever starts
		// recognising the message itself.
		if client != "" && !errors.Is(err, errs.ErrSessionNotFound) && isClientMissingMsg(err.Error()) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}
