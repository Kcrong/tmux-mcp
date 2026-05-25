package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// isSwitchNoCurrentClientMsg recognises the stderr tmux emits when
// `switch-client` runs without a `-c` target on a server that has no
// attached clients ("no current client"). The headless tmux servers
// tmux-mcp owns produce this in the common "agent asked to redirect a
// client, nobody is watching" case — treating it as a successful no-op
// keeps callers from having to substring-match stderr to distinguish
// the legitimate-empty case from a real failure.
//
// Detection is by message text rather than exit-code shape because
// every switch-client failure exits non-zero; the message is the only
// signal that pins down which failure mode tmux hit. Helper is uniquely
// named so it can coexist with sibling matchers (refresh_client,
// lock_client) that ship the same predicate under their own name.
func isSwitchNoCurrentClientMsg(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "no current client")
}

// isSwitchClientMissingMsg recognises tmux's "can't find client: <name>"
// stderr when a `-c <client>` target does not match any attached
// client. Detect it broadly across versions so the dispatcher can map
// the failure onto errs.ErrSessionNotFound — the typed sentinel the
// JSON-RPC layer already wires to CodeSessionNotFound (-32000), which
// the rest of the surface (session_kill, list_clients, …) reuses for
// "named target does not exist". Sharing the code keeps clients from
// having to learn a per-tool failure vocabulary.
func isSwitchClientMissingMsg(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "can't find client")
}

// SwitchClient drives `tmux switch-client [-c <client>] [-t <target>]
// [-l|-n|-p] [-r]`. It rebinds an attached client's current session to
// a different one (the `-t <target>` form), or moves it to the last,
// next, or previous session via the directional flags. With `-r` tmux
// also toggles the client's read-only / ignore-size flags. When
// `client` is empty tmux defaults to the caller's current client —
// which on the headless servers tmux-mcp owns is typically nothing at
// all, so the empty case maps to a successful no-op.
//
// Argument contract:
//   - exactly one of {target != "", last, next, prev} must hold; the
//     wrapper enforces this so a malformed call surfaces as a clean
//     error rather than tmux's confusing exit-status-1 with no stderr;
//   - target != "" → `-t TARGET`;
//   - last → `-l`, next → `-n`, prev → `-p`; these three are mutually
//     exclusive on the wire, and combining any of them with target is
//     rejected (tmux would silently ignore the directional flag in
//     that case);
//   - readOnly → `-r`, applied independently of the directional choice.
//
// Surface intent: this is the primitive an agent uses to redirect an
// already-attached client between sessions on the same server — for
// example, "park the operator's terminal on the build session while a
// long compile runs, then bounce them back to the editor session when
// it's done". A future `pane` form (target containing ':', '.' or
// '%') would land here too without changing the wire shape; we accept
// the same string tmux does and let the daemon validate the syntax.
//
// Empty `client` is the common case for headless servers — there are
// typically no attached terminals at all, and `switch-client` with no
// -c emits "no current client" stderr in that case (same phrasing
// `refresh-client` and `lock-client` use). We treat that exact stderr
// as a successful no-op so callers can fire-and-forget the switch
// without having to first run `list-clients` to know whether there is
// anything to redirect.
//
// A non-empty `client` that does not match an attached terminal
// surfaces as a wrapped errs.ErrSessionNotFound — the same typed
// sentinel list_clients / session_kill use for "named target does not
// exist" — so the JSON-RPC layer maps the failure to
// CodeSessionNotFound (-32000) without needing a switch-specific
// error vocabulary. A non-existent target session likewise wraps
// ErrSessionNotFound via the controller's run() machinery.
func (c *Controller) SwitchClient(ctx context.Context, client, target string, last, next, prev, readOnly bool) error {
	// Exactly-one-of validation. We check at this layer (not just the
	// JSON-RPC handler) so direct callers from inside the package see
	// the same contract — and so a future tool that wraps SwitchClient
	// without going through the dispatcher cannot bypass the rule.
	chosen := 0
	if target != "" {
		chosen++
	}
	if last {
		chosen++
	}
	if next {
		chosen++
	}
	if prev {
		chosen++
	}
	if chosen == 0 {
		return errors.New("switch_client: exactly one of {target, last, next, prev} must be set")
	}
	if chosen > 1 {
		return errors.New("switch_client: target/last/next/prev are mutually exclusive")
	}

	args := []string{"switch-client"}
	if client != "" {
		args = append(args, "-c", client)
	}
	if last {
		args = append(args, "-l")
	}
	if next {
		args = append(args, "-n")
	}
	if prev {
		args = append(args, "-p")
	}
	if readOnly {
		args = append(args, "-r")
	}
	if target != "" {
		args = append(args, "-t", target)
	}
	if _, err := c.run(ctx, args...); err != nil {
		// Empty client + no attached terminals is the "nothing to do"
		// case for a headless server; map it to nil so the caller sees
		// a clean success rather than having to match stderr text.
		// Only safe when the caller did not name a client: a named
		// client missing is a real not-found case the next branch
		// handles separately.
		if client == "" && isSwitchNoCurrentClientMsg(err.Error()) {
			return nil
		}
		// Named-but-missing client: wrap into errs.ErrSessionNotFound
		// so the dispatcher's existing CodeSessionNotFound path catches
		// it. Guard against double-wrapping in case run() ever starts
		// recognising the message itself.
		if client != "" && !errors.Is(err, errs.ErrSessionNotFound) && isSwitchClientMissingMsg(err.Error()) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}
