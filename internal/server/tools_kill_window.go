package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// killWindowToolDefs holds the JSON Schema for the kill_window tool.
// It is appended onto the main toolDefs slice from this file's init()
// so the registration site stays close to the handler — the dispatcher
// in tools.go only needs the single name → handler entry.
//
// kill_window is the kill-X surface counterpart to pane_kill /
// kill_all_sessions: it removes a single window from a session and
// honours tmux's natural "killing the only window also reaps the
// session" cascade by surfacing the cascade as a structured
// `session_killed` flag instead of refusing the call. Sibling tool
// `window_kill` keeps the older, refusal-based contract for callers
// that prefer to be told to reach for session_kill themselves.
var killWindowToolDefs = []map[string]any{
	{
		"name": "kill_window",
		"description": "Destroy a single window in a session via `tmux kill-window -t <session>:<window>`. " +
			"Honours tmux's natural cascade: when the targeted window is the only window left in the " +
			"session, killing it also reaps the session — that case is reported as " +
			"`{\"killed\": true, \"session_killed\": true}` rather than rejected. The common case " +
			"(window goes away, session lives on) returns `{\"killed\": true}`. " +
			"`window` may be a name (1-64, [A-Za-z0-9_-]) or a numeric index. Pairs with " +
			"pane_kill / kill_all_sessions to round out the kill-X surface.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "Existing session name; len 1-64, [A-Za-z0-9_-].",
				},
				"window": map[string]any{
					"type":        "string",
					"description": "Window name (len 1-64, [A-Za-z0-9_-]) or numeric index (\\d+).",
				},
			},
			"required":             []string{"session", "window"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register kill_window onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, killWindowToolDefs...)
}

// killWindow drives tmuxctl.Controller.KillWindowReport. Up-front it
// validates the session and window target, then asks tmux to kill the
// window. On success the response is a small JSON ack: `{"killed":
// true}` for the common case, or `{"killed": true, "session_killed":
// true}` when the kill collapsed the session because the targeted
// window was the only one. Mirrors pane_kill's response shape so a
// caller stitching kill-X tools together sees a uniform contract.
//
// Distinct from the older window_kill tool: window_kill refuses to
// destroy the only window of a session (returns CodeInvalidParams with
// a "use session_kill instead" hint), while kill_window honours the
// natural tmux cascade and reports it explicitly. Both tools coexist so
// callers can pick the contract that suits them.
func (t *Tools) killWindow(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session string `json:"session"`
		Window  string `json:"window"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("kill_window: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	// validateWindowTarget rejects both empty strings and shapes that
	// would slip past tmux's quoting (spaces, colons, dots) — the same
	// guard window_kill uses, so a malformed target trips
	// CodeInvalidParams before any tmux command runs.
	if rerr := validateWindowTarget(args.Window); rerr != nil {
		return nil, rerr
	}
	res, err := t.Ctl.KillWindowReport(ctx, args.Session, args.Window)
	if err != nil {
		return nil, internalError(fmt.Errorf("kill_window: %w", err))
	}
	out := map[string]any{"killed": res.Killed}
	if res.SessionKilled {
		// Only emit session_killed when the cascade actually fired —
		// keeps the common-case payload minimal and matches the
		// pane_kill `{"killed": true}` shape, which most callers will
		// already be parsing. Forget snapshot history for the dead
		// session so we don't leak per-session entries across many
		// create/kill cycles, mirroring session_kill's cleanup.
		out["session_killed"] = true
		t.Snap.Forget(args.Session)
	}
	return jsonBlock(out)
}
