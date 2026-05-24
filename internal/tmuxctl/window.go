package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// WindowSpec describes a window to create with [Controller.CreateWindow].
//
// Only Session is required; Name / Command / Select are optional and
// match the underlying `tmux new-window` flags one-for-one.
type WindowSpec struct {
	// Session is the existing tmux session the new window will live in.
	Session string
	// Name is the human-readable label tmux will assign to the window
	// (passed via -n). When empty, tmux auto-assigns a name from the
	// command being run.
	Name string
	// Command is the initial command tmux runs in the new window. When
	// empty, tmux falls back to the user's default shell — same
	// semantics as `tmux new-window` with no trailing argument.
	Command string
	// Select controls whether tmux switches to the new window on
	// creation. When false the new window is created in the background
	// (-d flag); when true the session's active window pointer moves to
	// the freshly created window. Defaults map to "true" at the boundary
	// because that is what an interactive `tmux new-window` does.
	Select bool
}

// WindowResult is the structured outcome of a successful CreateWindow
// call. Callers (the JSON-RPC layer) format it into the human-readable
// "window <X> created in <Y>" message; we keep the raw fields here so
// future shapes (json blocks, structured logs) can reuse them.
type WindowResult struct {
	// Session echoes the input session so callers can correlate the
	// response with the request without round-tripping the spec.
	Session string
	// Name is the window name tmux ended up with — either the caller's
	// requested -n value, or whatever tmux auto-assigned when Name was
	// empty (typically the command's basename, e.g. "bash").
	Name string
	// Index is the numeric window index (`#{window_index}`) tmux placed
	// the new window at. Stable enough to use as a target string when
	// Name is empty.
	Index string
}

// CreateWindow creates a new window inside an existing session via
// `tmux new-window`. The boundary (server tool) is responsible for
// validating the inputs (session/name regex, length); this method just
// wires the spec into the right tmux flags and parses the resulting
// `#{window_name}|#{window_index}` line back into a WindowResult.
//
// A missing session surfaces as a wrapped errs.ErrSessionNotFound so
// the JSON-RPC layer maps it to CodeSessionNotFound.
func (c *Controller) CreateWindow(ctx context.Context, s WindowSpec) (WindowResult, error) {
	if s.Session == "" {
		return WindowResult{}, errors.New("session required")
	}
	// We use -P to make tmux print the new window's identity, and -F to
	// pin the format so we don't depend on tmux's default "session:index"
	// output (which has changed across versions). The literal '|' is safe
	// — tmux window names cannot contain it without aggressive escaping
	// that the boundary validator already forbids.
	args := []string{"new-window", "-P", "-F", "#{window_name}|#{window_index}", "-t", s.Session}
	if !s.Select {
		// -d means "do not switch to the new window". When Select is true
		// we omit the flag so tmux does the usual thing of focusing the
		// new window — same as an interactive `tmux new-window`.
		args = append(args, "-d")
	}
	if s.Name != "" {
		args = append(args, "-n", s.Name)
	}
	if s.Command != "" {
		// tmux new-window treats the trailing args as the shell command
		// (after `--`). Passing it as a single argument is fine; tmux
		// invokes it via /bin/sh -c so quoting / arg-splitting matches
		// what the user would type interactively.
		args = append(args, "--", s.Command)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		// tmux new-window -t <session> rejects an unknown session with
		// "can't find window: <session>" because -t accepts a window
		// target, not a session target. Translate that into the typed
		// errs.ErrSessionNotFound run() emits for "session not found", so
		// the JSON-RPC dispatcher can map it to CodeSessionNotFound the
		// same way the other tools do.
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find window") {
			return WindowResult{}, fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return WindowResult{}, err
	}
	line := strings.TrimRight(out, "\n")
	parts := strings.SplitN(line, "|", 2)
	if len(parts) != 2 {
		return WindowResult{}, fmt.Errorf("new-window: unexpected output %q", out)
	}
	return WindowResult{
		Session: s.Session,
		Name:    parts[0],
		Index:   parts[1],
	}, nil
}

// CountWindows returns the number of windows currently in the named
// session. Used by the boundary layer to refuse window_kill when it
// would destroy the last window of a session — letting tmux do that
// would also tear down the session itself, which blurs the line
// between window_kill and session_kill.
//
// A missing session surfaces as a wrapped errs.ErrSessionNotFound (via
// run()).
func (c *Controller) CountWindows(ctx context.Context, session string) (int, error) {
	if session == "" {
		return 0, errors.New("session required")
	}
	out, err := c.run(ctx, "list-windows", "-t", session, "-F", "#{window_id}")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line != "" {
			count++
		}
	}
	return count, nil
}

// KillWindow destroys a single window by `<session>:<window>` target.
// `window` may be either a window name or a numeric index — tmux
// resolves both forms uniformly.
//
// Callers must ensure the targeted window is not the only window in
// the session (use [Controller.CountWindows] first); otherwise tmux
// would also reap the session, which blurs the line between
// window_kill and session_kill.
//
// A missing session surfaces as errs.ErrSessionNotFound (wrapped) via
// run()'s built-in detection.
func (c *Controller) KillWindow(ctx context.Context, session, window string) error {
	if session == "" {
		return errors.New("session required")
	}
	if window == "" {
		return errors.New("window required")
	}
	target := session + ":" + window
	if _, err := c.run(ctx, "kill-window", "-t", target); err != nil {
		return err
	}
	return nil
}
