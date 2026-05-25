package tmuxctl

import (
	"context"
	"strings"
)

// HookEntry is one binding of a tmux hook to a command. tmux stores
// hooks in the same options-table machinery it uses for ordinary
// options, so an "entry" here is a single (name, command, target)
// triple — the format `tmux show-options -H` emits as
// `name[idx] command...`. Multiple bindings against the same hook
// share the name and target but get distinct array indices in tmux's
// internal table; we surface every index as its own entry so a caller
// rebuilding the binding set sees the same fan-out tmux applies at
// fire time.
//
// Fields:
//
//   - Name: the hook event (e.g. "pane-died", "client-attached"). When
//     tmux's output carried a `[N]` index suffix (multi-binding hooks)
//     the suffix is stripped here so the field always carries the bare
//     event name. The mapping back to the original index is not exposed
//     because tmux reassigns indices on every set-hook invocation,
//     making them unstable identifiers.
//   - Command: the tmux command line bound to the hook, verbatim from
//     tmux's stdout. Quoting is preserved as-is (tmux escapes embedded
//     quotes with backslashes) so a round-trip through set-hook +
//     show_hooks returns the same string the caller installed.
//   - Target: the scope this binding lives on. "" for server-global
//     bindings (returned by `show-options -gH` / `-gwH`); the session
//     name (e.g. "demo") for per-session bindings (returned by
//     `show-options -t SESSION -H` / `-t SESSION -wH`).
type HookEntry struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Target  string `json:"target"`
}

// ShowHooks enumerates every hook binding the controller's tmux server
// currently holds. Output is the full triple list (name, command,
// target) that a caller can either render as a status table or feed
// back into set-hook to reproduce the binding set on a sister server.
//
// Scope semantics:
//
//   - target == "" — scan both the server-global hook tables
//     (`show-options -gH` for server/session-class hooks like
//     client-attached, `show-options -gwH` for window-class hooks like
//     pane-died) AND every session's hook tables (iterate the live
//     session list and probe each session-options table).
//   - target != "" — scan only the named session's hook tables
//     (`show-options -t TARGET -H` and `-t TARGET -wH`). The hook
//     entries returned in this mode all carry Target == target.
//
// Returned slice is empty (not nil) when the server is up but holds no
// bindings, so callers branching on len() see the same shape regardless
// of whether the server is fresh or has been wiped via set-hook -u.
//
// Error mapping:
//   - target != "" but the session does not exist: surfaces via run()
//     as a wrapped errs.ErrSessionNotFound, which the JSON-RPC layer
//     maps to CodeSessionNotFound.
//   - server not running yet (no socket file): the global probe absorbs
//     the "no server running" / "error connecting" stderr the same way
//     ListSessions does, returning the empty-slice / nil-error pair so
//     a fresh controller does not look like a hard failure.
func (c *Controller) ShowHooks(ctx context.Context, target string) ([]HookEntry, error) {
	hooks := []HookEntry{}
	if target != "" {
		// Scope-restricted scan: only the named session's hook tables.
		// Order: -H first (server/session-class), then -wH
		// (window-class) so the response carries hooks in the same
		// "scoped first, then window-scoped" ordering the global path
		// uses.
		got, err := c.showHooksAt(ctx, target, target, true)
		if err != nil {
			return nil, err
		}
		hooks = append(hooks, got...)
		return hooks, nil
	}

	// target == "" — full sweep. Server-global tables first so the
	// response leads with hooks every session inherits, then every
	// session's per-table hooks so an operator-facing dump groups
	// "global" and "session-named" rows naturally.
	globals, err := c.showHooksGlobal(ctx)
	if err != nil {
		return nil, err
	}
	hooks = append(hooks, globals...)

	sessions, err := c.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range sessions {
		got, err := c.showHooksAt(ctx, name, name, true)
		if err != nil {
			return nil, err
		}
		hooks = append(hooks, got...)
	}
	return hooks, nil
}

// showHooksGlobal probes the server-global hook tables. Both probes
// are tolerated as empty so a brand-new controller (no socket file)
// returns an empty slice rather than bubbling up a "no server running"
// stderr — `tmux show-options -gH` against a not-yet-spawned server
// emits the same stderr shapes ListSessions absorbs, and the right
// answer at this layer is "no hooks set".
func (c *Controller) showHooksGlobal(ctx context.Context) ([]HookEntry, error) {
	out := []HookEntry{}
	for _, args := range [][]string{
		{"show-options", "-gH"},
		{"show-options", "-gwH"},
	} {
		raw, err := c.run(ctx, args...)
		if err != nil {
			// Treat the empty-server stderr shapes as "no hooks set"
			// rather than a genuine error so a fresh controller
			// answers cleanly. Anything else (bad flag, fork failure)
			// still propagates.
			msg := err.Error()
			if strings.Contains(msg, "no server running") ||
				strings.Contains(msg, "error connecting") ||
				strings.Contains(msg, "server exited unexpectedly") ||
				strings.Contains(msg, "No such file or directory") {
				return []HookEntry{}, nil
			}
			return nil, err
		}
		out = append(out, parseHookOutput(raw, "")...)
	}
	return out, nil
}

