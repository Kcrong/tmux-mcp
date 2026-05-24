package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_ListPanes_AfterSessionCreate confirms the list_panes tool
// returns the active pane of a freshly created session, with the fields
// we expect downstream agents to switch on.
func TestHandle_ListPanes_AfterSessionCreate(t *testing.T) {
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
		"name": "lp", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "lp"}}))
	})

	body := extractText(t, call("list_panes", map[string]any{"session": "lp"}))
	var obj struct {
		Panes []map[string]any `json:"panes"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_panes: %v\nbody=%s", err, body)
	}
	if len(obj.Panes) == 0 {
		t.Fatalf("expected at least one pane, got %s", body)
	}
	p := obj.Panes[0]
	if id, _ := p["id"].(string); !strings.HasPrefix(id, "%") {
		t.Errorf("pane id = %v, want a tmux %%N identifier", p["id"])
	}
	if sw, _ := p["session_win"].(string); sw != "lp:0" {
		t.Errorf("session_win = %v, want lp:0", p["session_win"])
	}
	if active, _ := p["active"].(bool); !active {
		t.Error("expected the only pane of a fresh session to be active")
	}
}

// TestHandle_ListPanes_NoArgs_ListsAllPanes proves the empty-session
// branch (server-wide -a listing) works through the tool surface.
func TestHandle_ListPanes_NoArgs_ListsAllPanes(t *testing.T) {
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

	for _, name := range []string{"a1", "a2"} {
		call("session_create", map[string]any{"name": name, "command": "/bin/sh"})
	}

	body := extractText(t, call("list_panes", map[string]any{}))
	if !strings.Contains(body, `"a1:0"`) || !strings.Contains(body, `"a2:0"`) {
		t.Fatalf("expected both sessions in list_panes output: %s", body)
	}
}

// TestHandle_PaneSelect_AcceptsTarget confirms the happy path: a
// "session:window.pane" target is accepted by the tool surface.
func TestHandle_PaneSelect_AcceptsTarget(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
		"name": "ps", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "ps"}}))
	})

	out := call("pane_select", map[string]any{"target": "ps:0.0"})
	if got := extractText(t, out); got != "ok" {
		t.Fatalf("pane_select = %q, want \"ok\"", got)
	}
}

// TestHandle_PaneSelect_MissingTargetMapsCode pins the wire contract
// that pane_select against an unknown session surfaces
// CodeSessionNotFound rather than the generic internal-error code.
func TestHandle_PaneSelect_MissingTargetMapsCode(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Anchor the tmux server with a real session so the dispatcher hits
	// the "server is up but the named session does not exist" branch.
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "anchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name":      "pane_select",
		"arguments": map[string]any{"target": "definitely_does_not_exist_xyzzy:0.0"},
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

// TestHandle_PaneSelect_RejectsEmptyTarget guards against the zero-arg
// caller — the schema lists "target" as required, but the handler
// must also reject the empty string at runtime.
func TestHandle_PaneSelect_RejectsEmptyTarget(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "pane_select",
		"arguments": map[string]any{"target": ""},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ToolsList_IncludesPaneTools makes sure tools/list
// advertises the new tools so MCP clients can discover them.
func TestHandle_ToolsList_IncludesPaneTools(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	want := map[string]bool{"list_panes": false, "pane_select": false}
	for _, def := range listing {
		name, _ := def["name"].(string)
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, ok := range want {
		if !ok {
			t.Errorf("tools/list missing %q", name)
		}
	}
}
