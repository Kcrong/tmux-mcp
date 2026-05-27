package tmuxctl

import (
	"context"
	"errors"
)

// EnvironmentScope constants accepted by [Controller.SetEnvironment].
// They map onto tmux's `set-environment` flag set: `-g` for the global
// (server-wide) environment future sessions inherit, and the default
// (no `-g`) for a single session's environment table. Keeping the
// scopes as named string constants lets the JSON-RPC boundary validate
// the caller's raw string against this exact set without hand-rolling
// a switch.
const (
	// EnvironmentScopeSession selects the per-session environment table
	// (`tmux set-environment -t <session> ...`). Future panes spawned
	// inside that session inherit the variable; existing panes keep
	// whatever environment they already have, matching the underlying
	// tmux semantics.
	EnvironmentScopeSession = "session"
	// EnvironmentScopeGlobal selects the server-wide environment table
	// (`tmux set-environment -g ...`). New sessions inherit the
	// variable when they are created; existing sessions keep their own
	// per-session overrides, which is the ordinary tmux precedence.
	EnvironmentScopeGlobal = "global"
)

// SetEnvironment wraps `tmux set-environment` and either sets or
// removes a variable in the requested scope.
//
//   - scope=[EnvironmentScopeSession]: invokes
//     `set-environment -t <session> NAME VALUE` (or `-u NAME` to
//     remove). session is required.
//   - scope=[EnvironmentScopeGlobal]: invokes
//     `set-environment -g NAME VALUE` (or `-g -u NAME`). session is
//     ignored — global env entries have no per-session qualifier.
//
// remove=true means "drop the variable" and maps to tmux's `-u`. value
// is ignored on the remove path; the caller is responsible for picking
// the form (the JSON-RPC boundary maps "value omitted" → remove=true).
//
// Validation lives at the JSON-RPC boundary; this method also guards
// against the obvious misuse (empty scope / missing session for
// scope=session / empty name) so direct callers get a clean error
// rather than a confusing tmux stderr.
//
// A missing session surfaces as a wrapped errs.ErrSessionNotFound (via
// run()'s isSessionMissingMsg detector) so the JSON-RPC dispatcher maps
// it to CodeSessionNotFound.
func (c *Controller) SetEnvironment(ctx context.Context, scope, session, name, value string, remove bool) error {
	args, err := buildSetEnvironmentArgs(scope, session, name, value, remove)
	if err != nil {
		return err
	}
	_, err = c.run(ctx, args...)
	return err
}

// buildSetEnvironmentArgs assembles the argv passed to
// `tmux set-environment` for the requested scope. Split out from
// [Controller.SetEnvironment] so the assembly logic can be unit-tested
// without spinning up a live tmux server.
func buildSetEnvironmentArgs(scope, session, name, value string, remove bool) ([]string, error) {
	if name == "" {
		return nil, errors.New("name required")
	}
	args := []string{"set-environment"}
	switch scope {
	case EnvironmentScopeSession:
		if session == "" {
			return nil, errors.New("session required for scope=session")
		}
		args = append(args, "-t", session)
	case EnvironmentScopeGlobal:
		// Global scope ignores -t entirely: tmux global env entries are
		// server-wide and have no session/window qualifier.
		args = append(args, "-g")
	case "":
		return nil, errors.New("scope required")
	default:
		return nil, errors.New("scope must be one of session|global")
	}
	if remove {
		// `-u NAME` is tmux's "unset" form; no value follows.
		args = append(args, "-u", name)
		return args, nil
	}
	// Set form: `set-environment [-g | -t SESSION] NAME VALUE`. tmux
	// accepts an empty VALUE (it really stores an empty string), so we
	// never substitute a placeholder here.
	args = append(args, name, value)
	return args, nil
}
