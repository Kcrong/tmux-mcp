package tmuxctl

import (
	"context"
	"errors"
	"strings"
)

// Option scope constants accepted by [Controller.ShowOptions]. They map
// one-for-one to tmux's own scoping flags: server (-s), session
// (default with -t), and window (-w with -t). Keeping them as exported
// string constants lets the JSON-RPC boundary validate the caller's
// raw string against this exact set without hand-rolling a switch.
const (
	// OptionScopeServer selects the server-wide option set
	// (`tmux show-options -s`). Server options are global to the tmux
	// server process and never carry a session/window qualifier.
	OptionScopeServer = "server"
	// OptionScopeSession selects the per-session option set
	// (`tmux show-options -t <session>`), or the global session
	// defaults (`-g`) when global=true is passed to [Controller.ShowOptions].
	OptionScopeSession = "session"
	// OptionScopeWindow selects the per-window option set
	// (`tmux show-options -w -t <session>:<window>`), or the global
	// window defaults (`-g`) when global=true is passed.
	OptionScopeWindow = "window"
)

// ShowOptions wraps `tmux show-options` and returns the resolved option
// set at the requested scope. The output is parsed line-by-line into a
// flat key→value map: tmux prints one `key value` pair per line, with
// the key always a single token and the value being the remainder of
// the line (which may itself contain spaces, e.g. `command-alias[2]
// "server-info=show-messages -JT"`).
//
// Scope semantics:
//
//   - scope=[OptionScopeServer]: invokes `show-options -s`. Both the
//     session and window arguments are ignored — server options have no
//     session/window qualifier.
//   - scope=[OptionScopeSession]: invokes `show-options -t <session>`,
//     or `show-options -g -t <session>` when global=true. Session is
//     required.
//   - scope=[OptionScopeWindow]: invokes
//     `show-options -w -t <session>:<window>`, or with `-g` prepended
//     when global=true. Both session and window are required.
//
// Validation lives at the JSON-RPC boundary; this method also guards
// against the obvious misuse (empty scope / missing session+window for
// the relevant scopes) so direct callers get a clean error rather than
// a confusing tmux stderr.
//
// A missing session/window surfaces as a wrapped errs.ErrSessionNotFound
// (via run()'s isSessionMissingMsg detector) so the JSON-RPC dispatcher
// maps it to CodeSessionNotFound.
func (c *Controller) ShowOptions(ctx context.Context, scope, session, window string, global bool) (map[string]string, error) {
	args, err := buildShowOptionsArgs(scope, session, window, global)
	if err != nil {
		return nil, err
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseShowOptions(out), nil
}

// buildShowOptionsArgs assembles the argv passed to `tmux show-options`
// for the requested scope. Split out from [Controller.ShowOptions] so
// the assembly logic can be unit-tested without spinning up a live tmux
// server.
func buildShowOptionsArgs(scope, session, window string, global bool) ([]string, error) {
	switch scope {
	case OptionScopeServer:
		// Server-scope options ignore -t entirely: tmux server options
		// are global to the process and have no session/window qualifier.
		// -g is also a no-op here (server options ARE global), so we
		// drop it to keep the argv minimal.
		return []string{"show-options", "-s"}, nil
	case OptionScopeSession:
		if session == "" {
			return nil, errors.New("session required for scope=session")
		}
		args := []string{"show-options"}
		if global {
			args = append(args, "-g")
		}
		args = append(args, "-t", session)
		return args, nil
	case OptionScopeWindow:
		if session == "" {
			return nil, errors.New("session required for scope=window")
		}
		if window == "" {
			return nil, errors.New("window required for scope=window")
		}
		args := []string{"show-options", "-w"}
		if global {
			args = append(args, "-g")
		}
		args = append(args, "-t", session+":"+window)
		return args, nil
	case "":
		return nil, errors.New("scope required")
	default:
		return nil, errors.New("scope must be one of server|session|window")
	}
}

// parseShowOptions converts the line-oriented stdout of tmux's
// show-options command into a flat map. Each non-empty line is split on
// the first space: everything before the space is the key, everything
// after is the value. Values are returned verbatim (including any
// surrounding quotes tmux emits for strings with spaces or special
// characters) — callers wanting to normalise quoting can do so on top
// of the raw map. Lines without a space are still recorded with an
// empty-string value so a caller never silently loses an option name.
func parseShowOptions(out string) map[string]string {
	options := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		// SplitN with n=2 ensures values that themselves contain spaces
		// (e.g. `command-alias[0] split-pane=split-window` is fine, but
		// `default-size 80x24`, `status-format[0] "..."` would lose
		// information without the n=2 guard) come through intact.
		parts := strings.SplitN(line, " ", 2)
		key := parts[0]
		if key == "" {
			continue
		}
		if len(parts) == 2 {
			options[key] = parts[1]
		} else {
			options[key] = ""
		}
	}
	return options
}
