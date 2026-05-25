package server

import (
	"context"
	"encoding/json"
	"strings"
)

// showWindowOptionsToolDefs holds the JSON Schema for the
// show_window_options tool. It is appended onto the main toolDefs slice
// from this file's init() so the registration site stays close to the
// handler — and the dispatcher in tools.go only needs the single
// name → handler entry.
//
// show_window_options is the read-side sibling of set_window_option:
// where set_window_option mutates per-window flags (synchronize-panes,
// automatic-rename, mode-keys, …), this tool reports the values an
// agent introspecting the window currently sees. show_options
// (server/session/window scopes) is intentionally kept distinct because
// its argument shape is different (scope discriminator vs. plain
// target+name) and its output shape is a flat map vs. an ordered list.
var showWindowOptionsToolDefs = []map[string]any{
	{
		"name": "show_window_options",
		"description": "Return the resolved tmux window-options table at the requested target. " +
			"Wraps `tmux show-window-options [-g] [-t TARGET] [OPTION]`. Pass `target` " +
			"as `<session>` or `<session>:<window>` to scope to a specific window; " +
			"omit it to let tmux pick its current target. Pass `name` to fetch a " +
			"single option (the response then carries at most one entry; an unset " +
			"option returns an empty list — not an error). Set `global=true` to " +
			"read the global window-options defaults (the `-g` view) instead of the " +
			"per-window override map. Sister of `set_window_option` on the write " +
			"side; complements `show_options` for the server/session/window scopes.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":        "string",
					"description": "Window target. `<session>` or `<session>:<window>`; len 1-129 (session ≤64, window ≤64, plus colon).",
					"maxLength":   maxSessionNameLen + 1 + maxWindowNameLen,
				},
				"name": map[string]any{
					"type":        "string",
					"description": "Option name to fetch (e.g. synchronize-panes, mode-keys). Empty queries every option on the target.",
					"maxLength":   maxOptionNameLen,
				},
				"global": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, query tmux's `-g` defaults instead of the per-window override map.",
				},
			},
			// show_window_options's surface is locked to (target, name,
			// global) today; an unknown field is far more likely a typo
			// than a future capability we forgot to advertise, so reject
			// it up front rather than silently ignore it. Mirrors
			// choose_tree's contract.
			"additionalProperties": false,
		},
	},
}

// maxOptionNameLen bounds the OPTION positional. tmux's own option
// keys top out around 30 characters (e.g. `automatic-rename-format`),
// so 64 leaves plenty of headroom for the array-style suffix
// (`command-alias[12]`) without letting a hostile caller smuggle a
// kilobyte of garbage past validation. Re-used here so the schema's
// maxLength stays in lockstep with whatever runtime guard the handler
// might add later.
const maxOptionNameLen = 64

func init() {
	// Register show_window_options onto the main toolDefs slice. Doing
	// this from init() keeps the new tool surface entirely contained in
	// this file (apart from a single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing. The read-only allowlist membership lives in readonly.go
	// alongside the other inspection-only tool names.
	toolDefs = append(toolDefs, showWindowOptionsToolDefs...)
}

// showWindowOptions drives [tmuxctl.Controller.ShowWindowOptions] and
// serialises the result to the standard `{"content":[{"type":"text",
// "text":"<json>"}]}` envelope MCP expects from a tools/call. The
// response shape is `{"options": [{"name": ..., "value": ...}]}` — a
// list (not a map) so the wire ordering matches what
// `tmux show-window-options` itself prints (alphabetical), which is what
// callers rendering the response usually want.
//
// Validation:
//
//   - target: optional. When non-empty, must satisfy the same regex/length
//     policy session refs and window targets share — either `<session>`
//     (validateSessionRef) or `<session>:<window>` (validated as session
//     left of the colon, window right of it).
//   - name: optional, capped at maxOptionNameLen.
//   - global: optional, defaults to false.
//
// Unknown session/window references surface via the wrapped
// errs.ErrSessionNotFound sentinel, which the JSON-RPC layer maps to
// CodeSessionNotFound (-32000). Empty results (no overrides on the
// target, or an unset OPTION positional) come back as
// `{"options": []}` so a client can iterate uniformly.
func (t *Tools) showWindowOptions(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target string `json:"target"`
		Name   string `json:"name"`
		Global bool   `json:"global"`
	}
	// json.Unmarshal on an empty payload is fine — the schema permits
	// `arguments: {}` here, and every field is optional. Mirrors
	// choose_tree's "len(raw)==0 is valid" contract.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("show_window_options: %v", err)
		}
	}
	if args.Target != "" {
		if rerr := validateWindowTargetRef(args.Target); rerr != nil {
			return nil, rerr
		}
	}
	if args.Name != "" {
		if len(args.Name) > maxOptionNameLen {
			return nil, invalidParams("show_window_options: name length %d exceeds %d", len(args.Name), maxOptionNameLen)
		}
	}
	resolved := t.resolveWindowTarget(args.Target)
	entries, err := t.Ctl.ShowWindowOptions(ctx, resolved, args.Name, args.Global)
	if err != nil {
		return nil, internalError(err)
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"name":  e.Name,
			"value": e.Value,
		})
	}
	return jsonBlock(map[string]any{"options": out})
}

// validateWindowTargetRef validates the optional `target` argument
// passed to show_window_options. The shape is either `<session>` (bare
// session reference) or `<session>:<window>` (qualified target). The
// session half must satisfy the existing session-ref policy; the
// window half (when present) must satisfy validateWindowTarget. Empty
// is rejected by the caller — this helper assumes the value is
// already non-empty so its diagnostics quote the offending input.
//
// Kept local to this file (rather than added to validate.go) because
// the show_window_options surface is the only place a user-supplied
// `<session>[:<window>]` reference flows through with both halves
// optional; pulling the helper into the shared file would force every
// call site that today uses validateSessionRef + validateWindowTarget
// individually to migrate, which is out of scope here.
func validateWindowTargetRef(target string) *rpcError {
	idx := strings.Index(target, ":")
	if idx < 0 {
		return validateSessionRef(target)
	}
	if rerr := validateSessionRef(target[:idx]); rerr != nil {
		return rerr
	}
	return validateWindowTarget(target[idx+1:])
}

// resolveWindowTarget rewrites a `<session>[:<window>]` reference so the
// session half picks up the configured -session-prefix. Empty input is
// returned unchanged so the controller-layer "no -t" branch fires
// naturally. The window half (if any) is left intact — only the session
// component participates in the prefix namespace.
func (t *Tools) resolveWindowTarget(target string) string {
	if t == nil || t.SessionPrefix == "" || target == "" {
		return target
	}
	idx := strings.Index(target, ":")
	if idx < 0 {
		return t.SessionPrefix + target
	}
	return t.SessionPrefix + target[:idx] + target[idx:]
}
