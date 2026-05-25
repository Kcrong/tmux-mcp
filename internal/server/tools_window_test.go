package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// callTool is a tiny test helper that JSON-encodes the args, fires
// tools/call through the dispatcher, and fails the test on RPC error.
// Returns the raw result so callers can extract the text block. Lives
// here (not in tools_test.go) so the window suite stays self-contained.
func callTool(t *testing.T, tools *Tools, ctx context.Context, name string, args any) any {
	t.Helper()
	params := mustJSON(t, map[string]any{"name": name, "arguments": args})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("%s: %s", name, rerr.Message)
	}
	return res
}

// TestHandle_WindowCreate_NamedAndSelected drives the happy path: a
// freshly created session gets a second window, focused, with a
// caller-supplied name. The response must echo back the new label and
// session so an agent can chain follow-ups.
func TestHandle_WindowCreate_NamedAndSelected(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "wc", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "wc"}}))
	})

	res := callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "wc", "name": "build", "command": "/bin/sh", "select": true,
	})
	got := extractText(t, res)
	if !strings.Contains(got, `"build"`) || !strings.Contains(got, `"wc"`) {
		t.Fatalf("window_create text = %q, want both 'build' and 'wc'", got)
	}
}

// TestHandle_WindowCreate_UnnamedFallsBackToIndex covers the
// "tmux assigned the name" branch: when the caller omits `name`, the
// response must still carry a usable identifier (the numeric index)
// so a follow-up window_kill has something to target.
func TestHandle_WindowCreate_UnnamedFallsBackToIndex(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "wcu", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "wcu"}}))
	})

	res := callTool(t, tools, ctx, "window_create", map[string]any{"session": "wcu"})
	got := extractText(t, res)
	if !strings.Contains(got, `"wcu"`) {
		t.Fatalf("window_create text = %q, want session 'wcu' in response", got)
	}
}

// TestHandle_WindowCreate_MissingSessionMapsCode pins the wire
// contract: window_create against an unknown session must return
// CodeSessionNotFound, mirroring session_kill / pane_select.
func TestHandle_WindowCreate_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor so the dispatcher hits "server up, session missing".
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name":      "window_create",
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

// TestHandle_WindowCreate_RejectsBadName guards the regex/length check
// on the optional `name` argument. Anything that would slip past
// tmux's own quoting (spaces, colons, dots) must be refused with
// CodeInvalidParams up front.
func TestHandle_WindowCreate_RejectsBadName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "window_create",
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

// TestHandle_WindowCreate_RejectsBadSession guards the same regex/
// length policy for `session` that every other tool enforces — extra
// belt because the dispatcher should never fall through to tmux on a
// malformed reference.
func TestHandle_WindowCreate_RejectsBadSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "window_create",
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

// TestHandle_WindowKill_RemovesNonLastWindow runs the happy path: with
// two windows in a session, killing the second by name yields the
// expected text block and leaves the session alive.
func TestHandle_WindowKill_RemovesNonLastWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "wk", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "wk"}}))
	})

	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "wk", "name": "second", "command": "/bin/sh", "select": false,
	})

	got := extractText(t, callTool(t, tools, ctx, "window_kill", map[string]any{
		"session": "wk", "window": "second",
	}))
	if !strings.Contains(got, `"wk:second"`) {
		t.Fatalf("window_kill text = %q, want contains 'wk:second'", got)
	}

	// Session must still be listed.
	listText := extractText(t, callTool(t, tools, ctx, "session_list", map[string]any{}))
	if !strings.Contains(listText, `"wk"`) {
		t.Fatalf("session_list missing wk after killing a non-last window: %s", listText)
	}
}

// TestHandle_WindowKill_RejectsLastWindow pins the contract that the
// boundary refuses to destroy the only window of a session — the
// JSON-RPC code must be CodeInvalidParams (-32602) and the session
// must remain alive.
func TestHandle_WindowKill_RejectsLastWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "wkl", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "wkl"}}))
	})

	params := mustJSON(t, map[string]any{
		"name":      "window_kill",
		"arguments": map[string]any{"session": "wkl", "window": "0"},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error killing last window, got result %#v", res)
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
	if !strings.Contains(rerr.Message, "session_kill") {
		t.Errorf("error message %q should hint at session_kill", rerr.Message)
	}

	// Session must still be alive — the refusal must not have
	// side-effected by way of partial tmux state.
	listText := extractText(t, callTool(t, tools, ctx, "session_list", map[string]any{}))
	if !strings.Contains(listText, `"wkl"`) {
		t.Fatalf("session_list missing wkl after refused window_kill: %s", listText)
	}
}

// TestHandle_WindowKill_MissingSessionMapsCode pins the
// CodeSessionNotFound contract for the kill path, mirroring the
// CreateWindow test above.
func TestHandle_WindowKill_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "window_kill",
		"arguments": map[string]any{
			"session": "definitely_does_not_exist_xyzzy",
			"window":  "0",
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

// TestHandle_WindowKill_RejectsBadWindowName guards the window-target
// regex: a value that would otherwise become a tmux target must be
// refused with CodeInvalidParams up front.
func TestHandle_WindowKill_RejectsBadWindowName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "window_kill",
		"arguments": map[string]any{"session": "demo", "window": "bad name with spaces"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad window name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_WindowKill_RejectsEmptyWindow pins the up-front guard on
// the window argument — an empty string would otherwise produce the
// target string "demo:" and let tmux act on whatever it considers
// current.
func TestHandle_WindowKill_RejectsEmptyWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "window_kill",
		"arguments": map[string]any{"session": "demo", "window": ""},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty window")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ToolsList_IncludesWindowTools makes sure tools/list
// advertises the new tools so MCP clients can discover them via the
// schema endpoint.
func TestHandle_ToolsList_IncludesWindowTools(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	want := map[string]bool{"window_create": false, "window_kill": false}
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
