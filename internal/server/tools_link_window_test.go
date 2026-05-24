package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_LinkWindow_SharesAcrossSessions runs the happy path: a
// window living in one session is linked into another, and follow-up
// list_windows on each side reflects the share. The response carries
// the dst handle the caller can hand back into list_windows /
// send_keys without having to round-trip through tools/list.
func TestHandle_LinkWindow_SharesAcrossSessions(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lksrc", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "lksrc"}}))
	})
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lkdst", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "lkdst"}}))
	})
	// Detached create so the active flag stays put — link_window cares
	// about layout, not focus.
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "lksrc", "name": "shared", "command": "/bin/sh", "select": false,
	})

	got := extractText(t, callTool(t, tools, ctx, "link_window", map[string]any{
		"src_session": "lksrc", "src_window": "shared",
		"dst_session": "lkdst", "dst_window": "1",
	}))
	if !strings.Contains(got, `"linked":true`) {
		t.Fatalf("link_window text = %q, want it to contain 'linked:true'", got)
	}
	if !strings.Contains(got, `"dst":"lkdst:1"`) {
		t.Fatalf("link_window text = %q, want it to echo dst lkdst:1", got)
	}

	// Source side keeps the original window; dst now sees it too.
	srcMap := listWindowNames(t, extractText(t, callTool(t, tools, ctx,
		"list_windows", map[string]any{"session": "lksrc"})))
	var srcHasShared bool
	for _, name := range srcMap {
		if name == "shared" {
			srcHasShared = true
		}
	}
	if !srcHasShared {
		t.Errorf("src lost the linked window: %+v", srcMap)
	}
	dstMap := listWindowNames(t, extractText(t, callTool(t, tools, ctx,
		"list_windows", map[string]any{"session": "lkdst"})))
	if dstMap[1] != "shared" {
		t.Errorf("dst index 1 = %q, want %q (full=%v)", dstMap[1], "shared", dstMap)
	}
}

// TestHandle_LinkWindow_KillOverwritesExistingDst pins the kill=true
// branch end-to-end through the dispatcher: when the dst slot is
// occupied, kill=true must replace it without a -32603 error. We
// don't try to inspect tmux's exact error phrasing for the kill=false
// path here — that is covered by the controller-level test — because
// the boundary's job is just to plumb the flag.
func TestHandle_LinkWindow_KillOverwritesExistingDst(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lkksrc", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "lkksrc"}}))
	})
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lkkdst", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "lkkdst"}}))
	})
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "lkksrc", "name": "live", "command": "/bin/sh", "select": false,
	})
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "lkkdst", "name": "stale", "command": "/bin/sh", "select": false,
	})

	got := extractText(t, callTool(t, tools, ctx, "link_window", map[string]any{
		"src_session": "lkksrc", "src_window": "live",
		"dst_session": "lkkdst", "dst_window": "1",
		"kill": true,
	}))
	if !strings.Contains(got, `"linked":true`) {
		t.Fatalf("link_window text = %q, want it to contain 'linked:true'", got)
	}

	dstMap := listWindowNames(t, extractText(t, callTool(t, tools, ctx,
		"list_windows", map[string]any{"session": "lkkdst"})))
	if dstMap[1] != "live" {
		t.Errorf("dst index 1 = %q, want %q (full=%v)", dstMap[1], "live", dstMap)
	}
	for _, name := range dstMap {
		if name == "stale" {
			t.Errorf("stale window survived a kill=true link: %v", dstMap)
		}
	}
}

// TestHandle_LinkWindow_RejectsSameSrcDst pins the up-front "src and
// dst must differ" guard. Letting tmux be the one to refuse a self-link
// would emit a less informative error than the boundary's own
// CodeInvalidParams response.
func TestHandle_LinkWindow_RejectsSameSrcDst(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "link_window",
		"arguments": map[string]any{
			"src_session": "demo", "src_window": "0",
			"dst_session": "demo", "dst_window": "0",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for src==dst")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "must differ") {
		t.Errorf("error message %q should reference the difference requirement", rerr.Message)
	}
}