// showHooksAt probes a per-session hook table. tmux files hooks in
// either the session-options table (`-H`) or the window-options table
// (`-wH`); both have to be probed so the returned slice covers every
// binding the session currently carries.
//
// `target` is the tmux argv value passed via -t. `entryTarget` is the
// value stamped into each returned HookEntry's Target field — usually
// the same as `target`, kept distinct so a future caller passing a
// resolved (prefixed) tmux name can stamp the user-facing logical name
// instead.
//
// `probeWindow` exists so a future server-class-only call can opt out
// of the window-scope probe; today every caller wants both, but the
// flag keeps the helper honest for the day a caller wants to avoid
// touching the window-options table at all.
func (c *Controller) showHooksAt(ctx context.Context, target, entryTarget string, probeWindow bool) ([]HookEntry, error) {
	out := []HookEntry{}
	probes := [][]string{
		{"show-options", "-t", target, "-H"},
	}
	if probeWindow {
		probes = append(probes, []string{"show-options", "-t", target, "-wH"})
	}
	for _, args := range probes {
		raw, err := c.run(ctx, args...)
		if err != nil {
			return nil, err
		}
		out = append(out, parseHookOutput(raw, entryTarget)...)
	}
	return out, nil
}

// parseHookOutput converts the line-oriented stdout of
// `tmux show-options -H` into a slice of [HookEntry] rows. The format
// tmux emits is:
//
//	name[idx] command...
//
// for hooks that have at least one binding. Hooks the operator never
// set surface as the bare hook name with no command — `-H` lists every
// known hook event, set or not. We filter the unset rows out here so
// callers see only the active bindings.
//
// The split is on the first whitespace: everything up to that
// whitespace (sans the `[idx]` suffix) is the hook name, everything
// after is the command body. tmux escapes embedded quotes with
// backslashes; we leave that escaping intact so a caller round-tripping
// through set-hook + ShowHooks sees the same string they installed.
func parseHookOutput(out, target string) []HookEntry {
	hooks := []HookEntry{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		// Split on the first whitespace. SplitN with n=2 keeps any
		// embedded whitespace in the command body intact (e.g.
		// `display-message "x y z"`).
		parts := strings.SplitN(line, " ", 2)
		key := parts[0]
		if key == "" {
			continue
		}
		// Hooks have an `[N]` array-index suffix; non-hook options
		// (e.g. `default-shell`, `status-format[0]`) also carry the
		// suffix on indexed values. We discriminate by whether the
		// row carries a command body — `-H` lists unset hooks as the
		// bare name with no value, and we only return rows with a
		// command.
		if len(parts) < 2 {
			continue
		}
		// Strip the `[idx]` suffix so the surfaced name is the bare
		// hook event. Without this strip a caller iterating multiple
		// bindings would see distinct keys (`pane-died[0]`,
		// `pane-died[1]`) for the same conceptual hook.
		if i := strings.IndexByte(key, '['); i >= 0 {
			key = key[:i]
		}
		// Filter out non-hook options (`default-shell`, `status-format`,
		// …) that bubble through `-H`. Only known-hook shapes are
		// listed by `-H` itself; the list is kept conservative so a
		// future tmux release adding a new hook is picked up without
		// a code change. We use the strict heuristic of "does the row
		// have an [N] array index?" — every bound hook is emitted with
		// one, while plain options are emitted without (status-format
		// is the rare exception, which we exclude with an explicit
		// known-non-hook prefix list below).
		if !looksLikeHookLine(parts[0]) {
			continue
		}
		hooks = append(hooks, HookEntry{
			Name:    key,
			Command: parts[1],
			Target:  target,
		})
	}
	return hooks
}

// nonHookOptionPrefixes lists the option-name prefixes `-H` surfaces
// in the same listing as hooks but which are NOT hooks. tmux files
// indexed string options (status-format[N], update-environment[N],
// command-alias[N], …) in the same array-style table that hooks use,
// so the bare "row carries [N]" heuristic isn't enough to discriminate.
// We exclude the known-non-hook prefixes by name; the list is kept
// short on purpose because a future tmux release could grow it, and
// the cost of a missed exclusion is one spurious row in the output —
// not a correctness failure.
var nonHookOptionPrefixes = []string{
	"status-format[",
	"update-environment[",
	"command-alias[",
	"terminal-features[",
	"terminal-overrides[",
	"user-keys[",
}

// looksLikeHookLine reports whether the leading token of a
// `show-options -H` row is a hook binding rather than an indexed
// non-hook option. Returns true only when the token carries an `[N]`
// array suffix AND its prefix is not one of the known non-hook
// option families. The combined check is deliberately conservative:
// missing a real hook is far worse than emitting an extra row, but
// the known-non-hook prefix list catches the only handful of false
// positives `show-options -H` produces in practice.
func looksLikeHookLine(token string) bool {
	if !strings.Contains(token, "[") {
		return false
	}
	for _, p := range nonHookOptionPrefixes {
		if strings.HasPrefix(token, p) {
			return false
		}
	}
	return true
}
