package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_ListWindows_AfterSessionCreate runs the happy path through
// the tool surface: a freshly created session has exactly one window,
// and the list_windows envelope echoes the structured fields an agent
// would switch on (index, name, active, panes).
func TestHandle_ListWindows_AfterSessionCreate(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lw", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "lw"}}))
	})

	body := extractText(t, callTool(t, tools, ctx, "list_windows", map[string]any{"session": "lw"}))
	var obj struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_windows: %v\nbody=%s", err, body)
	}
	if len(obj.Windows) != 1 {
		t.Fatalf("expected exactly one window, got %d (%s)", len(obj.Windows), body)
	}
	w := obj.Windows[0]
	// JSON numbers decode as float64 — coerce explicitly so the test
	// is not sensitive to encoder choices.
	if idx, _ := w["index"].(float64); int(idx) != 0 {
		t.Errorf("index = %v, want 0", w["index"])
	}
	if name, _ := w["name"].(string); name == "" {
		t.Error("name empty even though tmux always assigns one")
	}
	if active, _ := w["active"].(bool); !active {
		t.Error("expected the only window of a fresh session to be active")
	}
	if panes, _ := w["panes"].(float64); int(panes) != 1 {
		t.Errorf("panes = %v, want 1", w["panes"])
	}
}

// TestHandle_ListWindows_MultiWindow drives the multi-window path: a
// session with two windows must surface both, with exactly one flagged
// as active.
func TestHandle_ListWindows_MultiWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "mw", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "mw"}}))
	})
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "mw", "name": "second", "command": "/bin/sh", "select": true,
	})

	body := extractText(t, callTool(t, tools, ctx, "list_windows", map[string]any{"session": "mw"}))
	var obj struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_windows: %v\nbody=%s", err, body)
	}
	if len(obj.Windows) != 2 {
		t.Fatalf("expected 2 windows, got %d (%s)", len(obj.Windows), body)
	}
	activeCount := 0
	sawSecond := false
	for _, w := range obj.Windows {
		if active, _ := w["active"].(bool); active {
			activeCount++
		}
		if name, _ := w["name"].(string); name == "second" {
			sawSecond = true
		}
	}
	if !sawSecond {
		t.Errorf("missing 'second' window in tool output: %s", body)
	}
	if activeCount != 1 {
		t.Errorf("expected exactly one active window, got %d (%s)", activeCount, body)
	}
}

// TestHandle_ListWindows_NoArgs_ListsAllSessions proves the empty-
// session branch (server-wide -a listing) works through the tool
// surface — symmetric to the list_panes "no args" contract.
func TestHandle_ListWindows_NoArgs_ListsAllSessions(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	for _, name := range []string{"a1", "a2"} {
		callTool(t, tools, ctx, "session_create", map[string]any{
			"name": name, "command": "/bin/sh",
		})
	}

	body := extractText(t, callTool(t, tools, ctx, "list_windows", map[string]any{}))
	var obj struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_windows: %v\nbody=%s", err, body)
	}
	if len(obj.Windows) < 2 {
		t.Fatalf("expected at least 2 windows across 2 sessions, got %d (%s)", len(obj.Windows), body)
	}
}

// TestHandle_ListWindows_MissingSessionMapsCode pins the wire contract
// that asking for a non-existent session surfaces CodeSessionNotFound
// instead of a generic internal-error code, mirroring list_panes /
// session_kill / pane_select.
func TestHandle_ListWindows_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so the dispatcher
	// hits the "server is up but the named session does not exist"
	// branch.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name":      "list_windows",
		"arguments": map[string]any{"session": "definitely_does_not_exist_xyzzy"},
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

// TestHandle_ListWindows_RejectsBadSession guards the regex/length
// policy on the optional `session` argument — even though it's
// optional, a present-but-malformed value must still be refused with
// CodeInvalidParams up front so tmux is never asked to resolve it.
func TestHandle_ListWindows_RejectsBadSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "list_windows",
		"arguments": map[string]any{"session": "bad name with spaces"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad session name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ToolsList_IncludesListWindows makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint.
func TestHandle_ToolsList_IncludesListWindows(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "list_windows" {
			schema, _ := def["inputSchema"].(map[string]any)
			// additionalProperties:false is part of the contract — an
			// agent that misnames a field gets a fast schema-shaped
			// rejection rather than a silent no-op.
			if got, ok := schema["additionalProperties"].(bool); !ok || got {
				t.Errorf("list_windows schema additionalProperties = %v, want false", schema["additionalProperties"])
			}
			// Ensure the optional `session` arg carries the maxLength
			// bound documented in the schema.
			props, _ := schema["properties"].(map[string]any)
			if sess, _ := props["session"].(map[string]any); sess != nil {
				if maxLen, _ := sess["maxLength"].(int); maxLen != maxSessionNameLen {
					// JSON marshalling sometimes converts ints; allow
					// either int or float64.
					if maxLenF, _ := sess["maxLength"].(float64); int(maxLenF) != maxSessionNameLen {
						t.Errorf("list_windows session.maxLength = %v, want %d", sess["maxLength"], maxSessionNameLen)
					}
				}
			} else {
				t.Errorf("list_windows missing 'session' property in schema: %v", props)
			}
			return
		}
	}
	t.Fatalf("tools/list missing list_windows: %v", listing)
}

// TestHandle_ListWindows_AcceptsNullArguments guards the "raw is empty"
// branch — the dispatcher hands list_windows a nil-ish payload when
// the caller sends `arguments: {}`. The handler must accept it as
// "list every window on the server" rather than rejecting it as
// malformed.
func TestHandle_ListWindows_AcceptsNullArguments(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "any", "command": "/bin/sh",
	})

	res := callTool(t, tools, ctx, "list_windows", map[string]any{})
	body := extractText(t, res)
	if !strings.Contains(body, `"windows"`) {
		t.Fatalf("expected windows envelope, got %s", body)
	}
}
