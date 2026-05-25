package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// maxUnsetHookNameLen caps the up-front length check on the required
// `hook` argument so a hostile or buggy caller cannot blow up tmux's
// argv with a megabyte payload before we even reach the boundary.
// tmux's own hook names are short (longest as of 3.4 is
// "session-window-changed" at 22 chars); 256 is generous while staying
// well under any reasonable shell-arg cap.
const maxUnsetHookNameLen = 256

// unsetHookNameRE matches tmux hook event names accepted by unset_hook.
// The shape `^[a-z][a-z0-9_-]*$` is the conservative lowercase-snake or
// hyphenated form every tmux-documented hook follows (e.g. "pane-died",
// "session-window-changed", "client-attached"). Whitespace, dots,
// colons, uppercase letters, and shell metachars are rejected so a
// stray quote can never reach tmux's argv. The first-character
// constraint (`[a-z]`) blocks numeric-leading or punctuation-leading
// values that would otherwise look like flags to tmux's argv parser.
var unsetHookNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// unsetHookToolDefs holds the JSON Schema for the unset_hook tool. It
// is appended onto the main toolDefs slice from this file's init() so
// the registration site stays close to the handler — the dispatcher in
// tools.go only needs the single name → handler entry.
//
// Mutating: clearing a hook changes tmux's behaviour for every
// subsequent event the daemon sees, so the tool is deliberately
// excluded from the read-only allowlist; a `-read-only` operator must
// not be able to install or remove hooks.
//
// `additionalProperties: false` keeps the schema strict so an agent
// that misnames a field (e.g. "hook_name" instead of "hook") gets a
// fast schema-shaped rejection rather than a silent no-op.
var unsetHookToolDefs = []map[string]any{
	{
		"name": "unset_hook",
		"description": "Remove a previously-set tmux hook via " +
			"`tmux set-hook -u [-g | -w] [-t TARGET] HOOK-NAME` — the " +
			"inverse of `set_hook`. `hook` is the event name to clear " +
			"(e.g. \"pane-died\", \"client-attached\", \"session-created\"). " +
			"Set `global=true` to clear on the server-wide options table " +
			"(`-g`); set `window=true` to clear on the window-options " +
			"table (`-w`); these two flags are mutually exclusive. " +
			"Otherwise the clear lands on the per-session options of " +
			"the resolved `target` session (`-t TARGET`). Returns a " +
			"small JSON ack `{\"unset\": true, \"global\": <bool>, " +
			"\"window\": <bool>, \"hook\": \"<name>\"}` so a caller " +
			"chaining several unset_hook calls can branch on a stable " +
			"shape. Idempotent: clearing a hook that is already absent " +
			"succeeds without an error. Mutating: not allowed under " +
			"`-read-only`.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"hook": map[string]any{
					"type":        "string",
					"maxLength":   maxUnsetHookNameLen,
					"pattern":     unsetHookNameRE.String(),
					"description": "Hook event name to clear (e.g. \"pane-died\", \"client-attached\"). Regex `^[a-z][a-z0-9_-]*$`, len 1-256.",
				},
				"target": map[string]any{
					"type":        "string",
					"maxLength":   maxSessionNameLen,
					"description": "Per-session clear target (`-t TARGET`); required when neither global=true nor window=true. Same regex/length policy as session names. Optional under window=true (omit to use the current window).",
				},
				"global": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, clear on the server-wide options table (`-g`); `target` is ignored. Mutually exclusive with `window=true`.",
				},
				"window": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, clear on the window-options table (`-w`); `target` may name a window or be omitted for the current window. Mutually exclusive with `global=true`.",
				},
			},
			"required":             []string{"hook"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register unset_hook onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, unsetHookToolDefs...)
}