// TestHandle_LinkWindow_RejectsEmptyArgs locks the up-front empty-
// string guards so the dispatcher never builds a partial tmux target.
// All four required fields are exercised because each one feeds a
// different validator path (session vs window) — a regression in any
// one of them would silently let a malformed call reach tmux.
func TestHandle_LinkWindow_RejectsEmptyArgs(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []struct {
		name string
		args map[string]any
	}{
		{
			"empty src_session",
			map[string]any{"src_session": "", "src_window": "0", "dst_session": "demo", "dst_window": "1"},
		},
		{
			"empty src_window",
			map[string]any{"src_session": "demo", "src_window": "", "dst_session": "other", "dst_window": "1"},
		},
		{
			"empty dst_session",
			map[string]any{"src_session": "demo", "src_window": "0", "dst_session": "", "dst_window": "1"},
		},
		{
			"empty dst_window",
			map[string]any{"src_session": "demo", "src_window": "0", "dst_session": "other", "dst_window": ""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "link_window", "arguments": tc.args,
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params for %s", tc.name)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_LinkWindow_RejectsBadTargets covers the regex check for
// every name field — a stray quote / shell metachar must not slip
// through to the tmux argv. Both halves of each (session, window) pair
// are exercised independently so a regression in any one validator is
// pinpointed in the failure message.
func TestHandle_LinkWindow_RejectsBadTargets(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []struct {
		name string
		args map[string]any
	}{
		{
			"bad src_session",
			map[string]any{"src_session": "bad name", "src_window": "0", "dst_session": "demo", "dst_window": "1"},
		},
		{
			"bad src_window",
			map[string]any{"src_session": "demo", "src_window": "0; rm -rf /", "dst_session": "other", "dst_window": "1"},
		},
		{
			"bad dst_session",
			map[string]any{"src_session": "demo", "src_window": "0", "dst_session": "bad:name", "dst_window": "1"},
		},
		{
			"bad dst_window",
			map[string]any{"src_session": "demo", "src_window": "0", "dst_session": "other", "dst_window": "bad name"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "link_window", "arguments": tc.args,
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params for %s", tc.name)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_LinkWindow_RejectsUnknownField pins the schema's
// additionalProperties:false at runtime: a typo like "src_win" must
// fail fast with -32602 instead of being silently dropped (which
// would let a partial target reach tmux). The handler enforces this
// via json.Decoder.DisallowUnknownFields.
func TestHandle_LinkWindow_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "link_window",
		"arguments": map[string]any{
			"src_session": "demo", "src_window": "0",
			"dst_session": "other", "dst_window": "1",
			"src_win": "typo",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for unknown field")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_LinkWindow_MissingSessionMapsCode pins the wire contract:
// link_window against an unknown session must surface
// CodeSessionNotFound (-32000), mirroring swap_window /
// window_select / window_move.
func TestHandle_LinkWindow_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor a real session so the dispatcher hits the "server up,
	// session missing" branch.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lkanchor", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "lkanchor"}}))
	})

	params := mustJSON(t, map[string]any{
		"name": "link_window",
		"arguments": map[string]any{
			"src_session": "definitely_no_such_lk_session",
			"src_window":  "0",
			"dst_session": "lkanchor",
			"dst_window":  "1",
		},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d), msg=%q",
			rerr.Code, errs.CodeSessionNotFound, rerr.Message)
	}
}

// TestHandle_ToolsList_IncludesLinkWindow makes sure the dispatch
// surface advertises the new tool so MCP clients can discover it via
// tools/list — including the strict additionalProperties contract every
// other window tool upholds. Pins the four required fields too so a
// future contributor cannot silently drop one (which would weaken the
// schema's "fast-fail on a missing target" contract).
func TestHandle_ToolsList_IncludesLinkWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		name, _ := def["name"].(string)
		if name != "link_window" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		// additionalProperties:false is part of the contract — an agent
		// that misnames a field gets a fast schema-shaped rejection
		// rather than a silent no-op.
		if got, ok := schema["additionalProperties"].(bool); !ok || got {
			t.Errorf("schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		req, _ := schema["required"].([]string)
		if len(req) != 4 {
			t.Errorf("required = %v, want [src_session src_window dst_session dst_window]", req)
		}
		return
	}
	t.Fatalf("tools/list missing 'link_window'")
}
