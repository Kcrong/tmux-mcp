package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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

// SelectWindow makes target the active window of session via
// `tmux select-window -t <session>:<target>`. target may be either a
// window name or a numeric index — tmux resolves both forms uniformly.
//
// Like CreateWindow / KillWindow, a missing session is normalised to a
// wrapped errs.ErrSessionNotFound: `select-window -t <session>:...`
// emits "can't find window" instead of "can't find session" because -t
// names a window target, so detect that phrasing too and surface the
// same typed sentinel run() uses elsewhere.
func (c *Controller) SelectWindow(ctx context.Context, session, target string) error {
	if session == "" {
		return errors.New("session required")
	}
	if target == "" {
		return errors.New("target required")
	}
	full := session + ":" + target
	if _, err := c.run(ctx, "select-window", "-t", full); err != nil {
		// `select-window -t <session>:<target>` rejects an unknown
		// session/window with "can't find window", which run() does not
		// translate. Fold it into errs.ErrSessionNotFound so the
		// JSON-RPC layer maps the failure consistently with create/kill.
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find window") {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}

// RenameWindow renames the targeted window via
// `tmux rename-window -t <session>:<target> <newName>`. target may be a
// window name or numeric index; newName is the new label tmux will
// assign. Validation of newName (regex / length) lives at the boundary
// — this method just wires the spec into the right tmux flags.
//
// A missing session/window surfaces as a wrapped errs.ErrSessionNotFound
// for the same reason described on SelectWindow.
func (c *Controller) RenameWindow(ctx context.Context, session, target, newName string) error {
	if session == "" {
		return errors.New("session required")
	}
	if target == "" {
		return errors.New("target required")
	}
	if newName == "" {
		return errors.New("name required")
	}
	full := session + ":" + target
	if _, err := c.run(ctx, "rename-window", "-t", full, newName); err != nil {
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find window") {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}

// SwapWindow exchanges two windows of the same session in place via
// `tmux swap-window -s <session>:<src> -t <session>:<dst>` (with `-d`
// when noSelect is true). tmux trades the layout slots: each window
// keeps its `#{window_id}`, contents, panes, and running processes
// while the position indices/names trade. Both src and dst may be
// either window names or numeric indices — tmux resolves both forms
// uniformly. Pairs with [Controller.SwapPane] (which swaps panes
// inside a single window) and [Controller.MoveWindow] (which
// relocates a window to a different slot or session).
//
// noSelect maps to tmux's `-d` flag: when true, the active window of
// the session is left where it was; when false (the default), tmux
// behaves as it does interactively and may shift focus to follow the
// swap. Most agents want noSelect=true so a chained
// send_keys/capture stays deterministic.
//
// A missing session/window surfaces as a wrapped errs.ErrSessionNotFound
// for the same reason described on SelectWindow: tmux's swap-window emits
// "can't find window" when a target half doesn't exist, which run() does
// not translate by itself, so we fold it into the typed sentinel here so
// the JSON-RPC dispatcher maps the failure to CodeSessionNotFound.
//
// Other failures pass through as-is so the JSON-RPC layer surfaces them
// via CodeInternal — the caller can read the wrapped tmux stderr to
// tell the cases apart.
func (c *Controller) SwapWindow(ctx context.Context, session, src, dst string, noSelect bool) error {
	if session == "" {
		return errors.New("session required")
	}
	if src == "" {
		return errors.New("src required")
	}
	if dst == "" {
		return errors.New("dst required")
	}
	args := []string{"swap-window", "-s", session + ":" + src, "-t", session + ":" + dst}
	if noSelect {
		// -d means "do not change the session's active window after the
		// swap". Append at the end so the argv order stays easy to diff
		// against tmux's man page (`-s … -t … [-d]`).
		args = append(args, "-d")
	}
	if _, err := c.run(ctx, args...); err != nil {
		// tmux swap-window against a missing window emits "can't find
		// window: <name>", which run() does not translate by itself.
		// Translate it so callers can errors.Is into errs.ErrSessionNotFound
		// regardless of which message tmux emitted.
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find window") {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}

// LinkWindow shares the window addressed by src onto the dst slot via
// `tmux link-window -s <src> -t <dst>` (with `-k` when kill is true).
// Unlike MoveWindow, link-window leaves the source intact: the same
// `#{window_id}` is now reachable from both sessions, so a long-running
// build window can be exposed in a "monitor" session without losing the
// foreground in the working session. Both src and dst use tmux's standard
// `<session>:<window>` target form; the boundary is responsible for the
// regex/length validation of each half.
//
// kill maps to tmux's `-k` flag: when true, an existing window already
// occupying the dst slot is destroyed before the link is established;
// when false (the default), tmux refuses with "index in use" rather than
// silently overwriting. Pairs with [Controller.SwapWindow] (in-place
// trade) and [Controller.MoveWindow] (cross-session relocation that
// removes the source).
//
// A missing session/window surfaces as a wrapped errs.ErrSessionNotFound
// for the same reason described on SelectWindow: tmux's link-window
// emits "can't find window" when a target half doesn't exist, which run()
// does not translate by itself, so we fold it into the typed sentinel
// here so the JSON-RPC dispatcher maps the failure to CodeSessionNotFound.
//
// Other failures (destination index already in use without kill, malformed
// target) pass through as-is so the JSON-RPC layer surfaces them via
// CodeInternal — the caller can read the wrapped tmux stderr to tell the
// cases apart.
func (c *Controller) LinkWindow(ctx context.Context, src, dst string, kill bool) error {
	if src == "" {
		return errors.New("src required")
	}
	if dst == "" {
		return errors.New("dst required")
	}
	args := []string{"link-window", "-s", src, "-t", dst}
	if kill {
		// -k means "kill the destination window if it already exists".
		// Append at the end so the argv order stays easy to diff against
		// tmux's man page (`link-window -s … -t … [-k]`).
		args = append(args, "-k")
	}
	if _, err := c.run(ctx, args...); err != nil {
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find window") {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}

// UnlinkWindow removes the window reference addressed by target via
// `tmux unlink-window -t <target>` (with `-k` when kill is true). It is
// the inverse of LinkWindow: where link-window grafts a window's
// `#{window_id}` into a second session's slot, unlink-window detaches
// the named slot from that session — leaving the window itself alive in
// any other sessions still referencing the same id. target uses tmux's
// standard `<session>:<window>` form; the boundary is responsible for
// the regex/length validation of each half.
//
// kill maps to tmux's `-k` flag: when false (the default), tmux refuses
// to unlink a window whose only reference is the one being removed
// (because doing so would also reap the underlying window itself); when
// true, the call proceeds even on the last reference, which destroys
// the window. The two flag values map to the two complementary
// use-cases: `kill=false` for "stop sharing into this session, but
// leave the window running where it lives", and `kill=true` for
// "destroy the linked window now that no session needs it any longer".
//
// A missing session/window surfaces as a wrapped errs.ErrSessionNotFound
// for the same reason described on SelectWindow: tmux's unlink-window
// emits "can't find window" / "can't find session" when the target does
// not exist, and we fold both into the typed sentinel here so the
// JSON-RPC dispatcher maps the failure to CodeSessionNotFound the same
// way every other window method does.
//
// Other failures — most notably the kill=false / last-reference refusal
// tmux phrases as "session has only one window" or "session would be
// destroyed" — pass through as-is so the JSON-RPC layer surfaces them
// via CodeInternal. The caller can read the wrapped tmux stderr to
// branch on the exact failure mode without us baking another sentinel
// into errs for a case the boundary already dissuades by inverting the
// kill flag.
func (c *Controller) UnlinkWindow(ctx context.Context, target string, kill bool) error {
	if target == "" {
		return errors.New("target required")
	}
	args := []string{"unlink-window", "-t", target}
	if kill {
		// -k means "unlink even if this is the last reference, destroying
		// the window". Append at the end so the argv order stays easy to
		// diff against tmux's man page (`unlink-window -t … [-k]`).
		args = append(args, "-k")
	}
	if _, err := c.run(ctx, args...); err != nil {
		// `tmux unlink-window -t <target>` rejects an unknown
		// session/window with "can't find window" — which run() does not
		// translate by itself — so fold it into errs.ErrSessionNotFound
		// here, mirroring SelectWindow / SwapWindow / LinkWindow's
		// handling of the same phrasing.
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find window") {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}

// MoveWindow relocates the window addressed by src onto the dst slot via
// `tmux move-window -s <src> -t <dst>`. Both src and dst use tmux's
// standard `<session>:<window>` target form; dst may carry an empty
// window part (e.g. "othersession:") to let tmux pick the next available
// index in that session. Validation of the target shapes (regex /
// length) lives at the boundary — this method just wires the strings
// into the right tmux flags.
//
// A missing source session surfaces as a wrapped errs.ErrSessionNotFound
// so the JSON-RPC dispatcher maps it to CodeSessionNotFound the same way
// SelectWindow / RenameWindow do. Note that `tmux move-window -s
// <missing>` emits "can't find session" (which run() already
// translates) rather than the "can't find window" form select / rename
// produce, because move-window's -s flag explicitly names a window
// target — we still translate "can't find window" defensively in case
// older tmux versions phrase it that way.
//
// Other failures (destination index already in use, malformed target)
// pass through as-is so the JSON-RPC layer surfaces them via
// CodeInternal — the caller can read the wrapped tmux stderr to tell
// the cases apart.
func (c *Controller) MoveWindow(ctx context.Context, src, dst string) error {
	if src == "" {
		return errors.New("src required")
	}
	if dst == "" {
		return errors.New("dst required")
	}
	if _, err := c.run(ctx, "move-window", "-s", src, "-t", dst); err != nil {
		if !errors.Is(err, errs.ErrSessionNotFound) &&
			strings.Contains(strings.ToLower(err.Error()), "can't find window") {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}

// Window describes a single tmux window as observed by `tmux list-windows`.
//
// The fields are the subset of window format variables that an agent
// needs to identify a target (Index/Name) and decide whether to drive
// it (Active, Panes count for layout-aware logic).
type Window struct {
	// Index is the window index within the session (0-based). Combined
	// with the session name it forms the canonical "session:index"
	// target string for follow-up calls (window_kill, send_keys, ...).
	Index int
	// Name is the human-readable label tmux assigned. May be the
	// caller-supplied -n value or whatever tmux auto-assigned from the
	// command's basename when no -n was passed.
	Name string
	// Active reports whether this window is the currently focused one
	// of its session.
	Active bool
	// Panes is the number of panes currently in the window. Useful for
	// layout-aware agents that need to know whether a window is split.
	Panes int
}

// listWindowsFormat matches the parsing in parseWindowLine — keep them
// in sync. tmux substitutes each #{...} variable and joins them with
// the literal '|' between them. '|' is safe because none of these
// variables ever contains it (the boundary validator already forbids
// it in user-supplied window names).
const listWindowsFormat = "#{window_index}|#{window_name}|#{?window_active,1,0}|#{window_panes}"

// ListWindows enumerates every window visible to this controller's
// tmux server. When session is non-empty the listing is scoped to that
// session; otherwise every window on the server is returned (`-a`),
// matching the same convention as [Controller.ListPanes].
//
// A typed errs.ErrSessionNotFound is returned (wrapped) when tmux
// reports the targeted session does not exist, so the JSON-RPC layer
// can map that to CodeSessionNotFound.
func (c *Controller) ListWindows(ctx context.Context, session string) ([]Window, error) {
	args := []string{"list-windows", "-F", listWindowsFormat}
	if session != "" {
		args = append(args, "-t", session)
	} else {
		// -a means "every window on the server" — only useful when no
		// specific session was requested.
		args = append(args, "-a")
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		// `tmux list-windows -t <session>` rejects an unknown session
		// with "can't find session: <name>", which run() already
		// translates to errs.ErrSessionNotFound. Some tmux builds emit
		// "no server running" when the controller has not yet started
		// its server — surface the same typed sentinel so callers can
		// rely on a single error type for "session does not exist on
		// this controller".
		msg := strings.ToLower(err.Error())
		if session != "" && !errors.Is(err, errs.ErrSessionNotFound) &&
			(strings.Contains(msg, "can't find window") ||
				strings.Contains(msg, "no server running")) {
			return nil, fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	lines := strings.Split(out, "\n")
	wins := make([]Window, 0, len(lines))
	for i, line := range lines {
		w, perr := parseWindowLine(line)
		if perr != nil {
			return nil, fmt.Errorf("list-windows: line %d: %w", i+1, perr)
		}
		wins = append(wins, w)
	}
	return wins, nil
}

// parseWindowLine splits one '|'-delimited row produced by
// listWindowsFormat into a Window. The format is fixed at the call
// site (above), so any drift in field count is a bug — reject it
// loudly rather than guess.
func parseWindowLine(line string) (Window, error) {
	const wantFields = 4
	fields := strings.Split(line, "|")
	if len(fields) != wantFields {
		return Window{}, fmt.Errorf("expected %d '|'-separated fields, got %d in %q", wantFields, len(fields), line)
	}
	idx, err := strconv.Atoi(fields[0])
	if err != nil {
		return Window{}, fmt.Errorf("window_index %q: %w", fields[0], err)
	}
	panes, err := strconv.Atoi(fields[3])
	if err != nil {
		return Window{}, fmt.Errorf("window_panes %q: %w", fields[3], err)
	}
	// tmux emits "1" for the active window and "0" otherwise.
	active := strings.TrimSpace(fields[2]) == "1"
	return Window{
		Index:  idx,
		Name:   fields[1],
		Active: active,
		Panes:  panes,
	}, nil
}
