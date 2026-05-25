package server

import (
	"context"
	"encoding/json"
)

// nextWindowToolDef holds the JSON Schema for the `next_window` tool.
// Registered through this file's init() so the schema and dispatch case
// stay co-located with the handler — the same self-contained pattern
// kill_window / new_window / swap_window follow.
//
// `next_window` wraps `tmux next-window`: it advances the session's
// active window pointer one step forward (wrapping to the first window
// after the last). The optional `with_alert` knob maps to tmux's `-a`
// flag, which makes the step skip past windows without an
// activity/bell alert and land on the next one that has — the load-
// bearing flag for an agent watching a long-lived session for whatever
// raised the alert. Pairs with `window_select` (jump to a specific
// target) by offering the "step forward" idiom for the case where the
// caller doesn't have a concrete next index in hand.
var nextWindowToolDef = map[string]any{
	"name": "next_window",
	"description": "Advance the active window pointer to the next window in the session via " +
		"`tmux next-window -t <target>`. Wraps to the first window after the last, mirroring " +
		"tmux's interactive `next-window` keybinding. `target` is the session reference; " +
		"`with_alert` (default false) maps to tmux's `-a` flag and makes the step skip " +
		"to the next window with a monitor-activity / monitor-bell alert. Pairs with " +
		"window_select for the \"step forward\" idiom when the caller does not know the " +
		"concrete next index up front.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{
				"type":        "string",
				"description": "Existing session name; len 1-64, [A-Za-z0-9_-].",
			},
			"with_alert": map[string]any{
				"type":        "boolean",
				"default":     false,
				"description": "When true, skip to the next window with an activity alert (tmux's `-a`).",
			},
		},
		"required":             []string{"target"},
		"additionalProperties": false,
	},
}

func init() {
	// Register on the package-level toolDefs so tools/list advertises
	// next_window out of the box. Like the other window-management
	// tools, this runs before any *Tools instance is constructed, so
	// the per-instance defs slice picks it up via the lazy-seed in
	// snapshotDefs.
	toolDefs = append(toolDefs, nextWindowToolDef)
}

// nextWindow drives tmuxctl.Controller.NextWindow. The handler validates
// the session reference up front so a malformed call lands on
// CodeInvalidParams (-32602) before any tmux command runs, then forwards
// the optional `with_alert` flag verbatim. On success the response is
// the same trivial "ok" status text block window_select returns —
// callers chain into list_windows / capture if they want to confirm the
// active flag actually moved.
//
// `target` is the session reference (a tmux session target). We resolve
// it through resolveSessionRef so a deployment running with
// -session-prefix lands on the actual prefixed tmux session, then hand
// the resolved name to the controller. This mirrors the routing every
// other session-bearing window tool uses.
func (t *Tools) nextWindow(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target string `json:"target"`
		// *bool so we can distinguish "with_alert absent (default false)"
		// from "with_alert=false (explicit)". The schema's documented
		// default of false applies identically whether the field was
		// missing, null, or explicitly false.
		WithAlert *bool `json:"with_alert"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("next_window: %v", err)
	}
	if rerr := validateSessionRef(args.Target); rerr != nil {
		return nil, rerr
	}
	withAlert := false
	if args.WithAlert != nil {
		withAlert = *args.WithAlert
	}
	if err := t.Ctl.NextWindow(ctx, t.resolveSessionRef(args.Target), withAlert); err != nil {
		return nil, internalError(err)
	}
	return textBlock("ok"), nil
}
