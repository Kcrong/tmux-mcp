package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_KillWindow_RemovesNonLastWindow drives the common-case
// happy path: with two windows in a session, killing the second one by
// name returns `{"killed": true}` and leaves the session alive with the
// remaining window. Verifies the dispatcher is wired up, the schema
// accepts the documented arguments, and the response envelope carries
// the documented "killed" ack without a stray "session_killed" key
// (which would falsely tell the caller their session was reaped too).
func TestHandle_KillWindow_RemovesNonLastWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "kw_keep", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "kw_keep"}}))
	})

	// Add a second window so we can kill one and observe the session
	// surviving — without this anchor the test would conflate the
	// "common case" with the cascade path the next test pins.
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "kw_keep", "name": "second", "command": "/bin/sh", "select": false,
	})

	body := extractText(t, callTool(t, tools, ctx, "kill_window", map[string]any{
		"session": "kw_keep", "window": "second",
	}))
	var obj struct {
		Killed        bool `json:"killed"`
		SessionKilled bool `json:"session_killed"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode kill_window: %v\nbody=%s", err, body)
	}
	if !obj.Killed {
		t.Fatalf("kill_window killed = false, want true; body=%s", body)
	}
	if obj.SessionKilled {
		t.Fatalf("kill_window session_killed = true, want false (session must survive); body=%s", body)
	}
	// The common case must not even include the session_killed key —
	// agents that branch on its presence would otherwise see a noisy
	// `false` and have to add an extra check. Match the documented
	// "minimal payload" promise from tools.md.
	if strings.Contains(body, "session_killed") {
		t.Fatalf("kill_window body should omit session_killed when no cascade fired; got %s", body)
	}

	// Session must still be listed.
	listText := extractText(t, callTool(t, tools, ctx, "session_list", map[string]any{}))
	if !strings.Contains(listText, `"kw_keep"`) {
		t.Fatalf("session_list missing kw_keep after killing a non-last window: %s", listText)
	}
}

// TestHandle_KillWindow_LastWindowDestroysSession pins the cascade
// contract: killing the only window in a session collapses the session
// too, and the response surfaces that fact via `session_killed: true`
// rather than rejecting the call (which is what the older window_kill
// tool does). The session must no longer appear in session_list, and
// the snapshot history for that name must be forgotten so we don't
// leak entries across many create/kill cycles.
func TestHandle_KillWindow_LastWindowDestroysSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "kw_solo", "command": "/bin/sh",
	})
	// Best-effort cleanup — by the time the test body runs the session
	// is meant to be already dead, so the kill here is a guard against
	// an early test failure leaving stray state behind.
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "kw_solo"}}))
	})

	// Populate snapshot history so we can later assert it was forgotten
	// when the session was reaped along with the window. Mirrors the
	// session_kill cleanup contract.
	_ = extractText(t, callTool(t, tools, ctx, "capture", map[string]any{"session": "kw_solo"}))
	if !tools.Snap.Has("kw_solo") {
		t.Fatal("expected snapshot history for kw_solo after capture")
	}

	body := extractText(t, callTool(t, tools, ctx, "kill_window", map[string]any{
		"session": "kw_solo", "window": "0",
	}))
	var obj struct {
		Killed        bool `json:"killed"`
		SessionKilled bool `json:"session_killed"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode kill_window: %v\nbody=%s", err, body)
	}
	if !obj.Killed {
		t.Fatalf("kill_window killed = false, want true; body=%s", body)
	}
	if !obj.SessionKilled {
		t.Fatalf("kill_window session_killed = false, want true (last window cascade); body=%s", body)
	}

	// session_list must no longer mention the reaped session.
	listText := extractText(t, callTool(t, tools, ctx, "session_list", map[string]any{}))
	if strings.Contains(listText, `"kw_solo"`) {
		t.Fatalf("session_list still contains kw_solo after cascade kill: %s", listText)
	}
	// Snapshot history for the dead session must be dropped so we
	// don't leak per-session entries across create/kill cycles.
	if tools.Snap.Has("kw_solo") {
		t.Fatal("kill_window cascade should have forgotten snapshot history for kw_solo")
	}
}

// TestHandle_KillWindow_MissingSessionMapsCode pins the wire contract
// that kill_window against an unknown session surfaces
// CodeSessionNotFound (-32000), mirroring window_kill / session_kill.
// Without this, agents that branch on the typed code would have to
// substring-match tmux stderr to tell "session does not exist" apart
// from a generic internal failure.
func TestHandle_KillWindow_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so the dispatcher
	// hits the "server is up but the named session does not exist"
	// branch (a fresh controller has no socket file yet, which produces
	// a different stderr shape).
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "kw_anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "kill_window",
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

// TestHandle_KillWindow_RejectsEmptyWindow pins the up-front guard on
// the window argument — an empty string would otherwise produce the
// target string "demo:" and let tmux act on whatever it considers
// current, which is rarely what the agent meant to ask for. Mirrors
// the analogous guard on window_kill.
func TestHandle_KillWindow_RejectsEmptyWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "kill_window",
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

// TestHandle_KillWindow_RejectsBadWindowName guards the window-target
// regex: a value that would otherwise be interpolated into a tmux
// target must be refused with CodeInvalidParams up front so a stray
// quote / shell metachar can never reach tmux's argv.
func TestHandle_KillWindow_RejectsBadWindowName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "kill_window",
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

// TestHandle_KillWindow_RejectsBadSession guards the regex/length
// policy for the required `session` field — same rule every other tool
// enforces, surfaced before any tmux command runs.
func TestHandle_KillWindow_RejectsBadSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "kill_window",
		"arguments": map[string]any{"session": "bad name with spaces", "window": "0"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad session name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ToolsList_IncludesKillWindow makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Mirrors the smoke check every other tool ships
// with — a regression in init() registration would otherwise hide the
// tool from the surface even though the dispatcher case still works
// for a hardcoded call.
func TestHandle_ToolsList_IncludesKillWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "kill_window" {
			return
		}
	}
	t.Fatalf("tools/list missing kill_window")
}
