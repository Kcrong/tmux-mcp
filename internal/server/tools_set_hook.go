package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// maxHookNameLen / maxHookCommandLen cap the up-front length checks on
// the `name` and `command` arguments so a hostile or buggy caller
// cannot blow up tmux's argv with megabyte payloads before we even
// reach the boundary. tmux's own hook names are short (longest as of
// 3.4 is "session-window-changed" at 22 chars); 128 is generous while
// staying well under any reasonable shell-arg cap. Hook bodies are
// arbitrary tmux command lines so the cap there is larger; 4 KiB is
// more than enough for any realistic chain of `display-message` /
// `run-shell` invocations and small enough that a buggy caller cannot
// pin the JSON-RPC writer copying tens of MB into a single tmux argv.
const (
	maxHookNameLen    = 128
	maxHookCommandLen = 4096
)

// hookNameRE matches tmux hook event names. tmux's documented hooks
// follow the conservative shape `[A-Za-z0-9-]+` (e.g. "pane-died",
// "session-window-changed", "client-attached"); we widen marginally
// to also accept underscore for forward-compatibility with custom or
// future hook names. Whitespace, dots, colons, and shell metachars are
// rejected so a stray quote can never reach tmux's argv.
var hookNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// setHookToolDefs holds the JSON Schema for the set_hook tool. It is
// appended onto the main toolDefs slice via the package init() in this
// file so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
//
// Mutating: hooks change tmux's behaviour for every subsequent event
// the daemon sees, so the tool is deliberately excluded from the
// read-only allowlist; a `-read-only` operator must not be able to
// install or remove hooks.
var setHookToolDefs = []map[string]any{
	{
		"name": "set_hook",
		"description": "Bind or unbind a tmux command to a server / session-scoped event via " +
			"`tmux set-hook`. `name` is the event (e.g. \"pane-died\", \"client-attached\", " +
			"\"session-created\"); `command` is the tmux command to run when the event fires " +
			"(e.g. `display-message \"x\"`, `run-shell ./on-pane-died.sh`). Set " +
			"`unset=true` to clear an existing hook (`-u`); on the unset path `command` is " +
			"ignored. Set `global=true` to bind on the server-wide options table (`-g`) so " +
			"every current and future session inherits the hook; otherwise `target` is the " +
			"session whose options table the hook lands on (`-t TARGET`). Returns a small " +
			"JSON ack `{\"set\": true, \"unset\": <bool>, \"global\": <bool>, \"name\": " +
			"\"<hook>\"}` so a caller chaining several set_hook calls can branch on a stable " +
			"shape. Mutating: not allowed under `-read-only`.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"maxLength":   maxHookNameLen,
					"description": "Hook event name (e.g. \"pane-died\", \"client-attached\"). Regex `^[A-Za-z0-9_-]+$`, len 1-128.",
				},
				"command": map[string]any{
					"type":        "string",
					"maxLength":   maxHookCommandLen,
					"description": "tmux command to bind to the event (e.g. `display-message \"x\"`). Required when unset=false; ignored when unset=true. No NUL or control chars.",
				},
				"unset": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, clear the hook (`-u`) instead of binding it. `command` is ignored.",
				},
				"global": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, bind on the server-wide options table (`-g`); `target` is ignored. Otherwise the hook lands on the session named by `target`.",
				},
				"target": map[string]any{
					"type":        "string",
					"maxLength":   maxSessionNameLen,
					"description": "Per-session bind target (`-t TARGET`); required when global=false. Same regex/length policy as session names.",
				},
			},
			"required":             []string{"name"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register set_hook onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, setHookToolDefs...)
}

// validateHookName enforces the conservative hook-name policy on the
// required `name` argument. Empty is rejected (the schema's required
// list catches the missing-field case but a `"name": ""` body must not
// silently fall through). Anything outside the regex/length policy is
// rejected before tmux is consulted so a stray quote / shell metachar
// can never reach the daemon's argv.
func validateHookName(name string) *rpcError {
	if name == "" {
		return invalidParams("set_hook: name required")
	}
	if len(name) > maxHookNameLen {
		return invalidParams("set_hook: name length %d out of range [1..%d]", len(name), maxHookNameLen)
	}
	if !hookNameRE.MatchString(name) {
		return invalidParams("set_hook: name %q must match %s", name, hookNameRE.String())
	}
	return nil
}

