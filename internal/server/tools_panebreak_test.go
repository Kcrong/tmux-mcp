package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_PaneBreak_DetachesIntoNewWindow drives the happy path
// end-to-end: session_create → pane_split (vertical, detach) → pane_break
// against the second pane → assert the response carries a stable tmux
// `#{window_id}` and that list_windows now reports two windows on the
// session.
func TestHandle_PaneBreak_DetachesIntoNewWindow(t *testing.T) {
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
		"name": "pb", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "pb"}}))
	})

	// Two panes side-by-side. detach=true keeps focus deterministic.
	_ = extractText(t, call("pane_split", map[string]any{
		"session":   "pb",
		"direction": "vertical",
		"detach":    true,
	}))

	// Sanity: the session has exactly one window with two panes before
	// the break.
	winsBefore, err := tools.Ctl.ListWindows(ctx, "pb")
	if err != nil {
		t.Fatalf("ListWindows pre-break: %v", err)
	}
	if len(winsBefore) != 1 {
		t.Fatalf("ListWindows pre-break = %d, want 1", len(winsBefore))
	}

	out := call("pane_break", map[string]any{"target": "pb:0.1"})
	body := extractText(t, out)
	var resp struct {
		Window string `json:"window"`
	}
	if jerr := json.Unmarshal([]byte(body), &resp); jerr != nil {
		t.Fatalf("decode pane_break: %v\nbody=%s", jerr, body)
	}
	if !strings.HasPrefix(resp.Window, "@") {
		t.Fatalf("pane_break window = %q, want a tmux window id starting with '@'", resp.Window)
	}

	// After break-pane the session should have two windows: the original
	// (now back to one pane) and the new home of the broken-off pane.
	winsAfter, err := tools.Ctl.ListWindows(ctx, "pb")
	if err != nil {
		t.Fatalf("ListWindows post-break: %v", err)
	}
	if len(winsAfter) != 2 {
		t.Fatalf("ListWindows post-break = %d, want 2", len(winsAfter))
	}

	// The returned window id must address an actual pane.
	brokenPanes, err := tools.Ctl.ListPanes(ctx, resp.Window)
	if err != nil {
		t.Fatalf("ListPanes %s: %v", resp.Window, err)
	}
	if len(brokenPanes) != 1 {
		t.Fatalf("broken-off window pane count = %d, want 1", len(brokenPanes))
	}
}

// TestHandle_PaneBreak_RejectsMissingTarget pins the required-field
// path: omitting `target` must come back as CodeInvalidParams rather
// than falling through to tmux with an empty -s value (which tmux
// would resolve to whatever pane it considers current).
func TestHandle_PaneBreak_RejectsMissingTarget(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "pane_break",
		"arguments": map[string]any{},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneBreak_RejectsBadTarget locks the regex check on
// `target` so a stray quote / shell metachar can't slip through to the
// tmux argv, even though the boundary already guards `session` fields
// elsewhere.
func TestHandle_PaneBreak_RejectsBadTarget(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pane_break",
		"arguments": map[string]any{
			"target": "demo:0.0;rm -rf /",
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

// TestHandle_PaneBreak_MissingSessionMapsCode pins the wire contract
// that pane_break against an unknown session surfaces
// CodeSessionNotFound (-32000), mirroring pane_swap / pane_kill /
// pane_resize.
func TestHandle_PaneBreak_MissingSessionMapsCode(t *testing.T) {
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
		"name": "pane_break",
		"arguments": map[string]any{
			"target": "definitely_does_not_exist_xyzzy:0.0",
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

// TestHandle_ToolsList_IncludesPaneBreak makes sure tools/list
// advertises the new tool so MCP clients can discover its schema.
func TestHandle_ToolsList_IncludesPaneBreak(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "pane_break" {
			return
		}
	}
	t.Fatalf("tools/list missing pane_break")
}