// validateUnsetHookName enforces the conservative hook-name policy on
// the required `hook` argument. Empty is rejected (the schema's
// required list catches the missing-field case but a `"hook": ""`
// body must not silently fall through). Anything outside the regex /
// length policy is rejected before tmux is consulted so a stray quote
// or shell metachar can never reach the daemon's argv.
func validateUnsetHookName(name string) *rpcError {
	if name == "" {
		return invalidParams("unset_hook: hook required")
	}
	if len(name) > maxUnsetHookNameLen {
		return invalidParams("unset_hook: hook length %d out of range [1..%d]", len(name), maxUnsetHookNameLen)
	}
	if !unsetHookNameRE.MatchString(name) {
		return invalidParams("unset_hook: hook %q must match %s", name, unsetHookNameRE.String())
	}
	return nil
}

// unsetHook drives [tmuxctl.Controller.UnsetHook]. The handler
// validates the required `hook` shape and the optional `target` value
// up front so a malformed call sees CodeInvalidParams (-32602) before
// any tmux command runs. The mutual-exclusion of `global` / `window`
// is also enforced at the boundary because plain JSON Schema cannot
// express the contradiction — refusing the shape here means callers
// see a clean -32602 instead of tmux's version-dependent stderr.
//
// The response is a small JSON ack carrying the resolved hook name
// plus the mode flags so a caller chaining several unset_hook calls
// can branch on a stable shape rather than parse a free-form status
// string.
//
// The `target` argument is rewritten through the configured
// -session-prefix the same way every other session-targeting tool
// does, so a multi-tenant deployment routes the unset call at the
// prefixed tmux session the caller actually owns. `global=true` and
// `window=true` (with empty target) skip the rewrite — neither path
// has a per-session target and the controller already ignores the
// value on those paths.
//
// Unknown target sessions surface via the wrapped
// errs.ErrSessionNotFound sentinel, which the JSON-RPC layer maps to
// CodeSessionNotFound (-32000). The same sentinel covers tmux's
// "no such window" / "no current target" stderr shapes the controller
// folds in for the headless-server / missing-window outcomes.
func (t *Tools) unsetHook(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Hook   string `json:"hook"`
		Target string `json:"target"`
		Global bool   `json:"global"`
		Window bool   `json:"window"`
	}
	// Allow an explicit `null` / empty body so a tools/call frame with
	// `arguments: {}` still surfaces the required-field validation
	// below rather than choking on the unmarshal.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("unset_hook: %v", err)
		}
	}
	if rerr := validateUnsetHookName(args.Hook); rerr != nil {
		return nil, rerr
	}
	if args.Global && args.Window {
		return nil, invalidParams("unset_hook: global and window are mutually exclusive")
	}
	resolvedTarget := args.Target
	switch {
	case args.Global:
		// `-g` ignores target; skip both validation and the prefix
		// rewrite. The controller drops the value on this path too.
	case args.Window:
		// `-w` allows an empty target (clear on the current window).
		// Validate only when present so the empty-target shape is not
		// gratuitously refused at the boundary.
		if args.Target != "" {
			if rerr := validateSessionRef(args.Target); rerr != nil {
				return nil, rerr
			}
			resolvedTarget = t.resolveSessionRef(args.Target)
		}
	default:
		// Per-session clear: target is required and must satisfy the
		// session-name policy so a stray quote can't slip through to
		// tmux's argv. The session-prefix rewrite happens after the
		// shape check so validation sees the user-supplied name. The
		// controller wraps a missing-target shape as
		// errs.ErrSessionNotFound, but the boundary still rejects an
		// empty target up front so the caller sees a clean -32602
		// (the documented invalid-params code for this shape) rather
		// than the controller's typed "no current target" surface.
		if rerr := validateSessionRef(args.Target); rerr != nil {
			return nil, rerr
		}
		resolvedTarget = t.resolveSessionRef(args.Target)
	}
	if err := t.Ctl.UnsetHook(ctx, resolvedTarget, args.Hook, args.Global, args.Window); err != nil {
		return nil, internalError(fmt.Errorf("unset_hook: %w", err))
	}
	return jsonBlock(map[string]any{
		"unset":  true,
		"global": args.Global,
		"window": args.Window,
		"hook":   args.Hook,
	})
}
