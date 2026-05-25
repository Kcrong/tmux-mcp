package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// copyModeToolDefs holds the JSON Schema for the copy_mode tool. It is
// appended onto the main toolDefs slice from this file's init() so the
// registration site stays close to the handler — the dispatcher in
// tools.go only needs the single name → handler entry.
var copyModeToolDefs = []map[string]any{
	{
		"name": "copy_mode",
		"description": "Enter (or leave) tmux's copy-mode in a target pane via " +
			"`tmux copy-mode [-Hu] [-q] [-M] [-s SRC_PANE] [-t TARGET_PANE]`. " +
			"copy-mode puts the pane into scrollback / selection mode so a " +
			"follow-up send_keys can drive copy-mode key bindings (cursor " +
			"motion, search, copy-selection, …); `exit=true` returns the pane " +
			"to its normal state instead. `target` is required and uses any " +
			"tmux pane-target form (\"session\", \"session:window\", " +
			"\"session:window.pane\", or \"%N\"). `src_pane` is optional — " +
			"when set tmux clones that pane's scrollback into the target " +
			"before entering copy-mode (-s), the same shape as `Prefix + ;` " +
			"interactively. The boolean knobs map one-for-one onto tmux " +
			"flags: `scroll_down=true` anchors the cursor at the bottom (-u), " +
			"`mouse=true` starts in mouse-drag selection (-M), " +
			"`drag_mode=true` enters HALFLINE drag-mode (-H, the equivalent " +
			"of pressing `H` after entering copy-mode interactively).",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":        "string",
					"description": "Pane target (\"session\", \"session:window\", \"session:window.pane\", or \"%N\").",
				},
				"src_pane": map[string]any{
					"type":        "string",
					"description": "Optional source pane whose scrollback is cloned into the target (-s SRC_PANE).",
				},
				"exit": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, quit copy-mode immediately if the target is in it (-q).",
				},
				"scroll_down": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, anchor the cursor at the bottom of the visible region (-u).",
				},
				"mouse": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, start copy-mode in mouse-drag selection (-M).",
				},
				"drag_mode": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, enter HALFLINE drag-mode (-H), the same state as pressing `H` interactively.",
				},
			},
			"required":             []string{"target"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register copy_mode onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, copyModeToolDefs...)
}

// copyMode drives tmuxctl.Controller.CopyMode. `target` must be a
// non-empty pane target that passes the same conservative regex applied
// everywhere else on the boundary — the controller refuses stray
// quoting / shell metachars before any tmux command runs. `src_pane` is
// optional and shares the same regex when supplied. The boolean knobs
// (exit/scroll_down/mouse/drag_mode) are optional and default to false;
// each is parsed as a *bool so the schema's documented default is
// applied identically whether the field was missing, null, or
// explicitly false. A missing session/pane surfaces as
// CodeSessionNotFound (-32000) via internalError → errs.CodeOf,
// mirroring move_pane / pane_swap / pane_break.
func (t *Tools) copyMode(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target     string `json:"target"`
		SrcPane    string `json:"src_pane"`
		Exit       *bool  `json:"exit"`
		ScrollDown *bool  `json:"scroll_down"`
		Mouse      *bool  `json:"mouse"`
		DragMode   *bool  `json:"drag_mode"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("copy_mode: %v", err)
	}
	if args.Target == "" {
		return nil, invalidParams("copy_mode: target required")
	}
	if rerr := validatePaneTarget(args.Target); rerr != nil {
		return nil, invalidParams("copy_mode: target: %s", rerr.Message)
	}
	if rerr := validatePaneTarget(args.SrcPane); rerr != nil {
		return nil, invalidParams("copy_mode: src_pane: %s", rerr.Message)
	}
	exit := false
	if args.Exit != nil {
		exit = *args.Exit
	}
	scrollDown := false
	if args.ScrollDown != nil {
		scrollDown = *args.ScrollDown
	}
	mouse := false
	if args.Mouse != nil {
		mouse = *args.Mouse
	}
	dragMode := false
	if args.DragMode != nil {
		dragMode = *args.DragMode
	}
	if err := t.Ctl.CopyMode(ctx,
		t.resolvePaneTarget(args.Target),
		t.resolvePaneTarget(args.SrcPane),
		exit, scrollDown, mouse, dragMode,
	); err != nil {
		return nil, internalError(fmt.Errorf("copy_mode: %w", err))
	}
	// Echo the logical (un-prefixed) target/src the caller passed so a
	// -session-prefix deployment never leaks the prefixed identity. The
	// JSON ack mirrors the move_pane shape — a small envelope tells
	// callers the call landed without forcing them to re-inspect.
	out := map[string]any{
		"ok":     true,
		"target": args.Target,
		"exit":   exit,
	}
	if args.SrcPane != "" {
		out["src_pane"] = args.SrcPane
	}
	return jsonBlock(out)
}
