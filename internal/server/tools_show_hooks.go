package server

import (
	"context"
	"encoding/json"
)

// showHooksToolDefs holds the JSON Schema for the show_hooks tool. It
// is appended onto the main toolDefs slice via the package init() in
// this file so the registration site stays close to the handler — and
// the dispatcher in tools.go only needs the single name → handler
// entry.
//
// Read-only: show_hooks issues nothing but `tmux show-options -H` /
// `-wH` invocations under the hood. It never installs, removes, or
// otherwise mutates a hook binding (that is set_hook's job), so the
// tool is safe to expose under -read-only and is included in the
// readOnlyTools allowlist alongside its sister inspector show_options.
var showHooksToolDefs = []map[string]any{
	{
		"name": "show_hooks",
		"description": "List every hook binding the tmux server currently holds. Wraps " +
			"`tmux show-options -H` / `-wH` (the `-H` flag exposes hook entries that " +
			"are otherwise hidden in show-options output). When `target` is omitted, " +
			"the response covers both the server-global hook tables (server-class via " +
			"`-gH`, window-class via `-gwH`) AND every existing session's hook tables. " +
			"When `target` names a session, only that session's hook tables are " +
			"scanned, so the response is scoped to bindings the named session " +
			"actually carries. Output is `{\"hooks\": [{\"name\": \"<event>\", " +
			"\"command\": \"<tmux-command>\", \"target\": \"<session-or-empty>\"}, " +
			"...]}` — `target` is the empty string for global bindings and the session " +
			"name for per-session bindings. Sister of `set_hook` (which installs / " +
			"removes the bindings this tool enumerates). Read-only: never mutates tmux " +
			"state, allowed under `-read-only`.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":        "string",
					"maxLength":   maxSessionNameLen,
					"description": "Optional session name. When set, only this session's hook tables are scanned (`-tTARGET`). When omitted, both server-global tables and every session's tables are scanned. Same regex/length policy as session names (`^[A-Za-z0-9_-]+$`, len 1-64).",
				},
			},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register show_hooks onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from a single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, showHooksToolDefs...)
}

// showHooks drives [tmuxctl.Controller.ShowHooks] and serialises the
// result to the standard `{"content":[{"type":"text","text":"<json>"}]}`
// envelope MCP expects for tools/call. The output shape is intentionally
// flat — `{ "hooks": [{...}, {...}] }` — so future additions (e.g.
// surfacing the resolved scope back to the caller) do not break callers
// that read the fields they care about.
//
// The handler accepts an optional `target` argument. When set, it is
// validated against the session-name policy before tmux is consulted
// so a stray quote / shell metachar can never reach tmux's argv. When
// absent, the controller's full sweep runs — every session's hooks plus
// the server-global tables.
//
// `target` is rewritten through the configured -session-prefix the same
// way every other session-targeting tool does, so a multi-tenant
// deployment routes the show-options query at the prefixed tmux session
// the caller actually owns. The Target field on each returned hook
// entry is stamped with the user-supplied logical name (not the
// resolved tmux name) so the response stays readable on the caller's
// side regardless of whether a prefix is in play.
//
// Unknown target sessions surface via the wrapped errs.ErrSessionNotFound
// sentinel, which the JSON-RPC layer maps to CodeSessionNotFound
// (-32000). Empty server / no-bindings paths return `{"hooks": []}`
// with no error so a caller can branch on len() without first having
// to catch a non-existent failure.
func (t *Tools) showHooks(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target string `json:"target"`
	}
	// Allow an explicit `null` / empty body so a tools/call frame with
	// `arguments: {}` flows through the no-target branch; that's the
	// load-bearing default — "give me every binding".
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("show_hooks: %v", err)
		}
	}
	// resolved is the actual tmux session name to probe. We pass an
	// empty target through the controller as a "scan everything" signal
	// (the controller's ShowHooks branch treats target=="" as the full
	// sweep). When target is non-empty we validate the user-facing
	// shape first, then rewrite through the configured prefix.
	var resolved string
	if args.Target != "" {
		if rerr := validateSessionRef(args.Target); rerr != nil {
			return nil, rerr
		}
		resolved = t.resolveSessionRef(args.Target)
	}
	hooks, err := t.Ctl.ShowHooks(ctx, resolved)
	if err != nil {
		return nil, internalError(err)
	}
	// When a -session-prefix is configured the controller stamps the
	// resolved (prefixed) tmux name into HookEntry.Target. Translate
	// that back to the user's logical name so the response stays
	// internally consistent — the same string the caller passed as
	// `target` is what comes back in the per-row Target field.
	out := make([]map[string]any, 0, len(hooks))
	for _, h := range hooks {
		row := map[string]any{
			"name":    h.Name,
			"command": h.Command,
			"target":  t.stripSessionPrefix(h.Target),
		}
		out = append(out, row)
	}
	return jsonBlock(map[string]any{"hooks": out})
}
