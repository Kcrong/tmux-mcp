package server

import (
	"context"
	"encoding/json"
)

// lastWindowToolDefs holds the JSON Schema for the last_window tool.
// It is appended onto the main toolDefs slice from this file's init()
// so the registration site stays close to the handler — the dispatcher
// in tools.go only needs the single name → handler entry.
//
// last_window wraps tmux's `last-window` command: tmux remembers the
// most recently focused window per session and toggles between the
// "current" and the "last" slot. This is the equivalent of the
// interactive `prefix + l` (or the customary `Alt-a`) hot key, which
// agents reach for to flip between two related contexts (editor /
// build, code / repl) without having to remember the destination's
// index or name. Pairs with window_select (explicit target) and
// window_create / window_kill (lifecycle) to round out the per-window
// surface — and is the natural complement to next_window / previous_window
// (round-robin walks) landing alongside this tool.
var lastWindowToolDefs = []map[string]any{
	{
		"name": "last_window",
		"description": "Switch the named session back to its previously-active window via " +
			"`tmux last-window -t <target>`. tmux remembers the last active window per session and " +
			"toggles between the \"current\" and the \"last\" slot — the equivalent of the interactive " +
			"`prefix + l` (or the customary `Alt-a`) hot key. Useful for flipping between two related " +
			"contexts (editor / build, code / repl) without having to remember the destination's index " +
			"or name. Pairs with window_select for explicit targets and window_create / window_kill " +
			"for lifecycle.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":        "string",
					"description": "Existing session name; len 1-64, [A-Za-z0-9_-].",
				},
			},
			"required":             []string{"target"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register last_window onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, lastWindowToolDefs...)
}

// lastWindow drives tmuxctl.Controller.LastWindow. The handler validates
// the session reference up front so a malformed argument fails fast with
// CodeInvalidParams (-32602) before any tmux command runs. On success the
// response is the same trivial "ok" status text block window_select uses
// — callers chain into list_windows / capture if they want to confirm
// the active flag actually moved.
//
// `target` accepts a tmux session reference (matching the standard
// session-name policy: len 1-64, `^[A-Za-z0-9_-]+$`). It is reused as
// the literal `tmux last-window -t <target>` argument; the validator
// guards against shell metacharacters slipping through to the tmux argv.
func (t *Tools) lastWindow(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("last_window: %v", err)
	}
	// validateSessionRef enforces both the empty-string check and the
	// regex/length policy; reusing it keeps the contract identical to
	// every other session-keyed tool (window_select / window_rename / …)
	// without re-stating the rule here.
	if rerr := validateSessionRef(args.Target); rerr != nil {
		return nil, rerr
	}
	if err := t.Ctl.LastWindow(ctx, t.resolveSessionRef(args.Target)); err != nil {
		return nil, internalError(err)
	}
	return textBlock("ok"), nil
}
