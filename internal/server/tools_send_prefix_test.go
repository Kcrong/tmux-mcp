package server

import (
	"context"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_SendPrefix_RoundTrip drives the load-bearing happy path
// through the JSON-RPC dispatcher: session_create → send_prefix against
// the active pane → "ok" ack. tmux delivers the configured prefix key
// (default C-b, byte 0x02) into the pane's pty; we rely on the absence
// of an error as the primary signal because asserting on the literal
// prefix byte appearing inside `cat` would couple the test to a
// particular shell's echo semantics — and "no error" is exactly what
// the JSON-RPC dispatcher promises clients.
func TestHandle_SendPrefix_RoundTrip(t *testing.T) {
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
		"name": "sp", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "sp"}}))
	})

	// Primary prefix.
	if got := extractText(t, call("send_prefix", map[string]any{"target": "sp"})); got != "ok" {
		t.Fatalf("send_prefix primary: got %q, want ok", got)
	}
	// Secondary prefix — tmux accepts -2 even when prefix2 is unset
	// (it falls back to whatever is configured), so the assertion is
	// "the flag did not break the call".
	if got := extractText(t, call("send_prefix", map[string]any{
		"target": "sp", "secondary": true,
	})); got != "ok" {
		t.Fatalf("send_prefix secondary: got %q, want ok", got)
	}
}

// TestHandle_SendPrefix_RejectsMissingTarget pins the required-field
// path: omitting `target` must come back as CodeInvalidParams rather
// than falling through to tmux with an empty -t value (which tmux
// would resolve to whatever pane it considers current).
func TestHandle_SendPrefix_RejectsMissingTarget(t *testing.T) {
	t.Parallel()

	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "send_prefix",
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

// TestHandle_SendPrefix_RejectsBadTarget locks the regex check on
// `target` so a stray quote/whitespace can't slip through to the tmux
// argv.
func TestHandle_SendPrefix_RejectsBadTarget(t *testing.T) {
	t.Parallel()

	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "send_prefix",
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

// TestHandle_SendPrefix_MissingSessionMapsCode pins the wire contract
// that send_prefix against a target on an unknown session surfaces
// CodeSessionNotFound (-32000), mirroring pane_kill / clear_history.
// The controller layer handles the "can't find pane" → sentinel
// translation; this test exists so a future refactor that drops the
// translation would fail at the boundary contract clients depend on.
func TestHandle_SendPrefix_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()

	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, pane missing"
	// rather than "no server" (different stderr shape, different code).
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "anchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name": "send_prefix",
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

// TestHandle_ToolsList_IncludesSendPrefix makes sure tools/list
// advertises send_prefix so MCP clients can discover it via the
// schema endpoint. This is the discovery half of the contract; the
// dispatch case in tools.go is the runtime half.
func TestHandle_ToolsList_IncludesSendPrefix(t *testing.T) {
	t.Parallel()

	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "send_prefix" {
			return
		}
	}
	t.Fatalf("tools/list missing send_prefix")
}

// TestHandle_SendPrefix_NotInReadOnlyAllowlist pins the policy that
// send_prefix mutates tmux state (it delivers a keystroke into the
// pane's pty) and therefore must NOT be inspection-allowed under
// -read-only. A future contributor adding it to the allowlist would
// silently let read-only agents drive prefix-bound commands.
func TestHandle_SendPrefix_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("send_prefix") {
		t.Fatal("send_prefix must not be inspection-allowed (it mutates pane state)")
	}
}
