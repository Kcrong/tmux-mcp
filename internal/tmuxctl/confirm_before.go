package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// isNoServerOrClientMsg recognises the stderr tmux emits when
// `confirm-before` runs against a server with nothing for it to ask:
// either no tmux server is up at all ("no server running on …" /
// "error connecting to …") or the server is up but no client is
// currently attached ("no current client"). Each of those phrasings
// means the same thing for confirm-before — there is no terminal
// available to display the y/n prompt — so the controller maps the
// whole family to one typed sentinel rather than asking callers to
// substring-match stderr themselves.
//
// Detection is by message text rather than exit-code shape because
// every confirm-before failure exits non-zero; the message is the
// only signal that pins down which "no client to ask" branch tmux
// hit. Matching is case-insensitive so a hypothetical future tmux
// version that capitalises differently still trips the branch.
func isNoServerOrClientMsg(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "no current client") ||
		strings.Contains(lower, "no server running") ||
		strings.Contains(lower, "error connecting")
}

// ConfirmBefore drives `tmux confirm-before [-p prompt] [-t target-client] command`.
// tmux pops a y/n confirmation prompt up in the matching client and
// only runs `command` if the user accepts. The wrapper is a single
// fire-and-forget invocation so an agent can stage destructive ops
// without making the tmux UI silently auto-execute.
//
// Argument shape:
//   - command is REQUIRED; tmux refuses confirm-before without one and
//     so do we (an empty command would surface a generic tmux usage
//     error that is far less actionable than this explicit guard).
//   - prompt is optional; tmux falls back to its default
//     "Confirm 'CMD'? (y/n)" phrasing when it is empty, so the
//     wrapper omits `-p` rather than passing an empty argument.
//   - target is optional; with target == "" tmux pops the prompt in
//     the caller's "current" client. On the headless tmux servers
//     tmux-mcp owns there is typically no client at all, which tmux
//     surfaces as one of "no current client" / "no server running"
//     / "error connecting to …" stderr.
//
// Error mapping (load-bearing for the JSON-RPC layer):
//   - "no current client" / "no server running" / "error connecting"
//     → wrap into errs.ErrSessionNotFound. The headless contract is
//     deliberately NOT idempotent here: there is no client to ask, so
//     returning a typed sentinel lets callers branch on the case
//     instead of mistakenly treating it as a successful no-op.
//   - "can't find client" with an explicit target → also
//     errs.ErrSessionNotFound. Same code, same caller branch — the
//     "named target does not exist" failure mode shared with
//     list_clients / session_kill.
//
// Other tmux failures fall through verbatim so the dispatcher's
// generic CodeInternal path handles them.
func (c *Controller) ConfirmBefore(ctx context.Context, prompt, target, command string) error {
	if command == "" {
		return errors.New("command required")
	}
	args := []string{"confirm-before"}
	if prompt != "" {
		args = append(args, "-p", prompt)
	}
	if target != "" {
		args = append(args, "-t", target)
	}
	args = append(args, command)
	if _, err := c.run(ctx, args...); err != nil {
		// Headless server / no attached terminal: tmux has nothing to
		// ask, so wrap into the typed sentinel so the JSON-RPC layer
		// can surface CodeSessionNotFound. We deliberately do NOT
		// swallow this as nil — confirm-before is interactive by
		// nature; a silent success would let an agent believe the
		// destructive command was queued when in fact nobody saw the
		// prompt.
		if isNoServerOrClientMsg(err.Error()) && !errors.Is(err, errs.ErrSessionNotFound) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		// Named-but-missing client: same wrapping so callers see one
		// stable code for "the target you named is not attached".
		if target != "" && !errors.Is(err, errs.ErrSessionNotFound) && isClientMissingMsg(err.Error()) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}
