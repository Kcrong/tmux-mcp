package server

import (
	"context"
	"encoding/json"
	"strings"
)

// newWindowToolDef holds the JSON Schema for the `new_window` tool.
//
// `new_window` is a thinner cousin of the existing `window_create`: both
// wrap `tmux new-window`, but `new_window` returns a structured
// JSON object (session, window_index, window_id, window_name) instead
// of a human-readable text block, and exposes the `after_index` knob
// so an agent can ask tmux to insert the new window at a specific slot
// inside the session rather than always appending.
//
// We register it through a dedicated init() so the schema and dispatch
// case live in this file — the same pattern tools_signal.go and
// tools_session_rename.go follow for self-contained tools.
var newWindowToolDef = map[string]any{
	"name": "new_window",
	"description": "Create a new window inside an existing tmux session via `tmux new-window` and return " +
		"the structured identity tmux assigned. `name` is the optional `-n` label (auto-assigned " +
		"when omitted). `command` is the optional initial command (defaults to the user's shell); " +
		"newlines are rejected up front because they would be silently swallowed by tmux's command " +
		"parser. `after_index` inserts the new window after that existing index; omit to let tmux " +
		"append at the next free slot. `select` (default true) controls whether tmux focuses the " +
		"new window — set false to create it in the background (`-d`). The response carries " +
		"`session`, `window_index` (numeric), `window_id` (`@N`, stable across renames), and " +
		"`window_name` so callers can chain follow-up tools by either index or id.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"session": map[string]any{
				"type":        "string",
				"description": "Existing session name; len 1-64, [A-Za-z0-9_-].",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Optional window name; len 1-64, [A-Za-z0-9_-].",
			},
			"command": map[string]any{
				"type":        "string",
				"description": "Optional initial command; defaults to the user's shell. Newlines are refused.",
			},
			"after_index": map[string]any{
				"type":        "integer",
				"minimum":     0,
				"description": "Optional. Insert the new window after this existing window index. Omit to append.",
			},
			"select": map[string]any{
				"type":        "boolean",
				"default":     true,
				"description": "When true (default), tmux focuses the new window. False creates it in the background (-d).",
			},
		},
		"required":             []string{"session"},
		"additionalProperties": false,
	},
}

func init() {
	// Register on the package-level toolDefs so tools/list advertises the
	// new tool out of the box. Like the other window tools, this runs
	// before any *Tools instance is constructed, so the per-instance defs
	// slice picks it up via the lazy-seed in snapshotDefs.
	toolDefs = append(toolDefs, newWindowToolDef)
}

// validateNewWindowCommand enforces the only command-shape rule the
// boundary cares about: tmux's command parser silently splits on
// newlines, so a multi-line `command` would be mis-interpreted as
// multiple shell commands. Other characters (spaces, quotes, shell
// metas) are fine because tmux invokes the command via /bin/sh -c and
// the surrounding argv passing handles quoting for us.
//
// Empty is permitted — the handler skips the command argument
// entirely when nothing was supplied.
func validateNewWindowCommand(command string) *rpcError {
	if command == "" {
		return nil
	}
	if strings.ContainsAny(command, "\r\n") {
		return invalidParams("command: must not contain newlines")
	}
	return nil
}

// newWindow drives tmuxctl.Controller.NewWindow. The handler validates
// the session reference, the optional window name, the command shape,
// and the boolean default for `select` before any tmux command runs.
// The successful response is a JSON block carrying the structured
// identity tmux just assigned — distinct from the older window_create
// path which returns a human-readable text block.
func (t *Tools) newWindow(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session string `json:"session"`
		Name    string `json:"name"`
		Command string `json:"command"`
		// *int so we can distinguish "after_index absent (append)" from
		// "after_index=0 (insert after the first window)". The schema's
		// minimum=0 is enforced here too — a negative value would
		// otherwise collide with the -1 sentinel the tmuxctl layer uses
		// to mean "no preference", and an agent that sent -5 deserves a
		// clear refusal rather than silent appending.
		AfterIndex *int `json:"after_index"`
		// *bool so we can distinguish "select absent (default true)"
		// from "select=false (explicit -d)".
		Select *bool `json:"select"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("new_window: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	if rerr := validateWindowName(args.Name); rerr != nil {
		return nil, rerr
	}
	if rerr := validateNewWindowCommand(args.Command); rerr != nil {
		return nil, rerr
	}
	// Translate the optional after_index into the -1 "no preference"
	// sentinel the tmuxctl layer expects when the field is absent.
	afterIndex := -1
	if args.AfterIndex != nil {
		if *args.AfterIndex < 0 {
			return nil, invalidParams("after_index: must be >= 0")
		}
		afterIndex = *args.AfterIndex
	}
	sel := true
	if args.Select != nil {
		sel = *args.Select
	}
	res, err := t.Ctl.NewWindow(ctx, args.Session, args.Name, args.Command, afterIndex, sel)
	if err != nil {
		return nil, internalError(err)
	}
	return jsonBlock(map[string]any{
		"session":      res.Session,
		"window_index": res.Index,
		"window_id":    res.ID,
		"window_name":  res.Name,
	})
}
