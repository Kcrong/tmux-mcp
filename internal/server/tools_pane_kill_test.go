package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_PaneKill_RemovesSplitPane drives the happy path end-to-end:
// session_create → pane_split → pane_kill → list_panes must report a
// single pane again. Verifies the dispatcher is wired up, the schema
// accepts the documented arguments, and the response envelope carries
// the `{"killed": true}` ack.
func TestHandle_PaneKill_RemovesSplitPane(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
		"name": "pk", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "pk"}}))
	})

	splitText := extractText(t, call("pane_split", map[string]any{
		"session":   "pk",
		"direction": "vertical",
		"detach":    true,
	}))
	var splitObj struct {
		ID    string `json:"id"`
		Index int    `json:"index"`
	}
	if err := json.Unmarshal([]byte(splitText), &splitObj); err != nil {
		t.Fatalf("decode pane_split: %v\nbody=%s", err, splitText)
	}

	killText := extractText(t, call("pane_kill", map[string]any{
		"target_pane": splitObj.ID,
	}))
	var killObj struct {
		Killed bool `json:"killed"`
	}
	if err := json.Unmarshal([]byte(killText), &killObj); err != nil {
		t.Fatalf("decode pane_kill: %v\nbody=%s", err, killText)
	}
	if !killObj.Killed {
		t.Fatalf("pane_kill killed flag = false, want true; body=%s", killText)
	}

	listText := extractText(t, call("list_panes", map[string]any{"session": "pk"}))
	var listObj struct {
		Panes []map[string]any `json:"panes"`
	}
	if err := json.Unmarshal([]byte(listText), &listObj); err != nil {
		t.Fatalf("decode list_panes: %v\nbody=%s", err, listText)
	}
	if len(listObj.Panes) != 1 {
		t.Fatalf("list_panes after kill = %d, want 1 (body=%s)", len(listObj.Panes), listText)
	}
}

// TestHandle_PaneKill_RejectsMissingTarget pins the required-field
// path: omitting `target_pane` must come back as CodeInvalidParams
// rather than falling through to tmux with an empty -t value (which
// tmux would resolve to whatever pane it considers current).
func TestHandle_PaneKill_RejectsMissingTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "pane_kill",
		"arguments": map[string]any{},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing target_pane")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneKill_RejectsBadTargetPane locks the regex check on
// `target_pane` so a stray quote/whitespace can't slip through to the
// tmux argv.
func TestHandle_PaneKill_RejectsBadTargetPane(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pane_kill",
		"arguments": map[string]any{
			"target_pane": "demo:0.0;rm -rf /",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad target_pane")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneKill_RejectsBadSession guards the regex/length policy
// for the optional `session` field — when supplied, it must satisfy
// the same rules every other tool enforces.
func TestHandle_PaneKill_RejectsBadSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pane_kill",
		"arguments": map[string]any{
			"session":     "bad name with spaces",
			"target_pane": "demo:0.0",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad session name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneKill_MissingSessionMapsCode pins the wire contract
// that pane_kill against a target on an unknown session surfaces
// CodeSessionNotFound (-32000), mirroring pane_select / pane_split.
func TestHandle_PaneKill_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

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
		"name": "pane_kill",
		"arguments": map[string]any{
			"target_pane": "definitely_does_not_exist_xyzzy:0.0",
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

// TestHandle_ToolsList_IncludesPaneKill makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint.
func TestHandle_ToolsList_IncludesPaneKill(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "pane_kill" {
			return
		}
	}
	t.Fatalf("tools/list missing pane_kill")
}
