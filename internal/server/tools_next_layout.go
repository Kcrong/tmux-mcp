package server

import (
	"context"
	"encoding/json"
)

// nextLayoutToolDef holds the JSON Schema for the `next_layout` tool.
// Registered through this file's init() so the schema and dispatch case
// stay co-located with the handler — the same self-contained pattern
// next_window / select_layout / swap_window follow.
//
// `next_layout` wraps `tmux next-layout`: it cycles the targeted
// window onto the next preset arrangement (even-horizontal →
// even-vertical → main-horizontal → main-vertical → tiled), wrapping
// to the first preset after the last. Pairs with `select_layout` (which
// takes a SPECIFIC layout name) by offering the "give me the next
// preset" affordance an agent reaches for when it doesn't care which
// layout, just wants to rotate.
//
// next_layout MUTATES tmux state (it changes the active window's pane
// arrangement), so it is intentionally NOT part of the read-only
// allowlist in readonly.go and is rejected when the operator runs the
// server with -read-only.
var nextLayoutToolDef = map[string]any{
	"name": "next_layout",
	"description": "Cycle the targeted window onto the next preset layout via " +
		"`tmux next-layout -t <target>`. Walks tmux's ordered preset ring " +
		"(even-horizontal → even-vertical → main-horizontal → main-vertical → tiled) " +
		"and wraps to the first preset after the last. `target` is the session " +
		"reference; tmux applies the cycle to that session's active window. Pairs " +
		"with `select_layout` (which takes a specific layout name) by offering the " +
		"\"give me the next preset\" idiom when the caller just wants to rotate.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{
				"type":        "string",
				"description": "Existing session name; len 1-64, [A-Za-z0-9_-]. tmux applies the cycle to the session's active window.",
			},
		},
		"required":             []string{"target"},
		"additionalProperties": false,
	},
}

func init() {
	// Register on the package-level toolDefs so tools/list advertises
	// next_layout out of the box. Like the other window-management
	// tools, this runs before any *Tools instance is constructed, so
	// the per-instance defs slice picks it up via the lazy-seed in
	// snapshotDefs.
	toolDefs = append(toolDefs, nextLayoutToolDef)
}

// nextLayout drives tmuxctl.Controller.NextLayout. The handler
// validates the session reference up front so a malformed call lands
// on CodeInvalidParams (-32602) before any tmux command runs, then
// forwards the resolved (prefix-aware) target to the controller. On
// success the response is the same trivial "ok" status text block
// next_window / window_select return — callers chain into
// `display_message` against `#{window_layout}` if they want to confirm
// the actual dump that landed.
//
// `target` is the session reference (a tmux session target). We
// resolve it through resolveSessionRef so a deployment running with
// -session-prefix lands on the actual prefixed tmux session, then hand
// the resolved name to the controller. This mirrors the routing every
// other session-bearing window tool uses.
//
// A missing session surfaces as CodeSessionNotFound (-32000) via
// internalError → errs.CodeOf, mirroring window_select / select_layout.
func (t *Tools) nextLayout(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("next_layout: %v", err)
	}
	if rerr := validateSessionRef(args.Target); rerr != nil {
		return nil, rerr
	}
	if err := t.Ctl.NextLayout(ctx, t.resolveSessionRef(args.Target)); err != nil {
		return nil, internalError(err)
	}
	return textBlock("ok"), nil
}
