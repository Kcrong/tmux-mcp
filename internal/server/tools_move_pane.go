package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// movePaneToolDefs holds the JSON Schema for the move_pane tool. It is
// appended onto the main toolDefs slice from this file's init() so the
// registration site stays close to the handler — the dispatcher in
// tools.go only needs the single name → handler entry.
var movePaneToolDefs = []map[string]any{
	{
		"name": "move_pane",
		"description": "Relocate a single pane to a different slot, window, or session via " +
			"`tmux move-pane -s <src> -t <dst>` (with `-h`/`-b`/`-d` selected by the boolean " +
			"knobs). Distinct from `pane_swap` (which trades two panes in place) and " +
			"`pane_break` (which detaches a pane into its own brand-new window): `move_pane` " +
			"takes one source pane and re-homes it next to the destination, splitting the " +
			"destination to make room. The source pane keeps its `#{pane_id}`, contents, and " +
			"running process — only the layout slot changes. `horizontal=true` splits the " +
			"destination left/right (`-h`); the default (false) splits top/bottom. " +
			"`before=true` places the moved pane before the destination in the resulting split " +
			"(`-b`); the default (false) places it after. `no_focus=true` leaves the active " +
			"pane alone after the move (`-d`) so a chained send_keys/capture stays " +
			"deterministic.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"src": map[string]any{
					"type":        "string",
					"description": "Source pane target (e.g. \"session:window.pane\" or \"%5\").",
				},
				"dst": map[string]any{
					"type":        "string",
					"description": "Destination pane target (same target forms as `src`).",
				},
				"horizontal": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, split the destination left/right (-h); default is top/bottom.",
				},
				"before": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, insert the moved pane before the destination (-b); default is after.",
				},
				"no_focus": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, do not change the active pane after the move (-d).",
				},
			},
			"required":             []string{"src", "dst"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register move_pane onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, movePaneToolDefs...)
}

// movePane drives tmuxctl.Controller.MovePane. Both `src` and `dst` must
// be non-empty pane targets that pass the same conservative regex
// applied everywhere else on the boundary — the controller refuses
// stray quoting / shell metachars before any tmux command runs. The
// boolean knobs (horizontal/before/no_focus) are optional and default to
// false; each is parsed as a *bool so the schema's documented default is
// applied identically whether the field was missing, null, or
// explicitly false. A missing session/pane surfaces as
// CodeSessionNotFound (-32000) via internalError → errs.CodeOf,
// mirroring pane_swap / pane_break / pane_join.
func (t *Tools) movePane(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Src        string `json:"src"`
		Dst        string `json:"dst"`
		Horizontal *bool  `json:"horizontal"`
		Before     *bool  `json:"before"`
		NoFocus    *bool  `json:"no_focus"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("move_pane: %v", err)
	}
	if args.Src == "" {
		return nil, invalidParams("move_pane: src required")
	}
	if args.Dst == "" {
		return nil, invalidParams("move_pane: dst required")
	}
	if rerr := validatePaneTarget(args.Src); rerr != nil {
		return nil, invalidParams("move_pane: src: %s", rerr.Message)
	}
	if rerr := validatePaneTarget(args.Dst); rerr != nil {
		return nil, invalidParams("move_pane: dst: %s", rerr.Message)
	}
	horizontal := false
	if args.Horizontal != nil {
		horizontal = *args.Horizontal
	}
	before := false
	if args.Before != nil {
		before = *args.Before
	}
	noFocus := false
	if args.NoFocus != nil {
		noFocus = *args.NoFocus
	}
	if err := t.Ctl.MovePane(ctx,
		t.resolvePaneTarget(args.Src),
		t.resolvePaneTarget(args.Dst),
		horizontal, before, noFocus,
	); err != nil {
		return nil, internalError(fmt.Errorf("move_pane: %w", err))
	}
	// Echo the logical (un-prefixed) src/dst the caller passed so a
	// -session-prefix deployment never leaks the prefixed identity. The
	// JSON ack mirrors the pane_break / window_move shapes — a small
	// envelope tells callers the move actually happened without forcing
	// them to inspect the next list_panes.
	return jsonBlock(map[string]any{
		"moved": true,
		"src":   args.Src,
		"dst":   args.Dst,
	})
}
