package server

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
)

// unlinkWindowToolDef is the JSON Schema for the unlink_window tool.
// Registered onto the main toolDefs slice from this file's init() so
// the schema and handler stay co-located, mirroring the layout
// tools_kill_window.go and tools_link_window.go use for the rest of
// the window surface.
var unlinkWindowToolDef = map[string]any{
	"name": "unlink_window",
	"description": "Remove a window reference from a session via " +
		"`tmux unlink-window -t <session>:<window>`. The inverse of `link_window`: where " +
		"link-window grafts a window's `#{window_id}` into a second session's slot, unlink-window " +
		"detaches the named slot from that session — leaving the window itself alive in any other " +
		"sessions still referencing the same id. `target` is in tmux's standard `<session>:<window>` " +
		"form; the session half satisfies the conservative session-name policy (1-64, " +
		"`^[A-Za-z0-9_-]+$`); the window half may be a window name (same regex/length policy) or a " +
		"numeric index (`\\d+`). `kill` (default false) maps to tmux's `-k` flag: when false, tmux " +
		"refuses to unlink a window whose only reference is the one being removed (because doing so " +
		"would also reap the underlying window itself); when true, the call proceeds even on the last " +
		"reference, which destroys the window. Use `kill=false` to stop sharing into a session " +
		"without destroying the window in the source session, and `kill=true` once no session needs " +
		"the linked window any longer.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{
				"type":        "string",
				"description": "Window reference like `mysession:0`; session 1-64, window name (1-64, [A-Za-z0-9_-]) or numeric index.",
			},
			"kill": map[string]any{
				"type":        "boolean",
				"default":     false,
				"description": "When true, unlink even the last reference (destroys the window) — tmux's `-k` flag.",
			},
		},
		"required":             []string{"target"},
		"additionalProperties": false,
	},
}

func init() {
	// Register the unlink_window tool onto the main toolDefs slice from
	// this file's init() so the registration site stays close to the
	// handler — the dispatcher in tools.go only needs the one name →
	// handler entry it grew alongside this init.
	toolDefs = append(toolDefs, unlinkWindowToolDef)
}

// validateUnlinkWindowTarget enforces the policy on unlink_window's
// `target` argument. Like window_move's src, the target must be a
// complete `<session>:<window>` reference because tmux's `unlink-window
// -t` needs an unambiguous window — leaving the window part empty would
// let tmux pick the "current" window of the session, which is rarely
// what an agent meant. Both halves are run through the same validators
// every other window-target field uses (validateSessionRef /
// validateWindowTarget) so a single typo / shell metachar fails fast
// with -32602 before the boundary asks tmux to do anything.
func validateUnlinkWindowTarget(target string) *rpcError {
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

// unlinkWindow drives tmuxctl.Controller.UnlinkWindow. Validates the
// target up front so a malformed call sees CodeInvalidParams (-32602)
// before any tmux command runs.
//
// kill is parsed as a *bool so the schema's documented default of false
// is applied identically whether the field was missing, null, or
// explicitly false — matching swap_window's no_select pattern. The
// handler returns a small JSON ack `{"unlinked": true}` on success;
// tmux's unlink-window itself produces no useful stdout, and chained
// list_windows is one call away if the caller wants to confirm the
// destination layout.
//
// A missing session/window surfaces as CodeSessionNotFound (-32000)
// via internalError → errs.CodeOf, mirroring swap_window /
// window_move / window_rename. The kill=false / last-reference refusal
// surfaces via the wrapped tmux error (CodeInternal) — the boundary
// does not pre-flight the reference count because the cheap path
// (let tmux refuse) yields the same answer with one fewer round-trip,
// and the alternative would race a concurrent link/unlink anyway.
func (t *Tools) unlinkWindow(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	// Local struct uses *bool for kill so the schema's documented
	// default of false is applied identically whether the field was
	// missing, null, or explicitly false — matching swap_window's
	// no_select / link_window's kill patterns.
	var args struct {
		Target string `json:"target"`
		Kill   *bool  `json:"kill"`
	}
	// Reject unknown fields up front so a typo (e.g. "targets") fails
	// fast with -32602 rather than silently producing a missing-target
	// rejection. json.Decoder.DisallowUnknownFields enforces the
	// schema's additionalProperties:false at the boundary — without it
	// the schema declaration is informational only and a misnamed field
	// would silently no-op.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return nil, invalidParams("unlink_window: %v", err)
	}
	if rerr := validateUnlinkWindowTarget(args.Target); rerr != nil {
		return nil, rerr
	}
	kill := false
	if args.Kill != nil {
		kill = *args.Kill
	}
	// Apply -session-prefix to the session half of the target so we
	// land on the actual prefixed tmux session the rest of the surface
	// addresses. resolveWindowMoveTarget already implements the exact
	// "split on first colon, prefix the session half" rule unlink_window
	// needs, so reuse it rather than open-coding a near-identical helper.
	resolved := t.resolveWindowMoveTarget(args.Target)
	if err := t.Ctl.UnlinkWindow(ctx, resolved, kill); err != nil {
		return nil, internalError(err)
	}
	return jsonBlock(map[string]any{"unlinked": true})
}
