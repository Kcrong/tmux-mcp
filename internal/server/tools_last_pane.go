package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// lastPaneToolDefs holds the JSON Schema for the last_pane tool. It is
// appended onto the main toolDefs slice via this file's init() so the
// registration site stays close to the handler — the dispatcher in
// tools.go only needs the single name → handler entry.
var lastPaneToolDefs = []map[string]any{
	{
		"name": "last_pane",
		"description": "Switch the active pane of a window back to whichever pane was previously " +
			"active via `tmux last-pane`. The `target_window` argument scopes the toggle to a " +
			"specific window (e.g. `demo:0`); when omitted, tmux operates on its idea of the " +
			"current window. `disable_input` (-d) and `enable_input` (-e) are mutually " +
			"exclusive and gate input on the newly-selected pane; `zoom_toggle` (-Z) flips " +
			"the pane's zoom state along with the toggle. Useful for an LLM agent that just " +
			"split a pane, drove the new one, and wants to flip back to the original without " +
			"having to track the pane id.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target_window": map[string]any{
					"type": "string",
					"description": "Optional tmux window target like `mysession:0`; session 1-64, window name " +
						"(1-64, [A-Za-z0-9_-]) or numeric index. Omit to use tmux's current window.",
				},
				"disable_input": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, disable input on the newly-selected pane (`-d`). Mutually exclusive with enable_input.",
				},
				"enable_input": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, re-enable input on the newly-selected pane (`-e`). Mutually exclusive with disable_input.",
				},
				"zoom_toggle": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, also toggle the pane's zoom state (`-Z`).",
				},
			},
			// last_pane's surface is locked to (target_window,
			// disable_input, enable_input, zoom_toggle) today; an
			// unknown field is far more likely a typo than a future
			// capability we forgot to advertise, so reject it up front
			// rather than silently ignore it.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register last_pane onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing. last_pane mutates the active-pane pointer, so it stays
	// off the read-only allowlist in readonly.go alongside
	// pane_select / window_select.
	toolDefs = append(toolDefs, lastPaneToolDefs...)
}

// lastPane drives tmuxctl.Controller.LastPane and returns the standard
// "ok" text-block envelope on success. The (disable_input, enable_input)
// pair is mutually exclusive and surfaces as CodeInvalidParams when both
// are true so a confused caller sees a fast, pointed error rather than
// having tmux silently honour one of the flags.
//
// `target_window`, when supplied, must satisfy the same conservative
// `<session>:<window>` policy window_move uses for src/dst — the
// session half is checked against validateSessionRef and the window
// half against validateWindowTarget, so a typo gets a -32602 rejection
// before tmux is consulted. An empty target_window is permitted (tmux
// then operates on its current window); -session-prefix deployments
// rewrite the session half via resolvePaneTarget so the prefix flows
// through unchanged.
//
// Unknown sessions / windows surface via the wrapped
// errs.ErrSessionNotFound which the dispatcher maps to
// CodeSessionNotFound (-32000), matching every other pane / window
// targeted tool.
func (t *Tools) lastPane(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		TargetWindow string `json:"target_window"`
		DisableInput bool   `json:"disable_input"`
		EnableInput  bool   `json:"enable_input"`
		ZoomToggle   bool   `json:"zoom_toggle"`
	}
	// json.Unmarshal on an empty payload is fine — every field defaults
	// to its zero value, which corresponds to the "no flags, current
	// window" form of `tmux last-pane`. Mirrors choose_tree's null
	// arguments handling.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("last_pane: %v", err)
		}
	}
	if args.DisableInput && args.EnableInput {
		return nil, invalidParams("last_pane: disable_input and enable_input are mutually exclusive")
	}

	// target_window has the shape `<session>:<window>`. Validate each
	// half so a malformed reference fails fast with -32602 before any
	// tmux command runs. An empty target is permitted — tmux falls
	// back to its current window — so we only validate when the
	// caller supplied one.
	resolvedTarget := args.TargetWindow
	if args.TargetWindow != "" {
		idx := strings.Index(args.TargetWindow, ":")
		if idx < 0 {
			return nil, invalidParams(
				"last_pane: target_window %q must be in <session>:<window> form",
				args.TargetWindow,
			)
		}
		session := args.TargetWindow[:idx]
		window := args.TargetWindow[idx+1:]
		if rerr := validateSessionRef(session); rerr != nil {
			return nil, rerr
		}
		if rerr := validateWindowTarget(window); rerr != nil {
			return nil, rerr
		}
		// Apply -session-prefix to the session half so the controller
		// hits the actual prefixed session name. The window half flows
		// through unchanged because pane / window targets never carry
		// the prefix themselves. Keep this before the controller call
		// so the prefix never leaks into the response.
		resolvedTarget = t.resolveSessionRef(session) + ":" + window
	}

	if err := t.Ctl.LastPane(ctx, tmuxctl.LastPaneOptions{
		TargetWindow: resolvedTarget,
		DisableInput: args.DisableInput,
		EnableInput:  args.EnableInput,
		ZoomToggle:   args.ZoomToggle,
	}); err != nil {
		return nil, internalError(fmt.Errorf("last_pane: %w", err))
	}
	return textBlock("ok"), nil
}
