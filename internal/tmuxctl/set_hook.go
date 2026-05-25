package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// SetHook wraps `tmux set-hook` — the verb that binds a tmux command
// to a server / session-scoped event such as `pane-died`,
// `client-attached`, `session-created`. The boundary layer
// (server-tool) is responsible for validating the regex/length shape of
// `name` and `command`; this method passes both through to tmux verbatim
// and is the single place argv assembly lives so a future maintainer
// changing the flag order only has to touch one spot.
//
// Argument flavours:
//
//   - `unset=true` clears whatever command is bound to the hook (`-u`).
//     `command` is ignored on this path because tmux's set-hook -u takes
//     no command argument; we keep the parameter on the Go signature so
//     a caller that flips `unset` doesn't have to also remember to blank
//     out `command`.
//   - `global=true` binds the hook on the server-wide options table
//     (`-g`), which is what most agents want for "every session that
//     ever exists" semantics. `target` is ignored on this path because
//     `-g` and `-t` are mutually exclusive on tmux's argv.
//   - Otherwise the hook lands on the per-session options of the
//     resolved `target` session (`-t TARGET`). `target` must be
//     non-empty in this branch — without it tmux would resolve "" to
//     whatever session it considers current, which is almost never what
//     the caller actually wanted.
//
// Error mapping:
//
//   - missing target session: surfaced via run() as a wrapped
//     errs.ErrSessionNotFound (the underlying message is
//     "can't find session: <target>" which isSessionMissingMsg already
//     recognises) so the JSON-RPC dispatcher maps it to
//     CodeSessionNotFound. We also catch tmux's "session not found"
//     phrasing on the unset branch (older tmux releases use a slightly
//     different stderr template when -u + -t hits an unknown target)
//     and fold it into the same sentinel.
//   - empty hook name: rejected up front as a plain error. The boundary
//     should never let an empty value through, but defending here keeps
//     the controller usable from tests and ad-hoc callers.
//   - empty command on the bind path (unset=false): rejected up front
//     for the same reason. tmux would otherwise fail with a less
//     helpful "command required" stderr the caller would have to
//     substring-match.
//   - empty target on the per-session bind path (unset=*, global=false):
//     rejected up front. tmux's "" → current-session resolution would
//     bind the hook on whatever session the daemon last touched, which
//     would silently mis-route a deployment script's hook against a
//     stale target.
func (c *Controller) SetHook(ctx context.Context, name, command, target string, unset, global bool) error {
	if name == "" {
		return errors.New("hook name required")
	}
	if !unset && command == "" {
		// On the bind path tmux requires a command. Reject up front so
		// the JSON-RPC layer maps to CodeInvalidParams via the wrapping
		// handler instead of surfacing tmux's own stderr.
		return errors.New("hook command required")
	}
	if !global && target == "" {
		return errors.New("hook target required when not global")
	}
	args := []string{"set-hook"}
	if unset {
		args = append(args, "-u")
	}
	if global {
		args = append(args, "-g")
	} else {
		// Per-session bind: -t TARGET is the only argv shape tmux
		// accepts here. -g and -t are mutually exclusive so the
		// global=true branch above already returned without taking
		// this fork.
		args = append(args, "-t", target)
	}
	args = append(args, name)
	if !unset {
		// On the bind path the command is the final positional argument.
		// On the unset path tmux's set-hook -u takes no command argument
		// at all; appending one would surface as a "too many arguments"
		// stderr.
		args = append(args, command)
	}
	if _, err := c.run(ctx, args...); err != nil {
		// run() already maps "can't find session" / "no such session"
		// to errs.ErrSessionNotFound. tmux 3.4 surfaces some
		// missing-target shapes via the underlying window-options
		// machinery instead — "no such window: <target>" — because
		// hooks live in the per-window options table. Fold those into
		// the same sentinel so callers can errors.Is against
		// ErrSessionNotFound regardless of which exact phrase tmux
		// emitted.
		//
		// Likewise, unsetting an unknown hook surfaces as
		// "invalid option: <hook>" on the unset path; map it to the
		// same sentinel so an agent that fat-fingers the hook name
		// sees the documented missing-target wire code instead of a
		// generic internal error.
		msg := strings.ToLower(err.Error())
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			(strings.Contains(msg, "no such window") ||
				strings.Contains(msg, "invalid option") ||
				strings.Contains(msg, "unknown hook") ||
				strings.Contains(msg, "no such hook")) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}
