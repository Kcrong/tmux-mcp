package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// validatePreviousLayoutTarget enforces the per-call check on the
// required `target` field. previous_layout's target identifies the
// window whose preset pointer should step backward, so it must arrive
// in `<session>:<window>` form — the same shape window_move /
// select_layout consume — and both halves must satisfy the standard
// session-name and window-target regexes. Empty session or empty
// window halves are rejected up-front because `tmux previous-layout`
// without a concrete window would target whatever tmux considers
// current, which is rarely what an agent meant.
func validatePreviousLayoutTarget(target string) *rpcError {
	if target == "" {
		return invalidParams("target required")
	}
	idx := strings.Index(target, ":")
	if idx < 0 {
		return invalidParams("target %q must be in `<session>:<window>` form", target)
	}
	session := target[:idx]
	window := target[idx+1:]
	if rerr := validateSessionRef(session); rerr != nil {
		return invalidParams("target session: %s", rerr.Message)
	}
	if rerr := validateWindowTarget(window); rerr != nil {
		return invalidParams("target window: %s", rerr.Message)
	}
	return nil
}

// previousLayoutToolDef holds the JSON Schema for the previous_layout
// tool. It is appended onto the main toolDefs slice via the package
// init() in this file so the registration site stays close to the
// handler — the dispatcher in tools.go only needs the single
// name → handler entry.
//
// previous_layout wraps `tmux previous-layout -t <target>`: it cycles
// the targeted window's pane arrangement one step BACKWARD through
// tmux's preset ring (even-horizontal, even-vertical, main-horizontal,
// main-vertical, tiled), wrapping on the edge. Sibling of next_layout
// — the two are deliberately symmetric so an agent that drives one
// does not need to relearn the schema for the other. Distinct from
// select_layout, which takes a concrete preset name or stored dump;
// previous_layout is the "step backward through presets" affordance
// and intentionally has no payload beyond `target`.
//
// previous_layout MUTATES tmux state (it changes a window's pane
// arrangement) so it is intentionally NOT part of the read-only
// allowlist in readonly.go and gets rejected when the operator runs
// the server with -read-only.
var previousLayoutToolDef = map[string]any{
	"name": "previous_layout",
	"description": "Cycle the targeted window's pane arrangement one step backward through tmux's preset " +
		"ring via `tmux previous-layout -t <target>`. The five presets tmux ships " +
		"(`even-horizontal`, `even-vertical`, `main-horizontal`, `main-vertical`, `tiled`) walk " +
		"in reverse — wrapping from the first preset to the last so the call never refuses on " +
		"an edge. `target` is required and identifies the window in tmux `<session>:<window>` " +
		"form (e.g. `demo:0`). Sibling of `next_layout`; pair with `select_layout` when you want " +
		"to jump to a specific preset or stored layout dump rather than step through the ring.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{
				"type":        "string",
				"description": "Window target in `<session>:<window>` form; session 1-64 `^[A-Za-z0-9_-]+$`, window may be a name (same regex) or numeric index.",
			},
		},
		"required":             []string{"target"},
		"additionalProperties": false,
	},
}

func init() {
	// Register previous_layout onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing in parallel.
	toolDefs = append(toolDefs, previousLayoutToolDef)
}

// previousLayout drives tmuxctl.Controller.PreviousLayout. The handler
// validates the target shape up-front so a malformed reference fails
// with -32602 before any tmux command runs. On success the response
// is a small JSON ack `{"cycled": true}` — tmux's previous-layout
// itself produces no useful stdout, and a follow-up `display_message`
// against `#{window_layout}` is one call away if the caller wants to
// confirm the actual dump that landed.
//
// A missing session/window surfaces as CodeSessionNotFound (-32000)
// via internalError → errs.CodeOf, mirroring window_select /
// swap_window / select_layout.
func (t *Tools) previousLayout(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("previous_layout: %v", err)
	}
	if rerr := validatePreviousLayoutTarget(args.Target); rerr != nil {
		return nil, rerr
	}
	// Apply the configured -session-prefix to the session half of the
	// target so the actual tmux call lands on the prefixed window. The
	// existing window-move helper already handles this exact split
	// (session-only prefixing, leaving window untouched) so reuse it
	// rather than re-implementing the same string surgery here.
	if err := t.Ctl.PreviousLayout(ctx, t.resolveWindowMoveTarget(args.Target)); err != nil {
		return nil, internalError(fmt.Errorf("previous_layout: %w", err))
	}
	return jsonBlock(map[string]any{"cycled": true})
}
