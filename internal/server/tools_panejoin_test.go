package server

import (
	"context"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_PaneJoin_MovesPaneIntoDestinationWindow drives the happy
// path end-to-end through the JSON-RPC dispatcher: session_create →
// window_create (a second window holding a single pane) → pane_join
// the donor pane back into window 0. After the join the destination
// window must hold two panes and the donor window must be gone — tmux
// reaps a window once its last pane has been pulled out, which is the
// observable contract MCP callers depend on.
func TestHandle_PaneJoin_MovesPaneIntoDestinationWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	call := func(name string, args any) any {
		t.Helper()
		params := mustJSON(t, map[string]any{"name": name, "arguments": args})
		res, rerr := tools.Handle(ctx, "tools/call", params)
		if rerr != nil {
			t.Fatalf("%s: %s", name, rerr.Message)
		}
		return res
	}

	call("session_create", map[string]any{
		"name": "pj", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "pj"}}))
	})

	// Add a second window via the window_create boundary tool. The new
	// window starts with a single pane (pj:1.0) — the donor we'll move
	// back into pj:0. select=false keeps focus on window 0 so the join
	// observation is independent of which window tmux considers active.
	call("window_create", map[string]any{
		"session": "pj",
		"select":  false,
	})

	// Move the donor pane (pj:1.0) into window 0. horizontal=false
	// (the default) asserts the top/bottom split path through the
	// dispatcher.
	out := call("pane_join", map[string]any{
		"src": "pj:1.0",
		"dst": "pj:0",
	})
	if got := extractText(t, out); got != "ok" {
		t.Fatalf("pane_join = %q, want \"ok\"", got)
	}

	// Window 0 must now hold two panes; the donor window must be gone
	// (its only pane was moved away, and tmux reaps the empty window).
	panes, err := tools.Ctl.ListPanes(ctx, "pj:0")
	if err != nil {
		t.Fatalf("ListPanes pj:0: %v", err)
	}
	if len(panes) != 2 {
		t.Fatalf("ListPanes pj:0 returned %d panes after join, want 2", len(panes))
	}
	wins, err := tools.Ctl.ListWindows(ctx, "pj")
	if err != nil {
		t.Fatalf("ListWindows pj: %v", err)
	}
	if len(wins) != 1 {
		t.Fatalf("ListWindows pj after join returned %d windows, want 1", len(wins))
	}
}

// TestHandle_PaneJoin_RejectsEmptySrc guards the required-field path:
// the schema lists src as required, but the handler must also reject
// the empty string at runtime so a half-formed call cannot leak a
// stray "" past the regex.
func TestHandle_PaneJoin_RejectsEmptySrc(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "pane_join",
		"arguments": map[string]any{"src": "", "dst": "demo:0"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty src")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneJoin_RejectsEmptyDst mirrors the src guard for the
// destination argument so tmux never sees a "-t" without a value.
func TestHandle_PaneJoin_RejectsEmptyDst(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "pane_join",
		"arguments": map[string]any{"src": "demo:0.0", "dst": ""},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty dst")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneJoin_RejectsBadSrc locks the regex check for the src
// target — a stray quote / shell metachar must not slip through to the
// tmux argv, even though the boundary already guards `session` fields
// elsewhere.
func TestHandle_PaneJoin_RejectsBadSrc(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pane_join",
		"arguments": map[string]any{
			"src": "demo:0.0;rm -rf /",
			"dst": "demo:0",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad src")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneJoin_RejectsBadDst mirrors the bad-src check for the
// destination argument, ensuring the regex applies symmetrically.
func TestHandle_PaneJoin_RejectsBadDst(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pane_join",
		"arguments": map[string]any{
			"src": "demo:0.0",
			"dst": "demo:0;rm -rf /",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad dst")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneJoin_MissingSessionMapsCode pins the wire contract that
// pane_join against an unknown session surfaces CodeSessionNotFound
// (-32000), mirroring pane_swap / pane_split.
func TestHandle_PaneJoin_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor so we hit "server up, session missing" rather than "no
	// server" (different stderr shape).
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "anchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name": "pane_join",
		"arguments": map[string]any{
			"src": "definitely_does_not_exist_xyzzy:0.0",
			"dst": "anchor:0",
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

// TestHandle_ToolsList_IncludesPaneJoin makes sure tools/list advertises
// the new tool so MCP clients can discover it via the schema endpoint.
func TestHandle_ToolsList_IncludesPaneJoin(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "pane_join" {
			return
		}
	}
	t.Fatalf("tools/list missing pane_join")
}
