package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// respawnWindowToolDefs holds the JSON Schema for the respawn_window
// tool. It is appended onto the main toolDefs slice from this file's
// init() so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
//
// respawn_window is the window-scoped sibling of respawn_pane: instead
// of restarting the command in a single pane, tmux respawns the whole
// window, which is what an agent wants when a window-level workflow
// (e.g. a build watcher that owned the only pane in its window) has
// exited and needs to come back without disturbing the surrounding
// session. The two tools share the same `kill` semantics and the same
// CodePaneActive (-32005) recovery contract so a client written
// against respawn_pane can branch on the busy-target case the same way
// for respawn_window.
var respawnWindowToolDefs = []map[string]any{
	{
		"name": "respawn_window",
		"description": "Restart the command in a tmux window via `tmux respawn-window [-k] " +
			"[-c <cwd>] -t <session>:<window> [command]`. Window-scoped sibling of " +
			"respawn_pane: where respawn_pane re-runs a single pane's command, respawn_window " +
			"re-runs the whole window — useful when a window-level workflow (a build watcher " +
			"that owned its window's only pane, a REPL inside a single-pane window) has " +
			"exited and the agent wants to bring it back without recreating the window and " +
			"reshuffling the session layout. When `command` is omitted tmux re-runs whatever " +
			"the window was created (or last respawned) with. When `command` is set it is " +
			"forwarded as the trailing argv to `tmux respawn-window` and run via `/bin/sh -c`. " +
			"`cwd` (optional, absolute path) maps to tmux's `-c <start-directory>` flag. " +
			"`kill` (default false) maps to `-k`: when true tmux SIGKILLs the existing process " +
			"before starting the new one; when false tmux refuses the call if the window is " +
			"still active and the boundary returns CodePaneActive (-32005) — the same code " +
			"respawn_pane uses, so the recovery contract is uniform across both tools. " +
			"Returns `{\"respawned\": true}` on success.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{
					"type":        "string",
					"description": "Session id; len 1-64, [A-Za-z0-9_-].",
				},
				"window": map[string]any{
					"type":        "string",
					"description": "Window name (1-64, [A-Za-z0-9_-]) or numeric index (\\d+).",
				},
				"command": map[string]any{
					"type":        "string",
					"description": "Optional command to run in the respawned window; defaults to the original.",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional starting directory (absolute path); maps to tmux -c.",
				},
				"kill": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, kill the running process before respawning (tmux -k).",
				},
			},
			"required":             []string{"session", "window"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register respawn_window onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, respawnWindowToolDefs...)
}

// respawnWindow drives tmuxctl.Controller.RespawnWindow. The handler
// applies the same up-front validation as the rest of the window tool
// surface so callers get a CodeInvalidParams response before any tmux
// command runs.
//
// Validation:
//   - session: required, regex/length policy via validateSessionRef.
//   - window:  required, name (1-64, [A-Za-z0-9_-]) or numeric index.
//   - command: optional; bounded length, no newlines (tmux respawn-window
//     would treat them as argv separators on /bin/sh -c, breaking the
//     "single command" contract). Reuses respawn_pane's maxRespawnCommandLen
//     so both respawn tools share the same policy.
//   - cwd:     optional; rejects relative paths via validateCwd so a
//     missing-prefix typo cannot accidentally land tmux in the server's
//     own working directory.
//   - kill:    optional, defaults to false.
//
// A window that is still running its original command (when kill=false)
// surfaces as errs.ErrPaneActive → CodePaneActive (-32005) — the same
// typed code respawn_pane uses, so a client handling the busy-target
// case once works for both pane- and window-scope respawns.
func (t *Tools) respawnWindow(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session string `json:"session"`
		Window  string `json:"window"`
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
		Kill    bool   `json:"kill"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("respawn_window: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	if rerr := validateWindowTarget(args.Window); rerr != nil {
		return nil, invalidParams("respawn_window: %s", rerr.Message)
	}
	if rerr := validateCwd(args.Cwd); rerr != nil {
		return nil, rerr
	}
	if args.Command != "" {
		if strings.ContainsAny(args.Command, "\n\r") {
			return nil, invalidParams("command: must not contain newlines")
		}
		if len(args.Command) > maxRespawnCommandLen {
			return nil, invalidParams(
				"respawn_window: command length %d exceeds %d",
				len(args.Command), maxRespawnCommandLen,
			)
		}
	}
	if err := t.Ctl.RespawnWindow(
		ctx,
		t.resolveSessionRef(args.Session),
		args.Window,
		args.Command,
		args.Cwd,
		args.Kill,
	); err != nil {
		return nil, internalError(fmt.Errorf("respawn_window: %w", err))
	}
	return jsonBlock(map[string]any{"respawned": true})
}
