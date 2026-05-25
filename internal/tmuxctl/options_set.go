package tmuxctl

import (
	"context"
	"errors"
	"fmt"
)

// OptionScopePane selects the per-pane option set
// (`tmux set-option -p -t <target>`). Pane options are a tmux 3.4+
// concept and behave like window options scoped to a single pane —
// agents that want to flip a render-time knob (e.g. `remain-on-exit`)
// for a specific pane reach for this scope. Server / session / window
// scopes are defined alongside the show-options helper in options.go.
const OptionScopePane = "pane"

// SetOption wraps `tmux set-option` and applies a single option=value
// pair (or an unset, when unset=true) at the requested scope. The
// scope semantics mirror [Controller.ShowOptions]:
//
//   - scope=[OptionScopeServer]: invokes `set-option -s NAME [VALUE]`.
//     The target argument is ignored — server options have no
//     session/window/pane qualifier and tmux silently rejects -t with
//     "no current target" if one is supplied.
//   - scope=[OptionScopeSession]: invokes
//     `set-option -t TARGET NAME [VALUE]`. target is required and is
//     the session name (or session id) the caller wants to mutate.
//   - scope=[OptionScopeWindow]: invokes
//     `set-option -w -t TARGET NAME [VALUE]`. target is required and
//     names the window (e.g. `session:window` or `@N`).
//   - scope=[OptionScopePane]: invokes
//     `set-option -p -t TARGET NAME [VALUE]`. target is required and
//     names the pane (e.g. `session:window.pane` or `%N`). Pane
//     options are a tmux 3.4+ concept; older builds reject `-p` with
//     "unknown flag" and the call surfaces as a plain internal error.
//
// When unset=true the VALUE positional is omitted and tmux is invoked
// with `-u`, clearing whatever override was set on the target. tmux's
// own contract for `set-option -u NAME` is "if no override exists, do
// nothing and exit 0", so an over-eager unset is a no-op rather than
// an error — matching the behaviour an agent would see from the CLI.
//
// Error mapping:
//   - target session not found: surfaced via run() as a wrapped
//     errs.ErrSessionNotFound (the underlying message
//     "can't find session: <name>" is recognised by the same
//     isSessionMissingMsg detector that powers the rest of the
//     surface) so the JSON-RPC dispatcher maps it to
//     CodeSessionNotFound.
//   - unknown option name: tmux replies "unknown option: <name>" on
//     stderr; the wrapped error surfaces unchanged, mapped to
//     CodeInternal at the boundary.
//   - empty/unsupported scope or empty target on a target-requiring
//     scope: rejected up front as a plain error so direct callers see
//     a clean diagnostic. The boundary already enforces these via the
//     handler's per-scope guard, but the controller defends here too
//     for tests and future call sites that bypass the JSON-RPC layer.
func (c *Controller) SetOption(ctx context.Context, name, value, scope, target string, unset bool) error {
	args, err := buildSetOptionArgs(name, value, scope, target, unset)
	if err != nil {
		return err
	}
	if _, err := c.run(ctx, args...); err != nil {
		return err
	}
	return nil
}

// buildSetOptionArgs assembles the argv passed to `tmux set-option`
// for the requested scope. Split out from [Controller.SetOption] so
// the assembly logic can be unit-tested without spinning up a live
// tmux server.
//
// Validation order matches the boundary's expectations: name first
// (required for every scope), scope next (must be in the supported
// set), then per-scope target requirements. The unset flag is
// orthogonal — when true the VALUE positional is suppressed and `-u`
// is added; when false VALUE is appended verbatim (including the
// empty string, which tmux accepts as a legitimate empty value).
func buildSetOptionArgs(name, value, scope, target string, unset bool) ([]string, error) {
	if name == "" {
		return nil, errors.New("option name required")
	}
	args := []string{"set-option"}
	if unset {
		args = append(args, "-u")
	}
	switch scope {
	case OptionScopeServer:
		// Server-scope set-option ignores -t entirely: tmux server
		// options are global to the process and have no
		// session/window/pane qualifier. Adding -t with a target
		// would either be silently ignored or rejected depending on
		// the tmux version, so we strip it up front.
		args = append(args, "-s")
	case OptionScopeSession:
		if target == "" {
			return nil, errors.New("target required for scope=session")
		}
		args = append(args, "-t", target)
	case OptionScopeWindow:
		if target == "" {
			return nil, errors.New("target required for scope=window")
		}
		args = append(args, "-w", "-t", target)
	case OptionScopePane:
		if target == "" {
			return nil, errors.New("target required for scope=pane")
		}
		args = append(args, "-p", "-t", target)
	case "":
		return nil, errors.New("scope required")
	default:
		return nil, fmt.Errorf("scope must be one of server|session|window|pane (got %q)", scope)
	}
	args = append(args, name)
	if !unset {
		// VALUE is the final positional. tmux accepts an empty string
		// as a valid value (it really does store "") so we never
		// substitute a placeholder; callers that genuinely want to
		// clear the override should pass unset=true rather than an
		// empty value.
		args = append(args, value)
	}
	return args, nil
}
