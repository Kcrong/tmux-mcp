package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// maxLayoutValueLen caps the `layout` argument's length. The five
// preset names ("even-horizontal", "even-vertical", "main-horizontal",
// "main-vertical", "tiled") are tiny, but a stored layout dump (e.g.
// the value tmux prints in `#{window_layout}`) can grow with the pane
// count. 4 KiB is well above the longest realistic dump on a normal
// window — anything longer is almost certainly a buggy or hostile
// caller, and bounding here keeps the JSON-RPC frame size predictable.
//
// The five preset names are documented in the schema description and
// in docs/tools.md; we intentionally do not gate on a hardcoded list
// here because tmux's `select-layout` itself merges the preset-name
// and stored-dump cases on the same positional, and the JSON-RPC
// layer should not second-guess that flexibility.
const maxLayoutValueLen = 4096

// validateSelectLayoutTarget is the per-call check on the required
// `target` field. Mirrors validateWindowMoveSrc's split-and-validate
// shape so a malformed reference fails with -32602 before any tmux
// command runs. Both halves of the colon must satisfy the standard
// session-name and window-target regexes — no empty window part is
// permitted (unlike window_move's `dst`) because `select-layout`
// without a concrete window would target whatever tmux considers
// current, which is rarely what an agent meant.
func validateSelectLayoutTarget(target string) *rpcError {
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

// validateSelectLayoutValue enforces the (looser) policy on the
// `layout` argument: it must be non-empty, bounded in length, and free
// of newlines that would let a stored-dump payload smuggle a second
// command into tmux's argv. We deliberately do not gate on the preset
// name list — tmux accepts both presets and opaque dump strings on the
// same positional, and the JSON-RPC layer should not second-guess that
// flexibility. The schema's description still calls out the five
// presets so a typoing agent gets a hint.
func validateSelectLayoutValue(layout string) *rpcError {
	if layout == "" {
		return invalidParams("layout required")
	}
	if len(layout) > maxLayoutValueLen {
		return invalidParams("layout length %d exceeds %d", len(layout), maxLayoutValueLen)
	}
	if strings.ContainsAny(layout, "\r\n") {
		return invalidParams("layout: must not contain newlines")
	}
	return nil
}

// selectLayoutToolDef holds the JSON Schema for the select_layout tool.
// It is appended onto the main toolDefs slice via the package init() in
// this file so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
//
// `select_layout` mutates tmux state (it changes a window's pane
// arrangement), so it is intentionally NOT part of the read-only
// allowlist in readonly.go and gets rejected when the operator runs
// the server with -read-only.
var selectLayoutToolDef = map[string]any{
	"name": "select_layout",
	"description": "Apply a preset or stored pane layout to a window via `tmux select-layout`. " +
		"`target` is required and identifies the window in tmux `<session>:<window>` form " +
		"(e.g. `demo:0`); `layout` is required and accepts either one of the five preset names " +
		"(`even-horizontal`, `even-vertical`, `main-horizontal`, `main-vertical`, `tiled`) or " +
		"a stored layout dump string previously obtained from `#{window_layout}`. Optional " +
		"`next` (-n) and `previous` (-p) cycle through the preset ring; optional `spread` " +
		"(-E) spreads the current pane and its neighbours out evenly after the layout " +
		"selection lands. Pairs with `list_windows` to discover available windows and with " +
		"`pane_split` to populate the panes the layout will reshape.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{
				"type":        "string",
				"description": "Window target in `<session>:<window>` form; session 1-64 `^[A-Za-z0-9_-]+$`, window may be a name (same regex) or numeric index.",
			},
			"layout": map[string]any{
				"type":        "string",
				"description": "Preset name (even-horizontal, even-vertical, main-horizontal, main-vertical, tiled) or a stored layout dump string. Newlines are refused.",
			},
			"next": map[string]any{
				"type":        "boolean",
				"default":     false,
				"description": "When true, cycle to the next preset layout (`-n`).",
			},
			"previous": map[string]any{
				"type":        "boolean",
				"default":     false,
				"description": "When true, cycle to the previous preset layout (`-p`).",
			},
			"spread": map[string]any{
				"type":        "boolean",
				"default":     false,
				"description": "When true, spread the current pane and its neighbours out evenly (`-E`).",
			},
		},
		"required":             []string{"target", "layout"},
		"additionalProperties": false,
	},
}

func init() {
	// Register select_layout onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, selectLayoutToolDef)
}

// selectLayout drives tmuxctl.Controller.SelectLayout. The handler
// validates the target shape, the layout positional (length + newline
// guard), and rejects the obviously contradictory next+previous combo
// before any tmux command runs. On success the response is a small
// JSON ack `{"selected": true}` — tmux's select-layout itself produces
// no useful stdout, and a follow-up `display_message` against
// `#{window_layout}` is one call away if the caller wants to confirm
// the actual dump that landed.
//
// A missing session/window surfaces as CodeSessionNotFound (-32000)
// via internalError → errs.CodeOf, mirroring window_select /
// swap_window.
func (t *Tools) selectLayout(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target   string `json:"target"`
		Layout   string `json:"layout"`
		Next     *bool  `json:"next"`
		Previous *bool  `json:"previous"`
		Spread   *bool  `json:"spread"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("select_layout: %v", err)
	}
	if rerr := validateSelectLayoutTarget(args.Target); rerr != nil {
		return nil, rerr
	}
	if rerr := validateSelectLayoutValue(args.Layout); rerr != nil {
		return nil, rerr
	}
	// *bool so the documented default of false is applied identically
	// whether the field was missing, null, or explicitly false.
	next := args.Next != nil && *args.Next
	previous := args.Previous != nil && *args.Previous
	spread := args.Spread != nil && *args.Spread
	// next + previous are mutually exclusive — passing both would let
	// tmux silently pick one (and the choice is version-sensitive).
	// Refusing here surfaces a clear -32602 instead of a confusing
	// "tmux did the other one" outcome.
	if next && previous {
		return nil, invalidParams("next and previous are mutually exclusive")
	}
	// Apply the configured -session-prefix to the session half of the
	// target so the actual tmux call lands on the prefixed window. The
	// existing window-move helper already handles this exact split
	// (session-only prefixing, leaving window untouched) so reuse it
	// rather than re-implementing the same string surgery here.
	if err := t.Ctl.SelectLayout(ctx,
		t.resolveWindowMoveTarget(args.Target),
		args.Layout,
		tmuxctl.SelectLayoutOpts{Next: next, Previous: previous, Spread: spread},
	); err != nil {
		return nil, internalError(fmt.Errorf("select_layout: %w", err))
	}
	return jsonBlock(map[string]any{"selected": true})
}
