package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_WindowSelect_MovesActiveFlag pins the integration: after
// creating a second window in the background and calling window_select,
// list_windows must report the second window as active so an agent can
// rely on the chain `create -> select -> list_windows -> capture`.
func TestHandle_WindowSelect_MovesActiveFlag(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "ws", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "ws"}}))
	})

	// Background the new window so we can prove the selection actually
	// moved the active flag rather than just confirming the post-create
	// state.
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "ws", "name": "second", "command": "/bin/sh", "select": false,
	})

	// Sanity: 'second' should *not* be active yet — without this baseline
	// the test could not distinguish a working window_select from a no-op.
	body := extractText(t, callTool(t, tools, ctx, "list_windows", map[string]any{"session": "ws"}))
	var pre struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(body), &pre); err != nil {
		t.Fatalf("decode list_windows: %v\nbody=%s", err, body)
	}
	for _, w := range pre.Windows {
		if name, _ := w["name"].(string); name == "second" {
			if active, _ := w["active"].(bool); active {
				t.Fatalf("baseline broken: 'second' active before window_select: %s", body)
			}
		}
	}

	got := extractText(t, callTool(t, tools, ctx, "window_select", map[string]any{
		"session": "ws", "target": "second",
	}))
	if got != "ok" {
		t.Fatalf("window_select text = %q, want \"ok\"", got)
	}

	body = extractText(t, callTool(t, tools, ctx, "list_windows", map[string]any{"session": "ws"}))
	var post struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(body), &post); err != nil {
		t.Fatalf("decode list_windows: %v\nbody=%s", err, body)
	}
	sawActive := false
	for _, w := range post.Windows {
		if name, _ := w["name"].(string); name == "second" {
			if active, _ := w["active"].(bool); active {
				sawActive = true
			}
		}
	}
	if !sawActive {
		t.Fatalf("'second' not active after window_select: %s", body)
	}
}

// TestHandle_WindowSelect_RejectsBadTarget guards the regex policy on
// the `target` argument: a string that would otherwise be passed to
// tmux must be refused with CodeInvalidParams up front.
func TestHandle_WindowSelect_RejectsBadTarget(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "window_select",
		"arguments": map[string]any{"session": "demo", "target": "bad name with spaces"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_WindowSelect_RejectsEmptyTarget locks the up-front guard
// on the target argument — otherwise the dispatcher would build the
// tmux target "session:" and let tmux pick whatever it considers
// current, almost never what the caller meant.
func TestHandle_WindowSelect_RejectsEmptyTarget(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "window_select",
		"arguments": map[string]any{"session": "demo", "target": ""},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_WindowSelect_MissingSessionMapsCode pins the
// CodeSessionNotFound contract: window_select against an unknown
// session must surface -32000, mirroring the rest of the window tools.
func TestHandle_WindowSelect_MissingSessionMapsCode(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name":      "window_select",
		"arguments": map[string]any{"session": "definitely_does_not_exist_xyzzy", "target": "0"},
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

// TestHandle_WindowRename_UpdatesListWindows runs the happy rename
// path: after renaming, list_windows must reflect the new label and
// the old one must be gone.
func TestHandle_WindowRename_UpdatesListWindows(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "wr", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "wr"}}))
	})

	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "wr", "name": "before", "command": "/bin/sh", "select": false,
	})

	got := extractText(t, callTool(t, tools, ctx, "window_rename", map[string]any{
		"session": "wr", "target": "before", "name": "after",
	}))
	if !strings.Contains(got, `"after"`) || !strings.Contains(got, `"wr:before"`) {
		t.Fatalf("window_rename text = %q, want references to 'wr:before' and 'after'", got)
	}

	body := extractText(t, callTool(t, tools, ctx, "list_windows", map[string]any{"session": "wr"}))
	var obj struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_windows: %v\nbody=%s", err, body)
	}
	sawAfter, sawBefore := false, false
	for _, w := range obj.Windows {
		switch name, _ := w["name"].(string); name {
		case "after":
			sawAfter = true
		case "before":
			sawBefore = true
		}
	}
	if !sawAfter {
		t.Fatalf("missing 'after' after rename: %s", body)
	}
	if sawBefore {
		t.Fatalf("'before' still present after rename: %s", body)
	}
}

// TestHandle_WindowRename_RejectsBadName guards the regex/length
// policy on the new `name` argument — the same rule window_create's
// optional `name` enforces, restated here so a hostile rename fails
// fast.
func TestHandle_WindowRename_RejectsBadName(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "window_rename",
		"arguments": map[string]any{
			"session": "demo", "target": "0", "name": "bad name with spaces",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_WindowRename_RejectsEmptyName pins the contract that
// `name` is required. tmux would otherwise reject the argument with a
// confusing error; the boundary catches it up front with
// CodeInvalidParams.
func TestHandle_WindowRename_RejectsEmptyName(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "window_rename",
		"arguments": map[string]any{"session": "demo", "target": "0", "name": ""},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_WindowRename_MissingSessionMapsCode pins the
// CodeSessionNotFound contract for the rename path, mirroring the
// CreateWindow / SelectWindow tests above.
func TestHandle_WindowRename_MissingSessionMapsCode(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "window_rename",
		"arguments": map[string]any{
			"session": "definitely_does_not_exist_xyzzy", "target": "0", "name": "x",
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

// TestHandle_ToolsList_IncludesWindowNavigation verifies the dispatch
// surface advertises the two new window-navigation tools so MCP
// clients can discover them via tools/list.
func TestHandle_ToolsList_IncludesWindowNavigation(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	want := map[string]bool{"window_select": false, "window_rename": false}
	for _, def := range listing {
		name, _ := def["name"].(string)
		if _, ok := want[name]; ok {
			want[name] = true
			schema, _ := def["inputSchema"].(map[string]any)
			// additionalProperties:false is part of the contract — an
			// agent that misnames a field gets a fast schema-shaped
			// rejection rather than a silent no-op.
			if got, ok := schema["additionalProperties"].(bool); !ok || got {
				t.Errorf("%s schema additionalProperties = %v, want false", name, schema["additionalProperties"])
			}
		}
	}
	for name, ok := range want {
		if !ok {
			t.Errorf("tools/list missing %q", name)
		}
	}
}
