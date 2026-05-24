package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// decodeNewWindowResult unwraps the {"content":[{"text":"<json>"}]}
// envelope new_window emits and decodes the JSON payload into a typed
// shape so individual tests can assert on the structured fields without
// re-doing the boilerplate. Keeping it local to this file (rather than
// pushing it into tools_test.go) avoids leaking the wire shape into
// the rest of the suite, which still works mostly with text blocks.
func decodeNewWindowResult(t *testing.T, result any) struct {
	Session     string `json:"session"`
	WindowIndex int    `json:"window_index"`
	WindowID    string `json:"window_id"`
	WindowName  string `json:"window_name"`
} {
	t.Helper()
	var out struct {
		Session     string `json:"session"`
		WindowIndex int    `json:"window_index"`
		WindowID    string `json:"window_id"`
		WindowName  string `json:"window_name"`
	}
	body := extractText(t, result)
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode new_window body=%s: %v", body, err)
	}
	return out
}

// TestHandle_NewWindow_HappyPathReturnsStructuredID is the primary
// integration test required by the spec: after spinning up a real tmux
// session, calling new_window must return a JSON payload whose
// `window_id` field is non-empty (the `@N` form tmux assigns). We also
// assert on the surrounding fields so a regression in the parser
// (wrong field order, dropped name) goes red here rather than silently
// degrading the response shape.
func TestHandle_NewWindow_HappyPathReturnsStructuredID(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "nw1", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "nw1"}}))
	})

	res := callTool(t, tools, ctx, "new_window", map[string]any{
		"session": "nw1", "name": "build", "command": "/bin/sh", "select": true,
	})
	got := decodeNewWindowResult(t, res)
	if got.Session != "nw1" {
		t.Errorf("session = %q, want nw1", got.Session)
	}
	if got.WindowName != "build" {
		t.Errorf("window_name = %q, want build", got.WindowName)
	}
	if got.WindowID == "" {
		t.Errorf("window_id empty, want non-empty `@N` form")
	}
	if !strings.HasPrefix(got.WindowID, "@") {
		t.Errorf("window_id %q missing `@` prefix", got.WindowID)
	}
	if got.WindowIndex <= 0 {
		// The session's first window is 0, so the new one must be > 0.
		t.Errorf("window_index = %d, want > 0", got.WindowIndex)
	}
}

// TestHandle_NewWindow_AfterIndexInsertsAtSlot pins the after_index
// semantics on the wire: a window asked to land "after index 0" must
// actually end up at index 1, not appended at the end of the session.
// Without this the field could regress to a no-op and the tests above
// would still pass.
func TestHandle_NewWindow_AfterIndexInsertsAtSlot(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "nwafter", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "nwafter"}}))
	})

	res := callTool(t, tools, ctx, "new_window", map[string]any{
		"session": "nwafter", "name": "afterzero", "command": "/bin/sh",
		"select": false, "after_index": 0,
	})
	got := decodeNewWindowResult(t, res)
	if got.WindowIndex != 1 {
		t.Fatalf("window_index = %d, want 1 when after_index=0", got.WindowIndex)
	}
}

// TestHandle_NewWindow_BackgroundDoesNotFocus pins the select=false
// semantics: the original window must remain the active one after the
// new window is created. Mirrors the window_create background test
// because new_window shares the same tmux flag mapping.
func TestHandle_NewWindow_BackgroundDoesNotFocus(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "nwbg", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "nwbg"}}))
	})

	got := decodeNewWindowResult(t, callTool(t, tools, ctx, "new_window", map[string]any{
		"session": "nwbg", "name": "bg", "command": "/bin/sh", "select": false,
	}))
	if got.WindowName != "bg" {
		t.Fatalf("window_name = %q, want bg", got.WindowName)
	}

	// Confirm the original window (index 0) is still the active one.
	listText := extractText(t, callTool(t, tools, ctx, "list_windows", map[string]any{"session": "nwbg"}))
	var listing struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(listText), &listing); err != nil {
		t.Fatalf("decode list_windows: %v\nbody=%s", err, listText)
	}
	for _, w := range listing.Windows {
		name, _ := w["name"].(string)
		active, _ := w["active"].(bool)
		if name == "bg" && active {
			t.Fatalf("'bg' active despite select=false: %s", listText)
		}
	}
}

// TestHandle_NewWindow_RejectsCommandWithNewlines pins the spec's
// validation rule: a command containing a newline must be refused with
// CodeInvalidParams up front, before any tmux call is issued. Prevents
// tmux's command parser from silently splitting the input into
// multiple shell commands.
func TestHandle_NewWindow_RejectsCommandWithNewlines(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "new_window",
		"arguments": map[string]any{
			"session": "demo",
			"command": "echo one\necho two",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for newline-bearing command")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "newline") {
		t.Errorf("error %q should mention newlines", rerr.Message)
	}
}

// TestHandle_NewWindow_RejectsCarriageReturnInCommand is a sibling of
// the newline test — `\r` would slip past a naive `\n`-only check and
// still confuse tmux's parser the same way. Keep both forms covered so
// the validator can't quietly regress to a half-fix.
func TestHandle_NewWindow_RejectsCarriageReturnInCommand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "new_window",
		"arguments": map[string]any{
			"session": "demo",
			"command": "echo one\rsneaky",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for CR-bearing command")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_NewWindow_MissingSessionMapsCode pins the wire contract:
// new_window against an unknown session must return CodeSessionNotFound,
// mirroring the matching window_create test.
func TestHandle_NewWindow_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor so the dispatcher hits "server up, session missing".
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "nwanchor", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "nwanchor"}}))
	})

	params := mustJSON(t, map[string]any{
		"name":      "new_window",
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

// TestHandle_NewWindow_RejectsEmptySession guards the up-front session
// check — tmux would otherwise resolve "" to whatever it considers
// current.
func TestHandle_NewWindow_RejectsEmptySession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "new_window",
		"arguments": map[string]any{"session": ""},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty session")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_NewWindow_RejectsBadName guards the regex/length check on
// the optional `name` argument — the same conservative policy
// window_create uses, since both tools accept the same identifier.
func TestHandle_NewWindow_RejectsBadName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "new_window",
		"arguments": map[string]any{
			"session": "demo",
			"name":    "bad name with spaces",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad window name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_NewWindow_RejectsNegativeAfterIndex pins the up-front
// guard: a negative after_index would otherwise collide with the -1
// "no preference" sentinel the tmuxctl layer uses internally and fall
// through to "append" silently, which is rarely what the agent meant.
func TestHandle_NewWindow_RejectsNegativeAfterIndex(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "new_window",
		"arguments": map[string]any{
			"session":     "demo",
			"after_index": -2,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for negative after_index")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_NewWindow_ToolsListIncludesIt makes sure tools/list
// advertises the new tool so MCP clients can discover its schema.
func TestHandle_NewWindow_ToolsListIncludesIt(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "new_window" {
			return
		}
	}
	t.Fatal("tools/list missing new_window")
}
