package tmuxctl

import (
	"context"
	"errors"
	"strings"
)

// EnvEntry is a single environment variable as reported by
// `tmux show-environment`. Present=false signals the variable is
// explicitly *removed* from the requested scope (tmux prints these
// as a leading dash, e.g. `-FOO`, when a session-level entry has
// been unset on top of an inherited global). Value is empty when
// Present is false; an empty value with Present=true is the legal
// "set to empty" case (tmux happily stores empty strings).
type EnvEntry struct {
	// Name is the variable name with no leading dash, regardless of
	// whether Present is true or false. Callers should not need to
	// strip a sentinel character themselves.
	Name string
	// Value is the variable's value verbatim. Empty when Present is
	// false. An empty Value with Present=true is the legal
	// "set to empty" form distinct from "removed".
	Value string
	// Present is true when tmux currently has a value for this
	// variable in the requested scope, and false when the entry is
	// the explicit "removed" form (`-NAME`).
	Present bool
}

// EnvDump is the structured result of a `tmux show-environment`
// invocation. Vars maps each variable name to its [EnvEntry];
// callers that only need the names can iterate the map keys, and
// callers that need full presence/value semantics use the value.
//
// Callers asking for a single variable (Controller.ShowEnvironment
// with a non-empty name) get an EnvDump whose Vars holds exactly
// that one entry. The "missing" case (tmux's `unknown variable:
// NAME`) is surfaced as a wrapped sentinel rather than an
// EnvDump-with-empty-map so the JSON-RPC boundary can decide how to
// report "the variable is not set at all" vs "the variable is
// explicitly marked removed".
type EnvDump struct {
	// Vars is the parsed environment table. Key is the variable
	// name (no leading dash); value is the full [EnvEntry] including
	// the Present flag.
	Vars map[string]EnvEntry
}

// ErrEnvNameNotSet is the typed sentinel returned when
// [Controller.ShowEnvironment] is called with a single `name` that
// tmux reports as `unknown variable: NAME`. Distinct from the
// "removed" form: a removed entry has a tmux record (with `-NAME`)
// and round-trips as Present=false; a not-set name has no record
// at all and produces this sentinel so the JSON-RPC boundary can
// decide whether to map it onto an empty result or an error.
var ErrEnvNameNotSet = errors.New("tmux show-environment: variable not set")

// ShowEnvironment wraps `tmux show-environment` and returns the
// parsed environment table for the requested scope.
//
//   - scope=[EnvironmentScopeSession]: invokes
//     `show-environment -t <target>` (with optional NAME). target
//     is required.
//   - scope=[EnvironmentScopeGlobal]: invokes
//     `show-environment -g` (with optional NAME). target is
//     ignored — global env entries have no per-session qualifier.
//
// When name is non-empty, tmux is asked for that single variable
// only. tmux exits non-zero with `unknown variable: NAME` when the
// variable has never been set in the scope; that case is wrapped as
// [ErrEnvNameNotSet] so callers can distinguish it from the explicit
// "removed" form (which tmux does emit, as a leading-dash line).
//
// Validation lives at the JSON-RPC boundary; this method also
// guards against the obvious misuse (empty scope / missing target
// for scope=session) so direct callers get a clean error rather
// than a confusing tmux stderr.
//
// A missing target (the "session does not exist" case under
// scope=session) surfaces as a wrapped errs.ErrSessionNotFound via
// run()'s isSessionMissingMsg detector so the JSON-RPC dispatcher
// can map it to CodeSessionNotFound.
func (c *Controller) ShowEnvironment(ctx context.Context, name, scope, target string) (EnvDump, error) {
	args, err := buildShowEnvironmentArgs(name, scope, target)
	if err != nil {
		return EnvDump{}, err
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		// tmux uses "unknown variable: NAME" stderr (and exit !=0)
		// when a single-name probe targets a variable that has never
		// been set in the scope. Map that onto ErrEnvNameNotSet so
		// the boundary can branch on the typed sentinel rather than
		// substring-matching tmux's stderr text.
		if name != "" && strings.Contains(strings.ToLower(err.Error()), "unknown variable") {
			return EnvDump{}, ErrEnvNameNotSet
		}
		return EnvDump{}, err
	}
	return parseShowEnvironment(out), nil
}

// buildShowEnvironmentArgs assembles the argv passed to
// `tmux show-environment` for the requested scope. Split out from
// [Controller.ShowEnvironment] so the assembly logic can be
// unit-tested without spinning up a live tmux server.
func buildShowEnvironmentArgs(name, scope, target string) ([]string, error) {
	args := []string{"show-environment"}
	switch scope {
	case EnvironmentScopeSession:
		if target == "" {
			return nil, errors.New("target required for scope=session")
		}
		args = append(args, "-t", target)
	case EnvironmentScopeGlobal:
		// Global scope ignores -t entirely: tmux global env entries
		// are server-wide and have no session/window qualifier.
		args = append(args, "-g")
	case "":
		return nil, errors.New("scope required")
	default:
		return nil, errors.New("scope must be one of session|global")
	}
	if name != "" {
		args = append(args, name)
	}
	return args, nil
}

// parseShowEnvironment converts the line-oriented stdout of tmux's
// show-environment command into an [EnvDump]. tmux emits one
// variable per line in one of two shapes:
//
//   - `NAME=VALUE` — a present entry; the value is everything after
//     the first `=`. An empty value (`NAME=`) is the legal
//     "set to empty" form and is preserved verbatim.
//   - `-NAME` — a removed entry; the leading dash signals the
//     variable was unset on top of an inherited (global) value, so
//     future panes in this scope will not see it. We strip the dash
//     and return Present=false with an empty value.
//
// Lines that do not match either shape are skipped silently — tmux
// 3.x has only ever emitted these two formats, so any divergence is
// either a header line we don't care about (none observed in
// practice) or a future format we'd rather drop than mis-classify.
func parseShowEnvironment(out string) EnvDump {
	vars := make(map[string]EnvEntry)
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "-") {
			// Removed entry. Strip the leading dash; tmux guarantees
			// the rest of the line is a bare name.
			name := strings.TrimPrefix(line, "-")
			if name == "" {
				continue
			}
			vars[name] = EnvEntry{Name: name, Present: false}
			continue
		}
		// Present entry. SplitN with n=2 keeps a value that contains
		// `=` (e.g. `KEY=foo=bar`) intact — we only consume the
		// first `=` as the separator.
		idx := strings.Index(line, "=")
		if idx < 0 {
			// No `=` and no leading dash. tmux 3.x has not been
			// observed emitting this shape, but be conservative:
			// treat the whole line as a Present-but-empty name so
			// callers don't lose the entry entirely.
			vars[line] = EnvEntry{Name: line, Value: "", Present: true}
			continue
		}
		name := line[:idx]
		value := line[idx+1:]
		if name == "" {
			continue
		}
		vars[name] = EnvEntry{Name: name, Value: value, Present: true}
	}
	return EnvDump{Vars: vars}
}
