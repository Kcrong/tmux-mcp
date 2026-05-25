package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// SetWindowOption wraps `tmux set-window-option [-aFgoqu] [-t TARGET] OPTION VALUE`
// — the explicit "set-window-option" form (an alias for `set-option -w`)
// kept so test fixtures and audit trails read naturally to anyone scanning
// the wire. Window options are tmux's third option scope (alongside
// server / session) and cover knobs like `synchronize-panes`,
// `automatic-rename`, `mode-keys`, and the window-side render formats.
//
// Flag semantics:
//
//   - target != "" → `-t TARGET` (typical `SESSION:WINDOW`, also a bare
//     window id `@N`). Empty target is allowed only with global=true,
//     the global window-options table is per-server, not per-window.
//   - appendValue=true → `-a` (append to a string-list option, e.g.
//     `pane-border-format`). tmux interprets the new value as an
//     extension of the existing string when the option is a list, and
//     replaces it otherwise.
//   - formatExpand=true → `-F` (run the value through tmux's #{format}
//     substitution before storing). Useful for status-format-style
//     options that embed substitutions; ignored / silently passes the
//     value through when the option does not interpret formats.
//   - global=true → `-g` (modify the global window-options table — the
//     defaults every window inherits — instead of the per-window
//     override map).
//   - allowMissing=true → `-q` (suppress "unknown option" diagnostics so
//     tmux reports success even when NAME is misspelled). Mirrors the
//     tmux CLI's "best-effort" mode for callers driving config that
//     might predate or postdate the running tmux build.
//   - unset=true → `-u` (delete the override; VALUE is omitted). When
//     unset=true the value argument is ignored entirely; when
//     unset=false the value is appended verbatim (including the empty
//     string, which tmux accepts as a legitimate empty value).
//
// Error mapping:
//   - target session/window not found: surfaced via run() as a wrapped
//     errs.ErrSessionNotFound (the underlying message
//     "can't find session: <name>" is recognised by the same
//     isSessionMissingMsg detector that powers the rest of the
//     surface) so the JSON-RPC dispatcher maps it to
//     CodeSessionNotFound.
//   - unknown option name (without -q): tmux replies
//     "unknown option: <name>" on stderr; the wrapped error surfaces
//     unchanged, mapped to CodeInternal at the boundary.
//   - empty name: rejected up front so tmux is never asked to
//     "set-window-option" with no positional. The boundary already
//     enforces this via the handler regex, but the controller defends
//     here too for tests and future direct call sites.
func (c *Controller) SetWindowOption(
	ctx context.Context,
	target, name, value string,
	appendValue, formatExpand, global, allowMissing, unset bool,
) error {
	args, err := buildSetWindowOptionArgs(target, name, value, appendValue, formatExpand, global, allowMissing, unset)
	if err != nil {
		return err
	}
	if _, err := c.run(ctx, args...); err != nil {
		// tmux set-window-option against a missing window (the
		// `SESSION:WINDOW` form where SESSION exists but WINDOW does
		// not, or where SESSION itself is missing on builds whose
		// stderr says "no such window: <target>" instead of the
		// "session not found" form run() already handles) emits a
		// distinct phrase that the run()-level detector does not see.
		// Translate either variant so callers can errors.Is into
		// errs.ErrSessionNotFound regardless of which message tmux
		// emitted — the same contract every other tmuxctl method
		// upholds.
		if !errors.Is(err, errs.ErrSessionNotFound) {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "no such window") ||
				strings.Contains(msg, "can't find window") ||
				strings.Contains(msg, "window not found") {
				return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
			}
		}
		return err
	}
	return nil
}

// buildSetWindowOptionArgs assembles the argv passed to
// `tmux set-window-option`. Split out from [Controller.SetWindowOption]
// so the assembly logic can be unit-tested without spinning up a live
// tmux server.
//
// Argument order mirrors tmux's documented syntax:
//
//	set-window-option [-aFgoqu] [-t TARGET] OPTION VALUE
//
// Flags come first (-a/-F/-g/-q/-u), then the optional -t TARGET,
// then the positional OPTION, and finally VALUE (suppressed when
// unset=true).
func buildSetWindowOptionArgs(
	target, name, value string,
	appendValue, formatExpand, global, allowMissing, unset bool,
) ([]string, error) {
	if name == "" {
		return nil, errors.New("option name required")
	}
	args := []string{"set-window-option"}
	if appendValue {
		args = append(args, "-a")
	}
	if formatExpand {
		args = append(args, "-F")
	}
	if global {
		args = append(args, "-g")
	}
	if allowMissing {
		args = append(args, "-q")
	}
	if unset {
		args = append(args, "-u")
	}
	if target != "" {
		args = append(args, "-t", target)
	}
	args = append(args, name)
	if !unset {
		// VALUE is the final positional. tmux accepts an empty string
		// as a valid value (it really does store "") so we never
		// substitute a placeholder; callers wanting to clear the
		// override should pass unset=true rather than an empty value.
		args = append(args, value)
	}
	return args, nil
}
