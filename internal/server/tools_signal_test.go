package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_SendSignal_TerminatesSession is the end-to-end happy path:
// spin up a session running `sleep 100`, fire send_signal TERM through
// the JSON-RPC dispatcher, and confirm the session ends within a short
// deadline. Catches both wiring (dispatcher case + handler) and the
// underlying tmuxctl.SendSignal call.
//
// We also create an anchor session so the tmux server is guaranteed
// to stay alive after the target session vanishes — without it the
// last session leaving would tear down the server itself, which
// makes session_list racy ("no server" vs "server exited unexpectedly"
// depending on timing).
func TestHandle_SendSignal_TerminatesSession(t *testing.T) {
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

	// Anchor keeps the tmux server alive past the sleep PID's exit.
	call("session_create", map[string]any{
		"name": "anchor", "command": "/bin/sh", "width": 80, "height": 20,
	})
	call("session_create", map[string]any{
		"name": "sgs", "command": "sleep 100", "width": 80, "height": 20,
	})

	out := call("send_signal", map[string]any{"session": "sgs", "signal": "TERM"})
	if got := extractText(t, out); got != "ok" {
		t.Fatalf("send_signal = %q, want \"ok\"", got)
	}

	// The session should disappear once the sleep child exits. Poll
	// session_list with a 5s ceiling to keep the test snappy.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		listText := extractText(t, call("session_list", map[string]any{}))
		if !strings.Contains(listText, `"sgs"`) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("session sgs still alive 5s after send_signal TERM")
}

// TestHandle_SendSignal_RejectsUnknownSignal pins the wire contract
// for "signal not in whitelist": CodeInvalidParams (-32602) so MCP
// clients can branch on the standard JSON-RPC code rather than the
// free-form message text.
func TestHandle_SendSignal_RejectsUnknownSignal(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "rsig", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name":      "send_signal",
		"arguments": map[string]any{"session": "rsig", "signal": "STOP"},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error for non-whitelisted signal, got result %#v", res)
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
}

// TestHandle_SendSignal_MissingSessionMapsCode pins the wire contract
// that send_signal against a session tmux doesn't know about returns
// CodeSessionNotFound, mirroring the contract enforced for
// session_kill / pane_select.
func TestHandle_SendSignal_MissingSessionMapsCode(t *testing.T) {
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
		"name": "send_signal",
		"arguments": map[string]any{
			"session": "definitely_does_not_exist_xyzzy",
			"signal":  "TERM",
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

// TestHandle_SendSignal_RejectsEmptySession guards against the
// zero-arg caller; the schema marks session as required, but the
// handler must also reject "" at runtime.
func TestHandle_SendSignal_RejectsEmptySession(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "send_signal",
		"arguments": map[string]any{"session": "", "signal": "TERM"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty session")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_SendSignal_RejectsBadSessionName guards the regex/length
// check on session names — the same policy every other tool enforces
// must apply here too.
func TestHandle_SendSignal_RejectsBadSessionName(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "send_signal",
		"arguments": map[string]any{"session": "bad name with spaces", "signal": "TERM"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad session name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_SendSignal_RejectsEmptySignal locks down the missing-
// signal branch: the dispatcher must return CodeInvalidParams rather
// than letting an empty string fall through to the controller.
func TestHandle_SendSignal_RejectsEmptySignal(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "send_signal",
		"arguments": map[string]any{"session": "demo", "signal": ""},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty signal")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ToolsList_IncludesSendSignal makes sure tools/list
// advertises the new tool so MCP clients can discover it.
func TestHandle_ToolsList_IncludesSendSignal(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "send_signal" {
			return
		}
	}
	t.Fatal("tools/list missing send_signal")
}
