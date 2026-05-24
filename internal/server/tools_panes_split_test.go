package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_PaneSplit_VerticalCreatesSecondPane drives the happy path
// end-to-end: session_create → pane_split (vertical, detach) →
// list_panes must report two panes. Verifies the dispatcher is wired
// up, the schema accepts the documented arguments, and the response
// envelope carries an `id`/`index` pair.
func TestHandle_PaneSplit_VerticalCreatesSecondPane(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
		"name": "psv", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "psv"}}))
	})

	splitText := extractText(t, call("pane_split", map[string]any{
		"session":   "psv",
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
	if !strings.HasPrefix(splitObj.ID, "%") {
		t.Errorf("pane_split id = %q, want a tmux %%N identifier", splitObj.ID)
	}

	listText := extractText(t, call("list_panes", map[string]any{"session": "psv"}))
	var listObj struct {
		Panes []map[string]any `json:"panes"`
	}
	if err := json.Unmarshal([]byte(listText), &listObj); err != nil {
		t.Fatalf("decode list_panes: %v\nbody=%s", err, listText)
	}
	if len(listObj.Panes) != 2 {
		t.Fatalf("list_panes after split = %d, want 2 (body=%s)", len(listObj.Panes), listText)
	}
}

// TestHandle_PaneSplit_HorizontalAcceptedOnly proves the schema enum
// and dispatcher accept the second valid direction, and that the new
// pane shows up in list_panes the same way as the vertical case.
func TestHandle_PaneSplit_HorizontalAcceptedOnly(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
		"name": "psh", "command": "/bin/sh", "width": 100, "height": 30,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "psh"}}))
	})

	_ = extractText(t, call("pane_split", map[string]any{
		"session":   "psh",
		"direction": "horizontal",
		"detach":    true,
	}))

	listText := extractText(t, call("list_panes", map[string]any{"session": "psh"}))
	var listObj struct {
		Panes []map[string]any `json:"panes"`
	}
	if err := json.Unmarshal([]byte(listText), &listObj); err != nil {
		t.Fatalf("decode list_panes: %v\nbody=%s", err, listText)
	}
	if len(listObj.Panes) != 2 {
		t.Fatalf("list_panes after horizontal split = %d, want 2", len(listObj.Panes))
	}
}

// TestHandle_PaneSplit_RejectsBadDirection pins the wire contract that
// a direction outside the {horizontal, vertical} whitelist surfaces as
// CodeInvalidParams (-32602) — a stable code the client can branch on.
func TestHandle_PaneSplit_RejectsBadDirection(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pane_split",
		"arguments": map[string]any{
			"session":   "demo",
			"direction": "diagonal",
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

// TestHandle_PaneSplit_RejectsMissingDirection guards the required-
// field path: omitting `direction` must come back as CodeInvalidParams
// rather than falling through to tmux with an empty axis flag.
func TestHandle_PaneSplit_RejectsMissingDirection(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "pane_split",
		"arguments": map[string]any{"session": "demo"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing direction")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneSplit_RejectsBadSession guards the same regex/length
// policy for `session` that every other tool enforces — the
// dispatcher must not fall through to tmux on a malformed reference.
func TestHandle_PaneSplit_RejectsBadSession(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pane_split",
		"arguments": map[string]any{
			"session":   "bad name with spaces",
			"direction": "vertical",
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

// TestHandle_PaneSplit_RejectsBadTargetPane locks the regex check for
// the optional `target_pane` argument so a stray quote/whitespace
// can't slip through to the tmux argv.
func TestHandle_PaneSplit_RejectsBadTargetPane(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "pane_split",
		"arguments": map[string]any{
			"session":     "demo",
			"target_pane": "demo:0.0;rm -rf /",
			"direction":   "vertical",
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

// TestHandle_PaneSplit_RejectsOversizedCommand guards the command-
// length cap. tmux happily forwards arbitrary strings to /bin/sh -c,
// but we bound the JSON-RPC frame size so a hostile caller can't push
// megabyte-sized commands through the dispatcher.
func TestHandle_PaneSplit_RejectsOversizedCommand(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	big := strings.Repeat("a", maxPaneCommandLen+1)
	params := mustJSON(t, map[string]any{
		"name": "pane_split",
		"arguments": map[string]any{
			"session":   "demo",
			"direction": "vertical",
			"command":   big,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversized command")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_PaneSplit_MissingSessionMapsCode pins the wire contract
// that pane_split against an unknown session surfaces
// CodeSessionNotFound (-32000), mirroring pane_select / window_create.
func TestHandle_PaneSplit_MissingSessionMapsCode(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Anchor so we hit "server up, session missing".
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "anchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name": "pane_split",
		"arguments": map[string]any{
			"session":   "definitely_does_not_exist_xyzzy",
			"direction": "vertical",
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

// TestHandle_ToolsList_IncludesPaneSplit makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint.
func TestHandle_ToolsList_IncludesPaneSplit(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "pane_split" {
			return
		}
	}
	t.Fatalf("tools/list missing pane_split")
}
