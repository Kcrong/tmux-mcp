package server

import (
	"context"
	"encoding/json"
)

// rotateWindowToolDefs holds the JSON Schema for the rotate_window tool.
// It is appended onto the main toolDefs slice from this file's init()
// so the registration site stays close to the handler — the dispatcher
// in tools.go only needs the single name → handler entry.
//
// rotate_window is the cycle-the-panes-within-the-current-layout
// counterpart to a future next_layout / previous_layout pair: it keeps
// the layout *shape* fixed (still even-horizontal, still tiled, still
// whatever the user picked) and only rotates which pane occupies which
// slot. swap_window — the closest existing sibling — moves *windows*
// between the session's index slots, while rotate_window leaves the
// window alone and moves panes within it.
var rotateWindowToolDefs = []map[string]any{
	{
		"name": "rotate_window",
		"description": "Cycle the panes inside a window through the existing layout slots via " +
			"`tmux rotate-window [-U|-D] -t <target>`. tmux leaves the layout shape (even-horizontal, " +
			"main-vertical, tiled, …) intact and rotates only which pane occupies which slot, so a " +
			"three-pane row A B C becomes B C A under the default `-U` and C A B under `-D`. " +
			"Distinct from a future next_layout / previous_layout pair, which switches between the " +
			"preset layouts: rotate_window keeps the current layout in place. `target` is a tmux " +
			"window target — bare session name (\"demo\") rotates the active window of that session, " +
			"`<session>:<window>` (\"demo:0\") pins a specific window. `downward` (default false) " +
			"selects tmux's `-D` flag — the inverse rotation direction.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type": "string",
					"description": "Window target. Bare session name (`demo`) or `<session>:<window>` " +
						"(`demo:0`); session 1-64 chars, [A-Za-z0-9_-]; window name (1-64, [A-Za-z0-9_-]) " +
						"or numeric index (\\d+).",
				},
				"downward": map[string]any{
					"type":        "boolean",
					"default":     false,
					"description": "When true, rotate the other way (tmux's `-D`). False (the default) emits the tmux-default `-U`.",
				},
			},
			"required":             []string{"target"},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register rotate_window onto the main toolDefs slice. Doing this
	// from init() keeps the new tool surface entirely contained in this
	// file (apart from the single dispatcher case in tools.go) and
	// avoids touching the shared toolDefs literal that other PRs are
	// editing.
	toolDefs = append(toolDefs, rotateWindowToolDefs...)
}

// rotateWindow drives tmuxctl.Controller.RotateWindow. The handler
// validates the target up front so a malformed reference fails fast
// with -32602 before any tmux command runs; the same `<session>` /
// `<session>:<window>` shapes the rest of the window-target surface
// supports are accepted here, with the session half routed through the
// standard regex/length policy.
//
// downward is parsed as a *bool so the schema's documented default of
// false is applied identically whether the field was missing, null, or
// explicitly false. The handler returns a small JSON ack
// `{"rotated": true}` on success — tmux's rotate-window itself produces
// no useful stdout, and a chained list_panes is one call away if the
// caller wants to confirm the new slot order.
//
// A missing session/window surfaces as CodeSessionNotFound (-32000)
// via internalError → errs.CodeOf, mirroring swap_window /
// window_select / window_move.
func (t *Tools) rotateWindow(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Target   string `json:"target"`
		Downward *bool  `json:"downward"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("rotate_window: %v", err)
	}
	if rerr := validateRotateWindowTarget(args.Target); rerr != nil {
		return nil, rerr
	}
	downward := false
	if args.Downward != nil {
		downward = *args.Downward
	}
	if err := t.Ctl.RotateWindow(ctx, t.resolveRotateWindowTarget(args.Target), downward); err != nil {
		return nil, internalError(err)
	}
	return jsonBlock(map[string]any{"rotated": true})
}

// validateRotateWindowTarget enforces the policy on rotate_window's
// `target` argument. Both "session" (bare) and "session:window" forms
// are accepted; the session half always goes through the standard
// regex/length policy, and the window half — when present — picks up
// the same window-target rules window_kill / window_select use. Empty
// is rejected up front because the schema marks the field required and
// tmux would otherwise fall back to "the current window of the current
// client", which is almost never what the agent meant.
func validateRotateWindowTarget(target string) *rpcError {
	if target == "" {
		return invalidParams("target required")
	}
	// Bare session form: no `:`, treat the whole string as a session
	// reference. Defer to validateSessionRef so the error message is
	// consistent with every other session-bearing tool.
	idx := indexColon(target)
	if idx < 0 {
		if rerr := validateSessionRef(target); rerr != nil {
			return invalidParams("target: %s", rerr.Message)
		}
		return nil
	}
	session := target[:idx]
	window := target[idx+1:]
	if rerr := validateSessionRef(session); rerr != nil {
		return invalidParams("target session: %s", rerr.Message)
	}
	// rotate-window's -t accepts the session-only form, so an empty
	// window half ("demo:") is also valid — tmux interprets it as "the
	// active window of <session>", same as the bare form. Only validate
	// the window portion when it is non-empty so we don't trip on that
	// shape.
	if window != "" {
		if rerr := validateWindowTarget(window); rerr != nil {
			return invalidParams("target window: %s", rerr.Message)
		}
	}
	return nil
}

// indexColon is a tiny wrapper around strings.IndexByte for the colon
// separator. Defined as a function (rather than open-coded everywhere)
// so the rotate_window validator can stay readable and so a future
// switch to a more elaborate target parser only needs one edit.
func indexColon(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

// resolveRotateWindowTarget rewrites a rotate_window target string so
// the session component picks up the configured -session-prefix. Both
// "session" and "session:window" shapes are handled; for the qualified
// form we prefix only the session half and leave the window part
// untouched. Empty input is returned unchanged so the validator can
// emit "target required" without seeing a stray prefix.
func (t *Tools) resolveRotateWindowTarget(target string) string {
	if t == nil || t.SessionPrefix == "" || target == "" {
		return target
	}
	idx := indexColon(target)
	if idx < 0 {
		return t.SessionPrefix + target
	}
	return t.SessionPrefix + target[:idx] + target[idx:]
}
