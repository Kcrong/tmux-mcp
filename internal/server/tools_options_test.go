package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestShowOptions_ServerScope drives the happy path for scope=server
// through the JSON-RPC dispatcher: anchor the tmux server with a real
// session, call show_options, then assert the response envelope decodes
// cleanly and at least one well-known server-scope key is present.
func TestShowOptions_ServerScope(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	const name = "opts_srv"
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": name, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": name},
			}))
	})

	body := extractText(t, callTool(t, tools, ctx, "show_options", map[string]any{
		"scope": "server",
	}))

	var got struct {
		Options map[string]string `json:"options"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode show_options body %q: %v", body, err)
	}
	if len(got.Options) == 0 {
		t.Fatalf("expected non-empty server options, got %v", got.Options)
	}
	// buffer-limit is a long-standing tmux server option, present in
	// every supported version. Its absence would mean either the parser
	// dropped a line or the dispatcher hit the wrong scope.
	if _, ok := got.Options["buffer-limit"]; !ok {
		t.Fatalf("expected buffer-limit in server options, got %v", got.Options)
	}
}

// TestShowOptions_SessionScopeGlobal exercises scope=session with
// global=true so we can pin a known default (default-shell) without
// depending on any per-session overrides — a fresh test session has no
// overrides set, so only the -g view is guaranteed to be populated.
func TestShowOptions_SessionScopeGlobal(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	const name = "opts_sess"
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": name, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": name},
			}))
	})

	body := extractText(t, callTool(t, tools, ctx, "show_options", map[string]any{
		"scope":   "session",
		"session": name,
		"global":  true,
	}))
	var got struct {
		Options map[string]string `json:"options"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode show_options body %q: %v", body, err)
	}
	if _, ok := got.Options["default-shell"]; !ok {
		t.Fatalf("expected default-shell among global session options, got %v", got.Options)
	}
}

// TestShowOptions_RejectsMissingScope locks the up-front guard: the
// dispatcher must reject the call before it reaches tmux when scope is
// absent, returning the standard JSON-RPC invalid-params code.
func TestShowOptions_RejectsMissingScope(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	params := mustJSON(t, map[string]any{
		"name":      "show_options",
		"arguments": map[string]any{},
	})
	res, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected invalid-params error, got result %#v", res)
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams (%d), got %d", errs.CodeInvalidParams, rerr.Code)
	}
}

// TestShowOptions_RejectsSessionScopeWithoutSession verifies the
// per-scope required-field guards run before any tmux call is made.
func TestShowOptions_RejectsSessionScopeWithoutSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	params := mustJSON(t, map[string]any{
		"name":      "show_options",
		"arguments": map[string]any{"scope": "session"},
	})
	res, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected invalid-params error, got result %#v", res)
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams (%d), got %d", errs.CodeInvalidParams, rerr.Code)
	}
}

// TestShowOptions_RejectsWindowScopeWithoutWindow guards the second
// per-scope required-field branch: scope=window with a session but no
// window must surface as invalid-params.
func TestShowOptions_RejectsWindowScopeWithoutWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	params := mustJSON(t, map[string]any{
		"name": "show_options",
		"arguments": map[string]any{
			"scope":   "window",
			"session": "demo",
		},
	})
	res, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected invalid-params error, got result %#v", res)
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams (%d), got %d", errs.CodeInvalidParams, rerr.Code)
	}
}

// TestShowOptions_RejectsUnknownScope pins the wire contract for an
// unrecognised scope value: the dispatcher rejects it with the standard
// invalid-params code rather than falling through to a tmux error that
// would surface as -32603.
func TestShowOptions_RejectsUnknownScope(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	params := mustJSON(t, map[string]any{
		"name":      "show_options",
		"arguments": map[string]any{"scope": "everything"},
	})
	res, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected invalid-params error, got result %#v", res)
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams (%d), got %d", errs.CodeInvalidParams, rerr.Code)
	}
}

// TestShowOptions_ListedInTools confirms the init()-time registration
// actually wired show_options into tools/list.
func TestShowOptions_ListedInTools(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] == "show_options" {
			return
		}
	}
	t.Fatal("tools/list missing show_options")
}
