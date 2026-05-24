package server

import (
	"context"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_PaneResize_GrowsPaneViaDispatcher drives the happy path
// end-to-end: session_create → pane_split (vertical, detach) → record
// the bottom pane's height via list_panes → pane_resize on that pane
// up by 5 → re-list and assert the height changed. Asserting the
// boundary's "ok" envelope and the post-resize height pins both the
// dispatcher wiring and the handler's argument-passthrough contract.
func TestHandle_PaneResize_GrowsPaneViaDispatcher(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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
		"name": "pr", "command": "/bin/sh", "width": 120, "height": 40,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "pr"}}))
	})

	// Stacked split: the new pane lands underneath the original, so
	// resizing it "up" must grow it.
	_ = extractText(t, call("pane_split", map[string]any{
		"session":   "pr",
		"direction": "vertical",
		"detach":    true,
	}))

	panesBefore, err := tools.Ctl.ListPanes(ctx, "pr")
	if err != nil {
		t.Fatalf("ListPanes pre-resize: %v", err)
	}
	if len(panesBefore) != 2 {
		t.Fatalf("ListPanes pre-resize = %d, want 2", len(panesBefore))
	}
	var beforeID string
	var beforeHeight int
	for _, p := range panesBefore {
		if p.Index == 1 {
			beforeID = p.ID
			beforeHeight = p.Height
		}
	}
	if beforeID == "" {
		t.Fatalf("could not locate bottom pane in %#v", panesBefore)
	}

	out := call("pane_resize", map[string]any{
		"target":    beforeID,
		"direction": "up",
		"amount":    5,
	})
	if got := extractText(t, out); got != "ok" {
		t.Fatalf("pane_resize = %q, want \"ok\"", got)
	}

	panesAfter, err := tools.Ctl.ListPanes(ctx, "pr")
	if err != nil {
		t.Fatalf("ListPanes post-resize: %v", err)
	}
	var afterHeight int
	for _, p := range panesAfter {
		if p.ID == beforeID {
			afterHeight = p.Height
		}
	}
	if afterHeight == 0 {
		t.Fatalf("pane %s vanished after resize: %#v", beforeID, panesAfter)
	}
	if afterHeight <= beforeHeight {
		t.Fatalf("pane_resize up by 5 did not grow pane: before=%d after=%d",
			beforeHeight, afterHeight)
	}
}

// TestHandle_PaneResize_RejectsBadDirection guards the boundary's
// direction whitelist. Anything outside {up, down, left, right} must
// trip CodeInvalidParams (-32602) before tmux is consulted, so a typo
// like "vertical" cannot reach the controller.
func TestHandle_PaneResize_RejectsBadDirection(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pane_resize",
		"arguments": map[string]any{
			"target":    "demo:0.0",
			"direction": "diagonal",
			"amount":    5,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad direction")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneResize_RejectsZeroAmount pins the lower bound on the
// `amount` field. JSON-RPC happily lets the integer through to the
// handler, but a zero step is a no-op tmux silently swallows — almost
// never what the caller meant — so the boundary refuses it explicitly.
func TestHandle_PaneResize_RejectsZeroAmount(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pane_resize",
		"arguments": map[string]any{
			"target":    "demo:0.0",
			"direction": "up",
			"amount":    0,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for zero amount")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneResize_RejectsNegativeAmount mirrors the zero-amount
// case for negative inputs. JSON-RPC `int` happily carries -1, but a
// signed step would drive the bounds check the wrong way and tmux
// would refuse with an unhelpful error if we let it through.
func TestHandle_PaneResize_RejectsNegativeAmount(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pane_resize",
		"arguments": map[string]any{
			"target":    "demo:0.0",
			"direction": "up",
			"amount":    -3,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for negative amount")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneResize_RejectsAmountAbove200 pins the upper bound at
// 200 cells. Anything past that is almost certainly a typo (pixels
// mistaken for cells) and the boundary catches it before any tmux
// call runs.
func TestHandle_PaneResize_RejectsAmountAbove200(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pane_resize",
		"arguments": map[string]any{
			"target":    "demo:0.0",
			"direction": "down",
			"amount":    201,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for amount > 200")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneResize_RejectsMissingTarget locks the required-field
// path: an empty `target` must come back as CodeInvalidParams rather
// than falling through to tmux with an empty -t value (which tmux
// would resolve to whatever pane it considers current).
func TestHandle_PaneResize_RejectsMissingTarget(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pane_resize",
		"arguments": map[string]any{
			"direction": "up",
			"amount":    5,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneResize_RejectsBadTarget locks the regex check on
// `target` so a stray quote / shell metachar can't slip through to
// the tmux argv, even though the boundary already guards `session`
// fields elsewhere.
func TestHandle_PaneResize_RejectsBadTarget(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pane_resize",
		"arguments": map[string]any{
			"target":    "demo:0.0;rm -rf /",
			"direction": "up",
			"amount":    5,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneResize_MissingSessionMapsCode pins the wire contract
// that pane_resize against an unknown session surfaces
// CodeSessionNotFound (-32000), mirroring pane_swap / pane_kill.
func TestHandle_PaneResize_MissingSessionMapsCode(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Anchor with a real session so we exercise "server up, pane missing"
	// rather than "no server" (different stderr shape).
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "anchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name": "pane_resize",
		"arguments": map[string]any{
			"target":    "definitely_does_not_exist_xyzzy:0.0",
			"direction": "up",
			"amount":    5,
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

// TestHandle_ToolsList_IncludesPaneResize makes sure tools/list
// advertises the new tool so MCP clients can discover its schema.
func TestHandle_ToolsList_IncludesPaneResize(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "pane_resize" {
			return
		}
	}
	t.Fatalf("tools/list missing pane_resize")
}
