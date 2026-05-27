package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// seedSessionWithSplits creates a session, splits its first window
// twice (so the layout has three panes the presets can reshape), and
// registers a session_kill cleanup. Returns the "session:window"
// target subsequent select_layout calls hand back to tmux. Centralised
// here so each select_layout test stays focused on its specific
// assertion rather than re-implementing the multi-pane prologue.
func seedSessionWithSplits(t *testing.T, tools *Tools, ctx context.Context, name string) string {
	t.Helper()
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": name, "command": "/bin/sh", "width": 120, "height": 40,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": name}}))
	})
	// Two splits → three panes. tmux refuses most preset layouts on a
	// single-pane window because the dump shape doesn't change, so the
	// splits are load-bearing for the assertions.
	callTool(t, tools, ctx, "pane_split", map[string]any{
		"session": name, "direction": "vertical", "detach": true,
	})
	callTool(t, tools, ctx, "pane_split", map[string]any{
		"session": name, "direction": "horizontal", "detach": true,
	})
	return name + ":0"
}

// TestHandle_SelectLayout_AppliesPresetReturnsAck drives the happy
// path: applying a preset layout to a freshly-split window must come
// back with the documented `{"selected":true}` JSON ack. Catches the
// dispatcher wiring (case "select_layout":) and the controller's argv
// translation in one shot.
func TestHandle_SelectLayout_AppliesPresetReturnsAck(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	target := seedSessionWithSplits(t, tools, ctx, "slh")

	got := extractText(t, callTool(t, tools, ctx, "select_layout", map[string]any{
		"target": target, "layout": "tiled",
	}))
	if !strings.Contains(got, `"selected":true`) {
		t.Fatalf("select_layout text = %q, want it to contain 'selected:true'", got)
	}
}

// TestHandle_SelectLayout_AcceptsNextFlag pins the optional `next`
// boolean's wiring: cycling onto the next preset must still come back
// with the standard ack. The assertion intentionally does not pin
// which specific preset tmux landed on — the ring order is a tmux
// version detail — only that the flag reached tmux without tripping
// validation.
func TestHandle_SelectLayout_AcceptsNextFlag(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	target := seedSessionWithSplits(t, tools, ctx, "sln")

	// Anchor on a known preset so the ring has a well-defined starting
	// position. Without it, tmux's "last preset used" pointer is empty
	// and -n's behaviour across versions is less stable.
	callTool(t, tools, ctx, "select_layout", map[string]any{
		"target": target, "layout": "tiled",
	})

	got := extractText(t, callTool(t, tools, ctx, "select_layout", map[string]any{
		"target": target, "layout": "tiled", "next": true,
	}))
	if !strings.Contains(got, `"selected":true`) {
		t.Fatalf("select_layout next text = %q, want 'selected:true'", got)
	}
}

// TestHandle_SelectLayout_RejectsNextAndPreviousTogether pins the
// up-front "mutually exclusive" guard. Letting tmux silently pick a
// direction when both flags are sent would surprise an agent that
// confused the two; rejecting here gets a clean -32602 with a
// descriptive message instead.
func TestHandle_SelectLayout_RejectsNextAndPreviousTogether(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "select_layout",
		"arguments": map[string]any{
			"target": "demo:0", "layout": "tiled",
			"next": true, "previous": true,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for next+previous combo")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "mutually exclusive") {
		t.Errorf("error message %q should call out the mutual-exclusion rule", rerr.Message)
	}
}

// TestHandle_SelectLayout_RejectsBadInputs locks the schema-shaped
// up-front guards so the dispatcher never builds a partial tmux argv.
// Each case is a single bad field — that keeps the failure message
// unambiguous when one of the validators regresses.
func TestHandle_SelectLayout_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []struct {
		name string
		args map[string]any
	}{
		{"empty target", map[string]any{"target": "", "layout": "tiled"}},
		{"target without colon", map[string]any{"target": "demo", "layout": "tiled"}},
		{"bad target session", map[string]any{"target": "bad name:0", "layout": "tiled"}},
		{"bad target window", map[string]any{"target": "demo:bad name", "layout": "tiled"}},
		{"empty layout", map[string]any{"target": "demo:0", "layout": ""}},
		{"layout newline", map[string]any{"target": "demo:0", "layout": "tiled\nrm -rf /"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "select_layout", "arguments": tc.args,
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

// TestHandle_SelectLayout_MissingSessionMapsCode pins the wire
// contract: select_layout against an unknown session must surface
// CodeSessionNotFound (-32000), mirroring window_select / swap_window.
func TestHandle_SelectLayout_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor a real session so the dispatcher hits the "server up,
	// session missing" branch — without it, tmux emits "no server
	// running" instead of "can't find pane", which would land on a
	// different code path.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "anchor_sl", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "anchor_sl"}}))
	})

	params := mustJSON(t, map[string]any{
		"name": "select_layout",
		"arguments": map[string]any{
			"target": "definitely_does_not_exist_xyzzy:0",
			"layout": "tiled",
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

// TestHandle_ToolsList_IncludesSelectLayout makes sure the dispatch
// surface advertises the new tool so MCP clients can discover it via
// tools/list — including the strict additionalProperties contract
// every other window-flavoured tool upholds.
func TestHandle_ToolsList_IncludesSelectLayout(t *testing.T) {
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
		if name != "select_layout" {
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
		if len(req) != 2 {
			t.Errorf("required = %v, want [target layout]", req)
		}
		return
	}
	t.Fatalf("tools/list missing 'select_layout'")
}

// TestIsReadOnlyTool_RejectsSelectLayout pins the policy: select_layout
// mutates tmux state (it changes a window's pane geometry) and
// therefore must NOT be inspection-allowed. A regression that
// accidentally added it to readOnlyTools would let a -read-only
// server reshape live windows, which is exactly the surprise the
// flag is meant to prevent.
func TestIsReadOnlyTool_RejectsSelectLayout(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("select_layout") {
		t.Fatal("IsReadOnlyTool(\"select_layout\") = true, want false (mutating tools must not be inspection-allowed)")
	}
}
