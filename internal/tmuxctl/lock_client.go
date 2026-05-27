package tmuxctl

import (
	"context"
	"errors"
	"fmt"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// LockClient drives `tmux lock-client [-t <client>]`. Without a `-t`
// target tmux locks the caller's "current" client (which, for the
// headless tmux servers tmux-mcp owns, is typically nothing at all).
// With a `-t <client>` target tmux locks just that one attached
// terminal.
//
// Surface intent: this is the single-client counterpart to a
// session-scoped lock. A future `lock_session` tool would lock every
// client attached to a named session; LockClient either targets one
// specific attached client by its TTY-path name (the value
// `list_clients` reports as `tty`) or, with `client == ""`, asks tmux
// to lock the caller's current client — which on a headless server
// has no attached terminal and so cleanly no-ops.
//
// Empty `client` is the common case for the headless tmux servers
// tmux-mcp owns — there are typically no attached terminals at all,
// and `lock-client` with no -t emits "no current client" stderr in
// that case (same phrasing `refresh-client` uses). We treat that exact
// stderr as a successful no-op so callers can fire-and-forget the lock
// without having to first run `list-clients` to know whether there is
// anything to lock.
//
// A non-empty `client` that does not match an attached terminal
// surfaces as a wrapped errs.ErrSessionNotFound — the same typed
// sentinel list_clients / session_kill use for "named target does not
// exist" — so the JSON-RPC layer maps the failure to
// CodeSessionNotFound (-32000) without needing a lock-specific error
// vocabulary.
//
// The `isNoCurrentClientMsg` / `isClientMissingMsg` matchers used here
// live in `detach_client.go` so every client-scoped primitive
// (detach_client, lock_client, …) shares one stderr-text vocabulary
// for the headless / missing-target cases.
func (c *Controller) LockClient(ctx context.Context, client string) error {
	args := []string{"lock-client"}
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
