package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// isNoCurrentClientMsg recognises the stderr tmux emits when
// `detach-client` (or `refresh-client`, `lock-client`, …) runs without
// a `-t`/`-s` target on a server that has no attached clients ("no
// current client"). The headless tmux servers tmux-mcp owns produce
// this in the common "agent fired a client-side primitive, nobody is
// watching" case — treating it as a successful no-op keeps callers
// from having to substring-match stderr to distinguish the legitimate-
// empty case from a real failure.
//
// Detection is by message text rather than exit-code shape because
// every detach-client failure exits non-zero; the message is the only
// signal that pins down which failure mode tmux hit.
//
// Note: sibling matchers exist in PR #110 (refresh_client) and PR #123
// (lock_client). When this branch rebases against whichever lands
// first, dedupe to a single shared matcher.
func isNoCurrentClientMsg(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "no current client")
}

// isClientMissingMsg recognises tmux's "can't find client: <name>"
// stderr when a `-t <client>` target does not match any attached
// client. Detect it broadly across versions so the dispatcher can map
// the failure onto errs.ErrSessionNotFound — the typed sentinel the
// JSON-RPC layer already wires to CodeSessionNotFound (-32000), which
// the rest of the surface (session_kill, list_clients, …) reuses for
// "named target does not exist". Sharing the code keeps clients from
// having to learn a per-tool failure vocabulary.
//
// "can't find session" is already recognised by run()'s
// isSessionMissingMsg, so the wrapping happens automatically inside
// run(); we only need to handle the client-name variant explicitly.
func isClientMissingMsg(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "can't find client")
}

// DetachClient drives `tmux detach-client [-a] [-s <session>] [-t <client>]`.
// It cleanly ends a tmux client's connection so its terminal is
// released, distinct from `kill_server` (which tears down the whole
// daemon) and `lock_client` (which holds the client but keeps the
// connection).
//
// Flag mapping:
//   - client != ""  → -t CLIENT (detach this one client)
//   - session != "" → -s SESSION (detach every client attached to this session)
//   - all == true   → -a (detach every OTHER client; only meaningful
//     when paired with `client`, where it inverts the
//     selection to "everyone except CLIENT")
//
// Caller contract: at least one of `client`, `session`, or `all` must
// be set. Empty strings on `client` / `session` mean "absent" — a bare
// DetachClient(ctx, "", "", false) is rejected up front rather than
// dispatched as `tmux detach-client` (which would target the caller's
// "current" client, a concept that does not exist on the headless
// servers tmux-mcp owns and would otherwise emit "no current client"
// stderr for every empty call). Validation lives at the controller
// boundary so a future caller bypassing the JSON-RPC layer (tests,
// other tools embedding the package) cannot accidentally hit the
// "current client" path.
//
// "no current client" stderr is still treated as a successful no-op:
// tmux emits that phrase whenever it can't find anything to detach —
// when nothing is attached server-wide (`-a` / no flags), AND when
// `-s SESSION` resolves but has zero attached clients, AND when
// `-s SESSION` names a session that does not exist (tmux's argument
// resolver folds "no such session" into "no current client" for
// detach-client specifically). Mapping all three onto nil keeps a
// fire-and-forget detach (e.g. "kick everyone out of session foo, if
// it exists and has anyone") looking like a clean success rather than
// an error the caller must substring-match.
//
// This makes the missing-target error contract deliberately asymmetric
// between the `-t CLIENT` and `-s SESSION` branches: a missing client
// surfaces as a wrapped errs.ErrSessionNotFound (so the dispatcher can
// map it to CodeSessionNotFound, mirroring list_clients /
// session_kill), but a missing session does NOT — because tmux itself
// does not distinguish that case from the legitimate-empty case. A
// future caller that needs strict missing-session semantics can
// pre-flight `has-session` before calling DetachClient.
func (c *Controller) DetachClient(ctx context.Context, client, session string, all bool) error {
	if client == "" && session == "" && !all {
		return fmt.Errorf("detach-client: at least one of client, session, or all must be set")
	}
	args := []string{"detach-client"}
	if all {
		args = append(args, "-a")
	}
	if session != "" {
		args = append(args, "-s", session)
	}
	if client != "" {
		args = append(args, "-t", client)
	}
	if _, err := c.run(ctx, args...); err != nil {
		// Headless server with nothing attached: tmux emits "no current
		// client" even when `-a`/`-s` would otherwise scope the action.
		// Map it onto nil so a fire-and-forget detach against an empty
		// roster looks like a clean success.
		if isNoCurrentClientMsg(err.Error()) {
			return nil
		}
		// Named-but-missing client: wrap into errs.ErrSessionNotFound so
		// the dispatcher's existing CodeSessionNotFound path catches it.
		// "can't find session" is already wrapped inside run() via
		// isSessionMissingMsg so we don't need to handle that branch
		// here. Guard against double-wrapping in case run() ever starts
		// recognising the client-name variant itself.
		if !errors.Is(err, errs.ErrSessionNotFound) && isClientMissingMsg(err.Error()) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}
