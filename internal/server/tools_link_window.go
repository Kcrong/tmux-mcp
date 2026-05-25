package server

import (
	"bytes"
	"context"
	"encoding/json"
)

// linkWindowToolDef is the JSON Schema for the link_window tool.
// Registered onto the main toolDefs slice from this file's init() so the
// schema and handler stay co-located, mirroring the layout
// tools_window.go uses for the rest of the window surface.
var linkWindowToolDef = map[string]any{
	"name": "link_window",
	"description": "Share a window across sessions in place via " +
		"`tmux link-window -s <src_session>:<src_window> -t <dst_session>:<dst_window>`. " +
		"Unlike `window_move` (which relocates and removes the source) and `swap_window` " +
		"(which trades two windows of the same session), link-window leaves the source intact: " +
		"the same `#{window_id}` is reachable from both sessions, so a long-running build " +
		"window can be exposed in a \"monitor\" session without losing the foreground in the " +
		"working session. `src_session` / `dst_session` follow the conservative session-name " +
		"policy (1-64, `^[A-Za-z0-9_-]+$`); `src_window` / `dst_window` may be window names " +
		"(same regex/length policy) or numeric indices (`\\d+`). The src and dst pair must " +
		"differ — passing the same `<session>:<window>` rejects with -32602 before tmux is " +
		"consulted. `kill` (default false) maps to tmux's `-k` flag: when true, an existing " +
		"window already at dst is destroyed before the link; when false, tmux refuses with " +
		"\"index in use\" rather than silently overwriting.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"src_session": map[string]any{
				"type":        "string",
				"description": "Source session name; len 1-64, [A-Za-z0-9_-].",
			},
			"src_window": map[string]any{
				"type":        "string",
				"description": "Source window name (len 1-64, [A-Za-z0-9_-]) or numeric index (\\d+).",
			},
			"dst_session": map[string]any{
				"type":        "string",
				"description": "Destination session name; len 1-64, [A-Za-z0-9_-].",
			},
			"dst_window": map[string]any{
				"type":        "string",
				"description": "Destination window name (len 1-64, [A-Za-z0-9_-]) or numeric index (\\d+).",
			},
			"kill": map[string]any{
				"type":        "boolean",
				"default":     false,
				"description": "When true, overwrite an existing dst window instead of erroring (`-k`).",
			},
		},
		"required":             []string{"src_session", "src_window", "dst_session", "dst_window"},
		"additionalProperties": false,
	},
}

func init() {
	// Register the link_window tool onto the main toolDefs slice from
	// this file's init() so the registration site stays close to the
	// handler. The dispatcher in tools.go only needs the one name →
	// handler entry it grew alongside this init.
	toolDefs = append(toolDefs, linkWindowToolDef)
}

// linkWindow drives tmuxctl.Controller.LinkWindow. Validates both
// session references and both window targets up front so a malformed
// call sees CodeInvalidParams (-32602) before any tmux command runs;
// the (src_session, src_window) and (dst_session, dst_window) pairs
// must also differ — letting tmux be the one to refuse a self-link
// would emit a less informative error than the boundary's own
// "src and dst must differ" message.
//
// kill is parsed as a *bool so the schema's documented default of false
// is applied identically whether the field was missing, null, or
// explicitly false. The handler returns a small JSON ack
// `{"linked": true, "dst": "<dst_session>:<dst_window>"}` on success —
// tmux's link-window itself produces no useful stdout, and chained
// list_windows is one call away if the caller wants to confirm the
// destination layout. The echoed dst uses the logical session name the
// caller passed, so a -session-prefix deployment does not leak the
// prefixed identity back to the wire.
//
// A missing session/window surfaces as CodeSessionNotFound (-32000)
// via internalError → errs.CodeOf, mirroring swap_window /
// window_move / window_rename.
func (t *Tools) linkWindow(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	// Local struct uses *bool for kill so the schema's documented
	// default of false is applied identically whether the field was
	// missing, null, or explicitly false — matching swap_window's
	// no_select pattern.
	var args struct {
		SrcSession string `json:"src_session"`
		SrcWindow  string `json:"src_window"`
		DstSession string `json:"dst_session"`
		DstWindow  string `json:"dst_window"`
		Kill       *bool  `json:"kill"`
	}
	// Reject unknown fields up front so a typo (e.g. "src_win") fails
	// fast with -32602 rather than silently producing a partial target.
	// json.Decoder.DisallowUnknownFields enforces the schema's
	// additionalProperties:false at the boundary — without it the
	// schema declaration is informational only and a misnamed field
	// would silently no-op.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return nil, invalidParams("link_window: %v", err)
	}
	if rerr := validateSessionRef(args.SrcSession); rerr != nil {
		return nil, invalidParams("src_session: %s", rerr.Message)
	}
	if rerr := validateWindowTarget(args.SrcWindow); rerr != nil {
		return nil, invalidParams("src_window: %s", rerr.Message)
	}
	if rerr := validateSessionRef(args.DstSession); rerr != nil {
		return nil, invalidParams("dst_session: %s", rerr.Message)
	}
	if rerr := validateWindowTarget(args.DstWindow); rerr != nil {
		return nil, invalidParams("dst_window: %s", rerr.Message)
	}
	// Same (session, window) pair on both sides is a no-op that tmux
	// would also refuse, but emitting a more specific boundary error
	// keeps the failure mode obvious in agent transcripts. The check
	// runs against the logical names (pre-prefix) because that is what
	// the caller can act on.
	if args.SrcSession == args.DstSession && args.SrcWindow == args.DstWindow {
		return nil, invalidParams("src and dst must differ")
	}
	kill := false
	if args.Kill != nil {
		kill = *args.Kill
	}
	src := t.resolveSessionRef(args.SrcSession) + ":" + args.SrcWindow
	dst := t.resolveSessionRef(args.DstSession) + ":" + args.DstWindow
	if err := t.Ctl.LinkWindow(ctx, src, dst, kill); err != nil {
		return nil, internalError(err)
	}
	// Echo the logical (un-prefixed) dst the caller passed so a
	// -session-prefix deployment never leaks the prefixed identity to
	// the caller. The src is omitted because the caller already knows
	// it — the response is the destination handle they should target
	// next, mirroring window_create's "what you can use now" contract.
	return jsonBlock(map[string]any{
		"linked": true,
		"dst":    args.DstSession + ":" + args.DstWindow,
	})
}