// validateHookCommand enforces the conservative command-body policy on
// the optional `command` argument. tmux command lines can contain a
// wide range of characters (quotes, semicolons, environment
// expansions), so this validator only enforces the length cap and a
// blanket "no NUL or other ASCII control chars" check. NUL would
// truncate tmux's argv parser; other control chars (BS, ESC, …) have
// no place in a tmux command line and admitting them risks an
// injected escape sequence taking effect when tmux later renders the
// hook in show-options output.
func validateHookCommand(command string) *rpcError {
	if len(command) > maxHookCommandLen {
		return invalidParams("set_hook: command length %d out of range [0..%d]", len(command), maxHookCommandLen)
	}
	for i := 0; i < len(command); i++ {
		b := command[i]
		// Reject NUL outright (it would terminate tmux's argv parsing)
		// and any other ASCII control byte except tab (0x09) which is
		// occasionally embedded in tmux command lines for formatting.
		if b == 0 {
			return invalidParams("set_hook: command contains NUL byte at offset %d", i)
		}
		if b < 0x20 && b != '\t' {
			return invalidParams("set_hook: command contains control byte 0x%02x at offset %d", b, i)
		}
		if b == 0x7f {
			return invalidParams("set_hook: command contains DEL byte at offset %d", i)
		}
	}
	return nil
}

// setHook drives [tmuxctl.Controller.SetHook]. The handler validates
// the required `name` shape and the optional `command`/`target` values
// up front so a malformed call sees CodeInvalidParams (-32602) before
// any tmux command runs. The response is a small JSON ack carrying the
// resolved name plus the mode flags so a caller chaining several
// set_hook calls can branch on a stable shape rather than parse a
// free-form status string.
//
// The `target` argument is rewritten through the configured
// -session-prefix the same way every other session-targeting tool
// does, so a multi-tenant deployment routes the hook bind at the
// prefixed tmux session the caller actually owns. `global=true` skips
// the rewrite — a `-g` bind has no per-session target and the
// controller already ignores the value on that path.
//
// Unknown target sessions surface via the wrapped
// errs.ErrSessionNotFound sentinel, which the JSON-RPC layer maps to
// CodeSessionNotFound (-32000). The same sentinel covers tmux's
// "no such window" / "invalid option" stderr shapes the controller
// folds in for the same conceptual outcome.
func (t *Tools) setHook(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Name    string `json:"name"`
		Command string `json:"command"`
		Unset   bool   `json:"unset"`
		Global  bool   `json:"global"`
		Target  string `json:"target"`
	}
	// Allow an explicit `null` / empty body so a tools/call frame with
	// `arguments: {}` still surfaces the required-field validation
	// below rather than choking on the unmarshal.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("set_hook: %v", err)
		}
	}
	if rerr := validateHookName(args.Name); rerr != nil {
		return nil, rerr
	}
	// On the bind path the command body is required; on the unset path
	// it is ignored. We still validate the shape when present so a
	// caller that flips `unset` later doesn't discover the policy
	// violation only on the second call.
	if !args.Unset && args.Command == "" {
		return nil, invalidParams("set_hook: command required when unset=false")
	}
	if rerr := validateHookCommand(args.Command); rerr != nil {
		return nil, rerr
	}
	resolvedTarget := args.Target
	if !args.Global {
		// Per-session bind: target is required and must satisfy the
		// session-name policy so a stray quote can't slip through to
		// tmux's argv. The session-prefix rewrite happens after the
		// shape check so validation sees the user-supplied name.
		if rerr := validateSessionRef(args.Target); rerr != nil {
			return nil, rerr
		}
		resolvedTarget = t.resolveSessionRef(args.Target)
	}
	if err := t.Ctl.SetHook(ctx, args.Name, args.Command, resolvedTarget, args.Unset, args.Global); err != nil {
		return nil, internalError(fmt.Errorf("set_hook: %w", err))
	}
	return jsonBlock(map[string]any{
		"set":    true,
		"unset":  args.Unset,
		"global": args.Global,
		"name":   args.Name,
	})
}
