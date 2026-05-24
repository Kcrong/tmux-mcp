package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// chooseTreeToolDefs holds the JSON Schema for the choose_tree tool.
// It is appended onto the main toolDefs slice from this file's init()
// so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
var chooseTreeToolDefs = []map[string]any{
	{
		"name": "choose_tree",
		"description": "Snapshot the (session, window) tree this server's tmux holds via the non- " +
			"interactive form of `tmux choose-tree`. Useful for an LLM agent that needs to " +
			"\"see the whole topology\" of the server in one call without iterating " +
			"`list_sessions` × `list_windows`. The interactive picker is intentionally not " +
			"reachable: this tool always returns a structured snapshot. Pass `scope=\"all\"` " +
			"(default) to walk every window on the server, `scope=\"session\"` with " +
			"`session=NAME` to scope to one session, or `scope=\"window\"` with both " +
			"`session=NAME` and `window=WIN` to drill down to a single window. Each row " +
			"carries the session name, window index, window name, pane count, and active " +
			"flag — enough to build a `session:index` target for any follow-up call.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"scope": map[string]any{
					"type":        "string",
					"enum":        []string{"all", "session", "window"},
					"default":     "all",
					"description": "What slice of the tree to return. \"all\" walks every window on the server.",
				},
				"session": map[string]any{
					"type":        "string",
					"maxLength":   maxSessionNameLen,
					"description": "Required when scope=\"session\" or scope=\"window\". len 1-64, [A-Za-z0-9_-].",
				},
				"window": map[string]any{
					"type":        "string",
					"maxLength":   maxWindowNameLen,
					"description": "Required when scope=\"window\". Window name (1-64, [A-Za-z0-9_-]) or numeric index.",
				},
			},
			// choose_tree's surface is locked to (scope, session,
			// window) today; an unknown field is far more likely a
			// typo than a future capability we forgot to advertise,
			// so reject it up front rather than silently ignore it.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register choose_tree onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in
	// this file (apart from the single dispatcher case in tools.go)
	// and avoids touching the shared toolDefs literal that other PRs
	// are editing. The read-only allowlist membership lives in
	// readonly.go alongside the other inspection-only tool names.
	toolDefs = append(toolDefs, chooseTreeToolDefs...)
}

// chooseTree drives tmuxctl.Controller.ChooseTree and serialises the
// result to the standard `{"content":[{"type":"text","text":"<json>"}]}`
// envelope MCP expects from a tools/call. The response shape is a flat
// object keyed by "rows" so a future filter (e.g. an "active_only"
// knob) can be added without breaking callers that iterate the list.
//
// The (scope, session, window) trio is validated up front:
//
//   - scope defaults to "all" (full-tree snapshot).
//   - scope="session" requires `session`; `window` is rejected.
//   - scope="window" requires both `session` and `window`.
//
// Session and window references reuse the conservative regex/length
// policy applied across the rest of the surface so a malformed value
// gets a fast -32602 rejection before tmux is consulted. Unknown
// session names surface via the wrapped errs.ErrSessionNotFound which
// the dispatcher maps to CodeSessionNotFound.
func (t *Tools) chooseTree(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Scope   string `json:"scope"`
		Session string `json:"session"`
		Window  string `json:"window"`
	}
	// json.Unmarshal on an empty payload is fine — the schema permits
	// `arguments: {}` here, and the zero value of args.Scope is
	// normalised to "all" below. Mirrors list_clients / list_windows.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("choose_tree: %v", err)
		}
	}
	if args.Scope == "" {
		args.Scope = "all"
	}

	// scopeArg is the controller-layer scope string we hand to
	// ChooseTree. Build it from the validated inputs so the handler
	// stays the single point of truth on the shape — the controller
	// just consumes whichever literal we decided is well-formed.
	var scopeArg string
	switch args.Scope {
	case "all":
		// All-tree snapshot: session/window are not used. Reject
		// stray values up front so a caller that meant "session"
		// scope but forgot to set scope=session sees a fast,
		// pointed error rather than silently getting a server-wide
		// listing.
		if args.Session != "" {
			return nil, invalidParams("choose_tree: scope=\"all\" does not accept a session")
		}
		if args.Window != "" {
			return nil, invalidParams("choose_tree: scope=\"all\" does not accept a window")
		}
		scopeArg = ""
	case "session":
		if rerr := validateSessionRef(args.Session); rerr != nil {
			return nil, rerr
		}
		if args.Window != "" {
			return nil, invalidParams("choose_tree: scope=\"session\" does not accept a window")
		}
		scopeArg = "session " + t.resolveSessionRef(args.Session)
	case "window":
		if rerr := validateSessionRef(args.Session); rerr != nil {
			return nil, rerr
		}
		if rerr := validateWindowTarget(args.Window); rerr != nil {
			return nil, rerr
		}
		scopeArg = "window " + t.resolveSessionRef(args.Session) + ":" + args.Window
	default:
		// The schema's enum already filters this surface, but we
		// keep a defensive switch default so a hand-crafted call
		// that bypasses schema validation sees a fast -32602 instead
		// of an unscoped fall-through.
		return nil, invalidParams("choose_tree: scope %q must be one of [all, session, window]", args.Scope)
	}

	rows, err := t.Ctl.ChooseTree(ctx, scopeArg)
	if err != nil {
		return nil, internalError(fmt.Errorf("choose_tree: %w", err))
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			// Echo the logical session name (strip the configured
			// -session-prefix) so a deployment that pins a prefix
			// never leaks the prefixed identity back to the caller.
			// Bare names (no prefix) round-trip unchanged.
			"session":      t.stripSessionPrefix(r.Session),
			"window_index": r.WindowIndex,
			"window_name":  r.WindowName,
			"pane_count":   r.PaneCount,
			"active":       r.Active,
		})
	}
	return jsonBlock(map[string]any{"rows": out})
}
