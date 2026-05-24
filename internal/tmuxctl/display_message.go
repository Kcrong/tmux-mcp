package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// DisplayMessage evaluates a tmux format string via `tmux display-message
// -p` and returns the single resolved line. The optional session /
// window / pane arguments combine into a tmux target ("session",
// "session:window", "session:window.pane") that pins where the format
// is evaluated; when all three are empty no `-t` is passed and tmux
// resolves variables against its current/global context.
//
// Surface intent: this is the canonical introspection escape hatch for
// any `#{...}` variable that does not yet have a dedicated tool. Higher
// layers can use it to read pane titles, window options, server
// uptime — anything tmux exposes in its format DSL — without the agent
// having to call `tmux` directly.
//
// Format validation: the format string must be non-empty and must not
// contain literal newlines. tmux silently joins multi-line formats with
// the embedded newlines, which would split the JSON-RPC frame budget
// and produce a multi-line "value" the schema documents as a single
// string. The boundary layer enforces both rules; this method also
// rejects an empty format defensively so a programmatic caller cannot
// bypass that check.
//
// A missing session surfaces as a wrapped errs.ErrSessionNotFound. We
// run an explicit `has-session -t <session>` probe up front whenever a
// session is provided because tmux's own `display-message -t` is
// happy to print a blank line for an unknown target instead of
// erroring — without the probe we would silently return "" and the
// JSON-RPC layer would map nothing. The probe's stderr ("can't find
// session: ...") is already recognised by run()'s isSessionMissingMsg,
// so the wrapping happens automatically. Window/pane drift from a
// real-but-mistargeted call still surfaces via the "can't find
// window/pane" detection below.
func (c *Controller) DisplayMessage(ctx context.Context, format, session, window, pane string) (string, error) {
	if format == "" {
		return "", errors.New("format required")
	}
	if session != "" {
		// has-session is the canonical existence check — its stderr
		// ("can't find session: <name>") is already recognised by
		// isSessionMissingMsg, so the wrapping happens automatically
		// inside run(). Doing this up front means the rest of this
		// method can assume the session exists, which matters because
		// `display-message -t <missing>` does not error — it just
		// prints a blank line.
		if _, err := c.run(ctx, "has-session", "-t", session); err != nil {
			return "", err
		}
	}
	args := []string{"display-message", "-p"}
	if target := buildDisplayTarget(session, window, pane); target != "" {
		args = append(args, "-t", target)
	}
	args = append(args, format)
	out, err := c.run(ctx, args...)
	if err != nil {
		// `tmux display-message -t <missing>` emits "can't find
		// window" / "can't find pane" instead of the "can't find
		// session" form run() already wraps. Detect those so callers
		// can rely on errs.ErrSessionNotFound regardless of which half
		// of the target tmux blamed.
		if !errors.Is(err, errs.ErrSessionNotFound) {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "can't find window") ||
				strings.Contains(msg, "can't find pane") {
				return "", fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
			}
		}
		return "", err
	}
	// tmux always terminates the resolved format with a single newline;
	// strip just that trailing newline so callers receive the tmux
	// payload verbatim. We deliberately do not TrimSpace — the format
	// may legitimately produce leading/trailing whitespace (e.g.
	// `#{=10:pane_title}` pads with spaces) that the caller asked for.
	return strings.TrimRight(out, "\n"), nil
}

// buildDisplayTarget assembles the `-t` argument from the optional
// session/window/pane parts. Returns "" when nothing was supplied so
// the caller can omit `-t` entirely (tmux then resolves the format
// against its current/global context).
//
// The resolution order mirrors what tmux's own target grammar accepts:
//   - pane present  → "<session>:<window>.<pane>"
//   - window only   → "<session>:<window>"
//   - session only  → "<session>"
//   - all empty     → ""
//
// Window or pane present without a session falls through to the same
// branches; tmux will then resolve the missing prefix against its
// current target. The boundary layer is responsible for refusing
// nonsense combinations (pane without window, window without session)
// when the operator wants strict semantics — this helper just shapes
// what was actually passed.
func buildDisplayTarget(session, window, pane string) string {
	switch {
	case pane != "":
		return session + ":" + window + "." + pane
	case window != "":
		return session + ":" + window
	case session != "":
		return session
	default:
		return ""
	}
}
