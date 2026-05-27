package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// activeWindowIndex pulls the index of the currently-active window out
// of a list_windows response body. Centralised so the next_window
// assertions can compare integer indices and the failure message names
// the concrete index involved instead of dumping the raw body.
func activeWindowIndex(t *testing.T, body string) int {
	t.Helper()
	var obj struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_windows: %v\nbody=%s", err, body)
	}
	for _, w := range obj.Windows {
		if active, _ := w["active"].(bool); active {
			// JSON numbers decode as float64 — coerce explicitly so the
			// assertion is not sensitive to encoder choices.
			idx, _ := w["index"].(float64)
			return int(idx)
		}
	}
	return -1
}

// TestHandle_NextWindow_AdvancesActive runs the load-bearing happy path
// end-to-end through the dispatcher: a session with three windows, the
// active flag starting on index 0, and a next_window call must move it
// to index 1. Catches both the dispatcher wiring and the controller
// argv ordering in one shot.
func TestHandle_NextWindow_AdvancesActive(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "nwh", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "nwh"}}))
	})
	for _, name := range []string{"second", "third"} {
		callTool(t, tools, ctx, "window_create", map[string]any{
			"session": "nwh", "name": name, "command": "/bin/sh", "select": false,
		})
	}

	// Sanity baseline so a failed next_window is distinguishable from
	// a no-op: index 0 starts active because the first window of the
	// session is always focused after session_create.
	pre := extractText(t, callTool(t, tools, ctx, "list_windows", map[string]any{"session": "nwh"}))
	if got := activeWindowIndex(t, pre); got != 0 {
		t.Fatalf("baseline broken: active index = %d, want 0\nbody=%s", got, pre)
	}

	got := extractText(t, callTool(t, tools, ctx, "next_window", map[string]any{
		"target": "nwh",
	}))
	if got != "ok" {
		t.Fatalf("next_window text = %q, want \"ok\"", got)
	}

	post := extractText(t, callTool(t, tools, ctx, "list_windows", map[string]any{"session": "nwh"}))
	if idx := activeWindowIndex(t, post); idx != 1 {
		t.Fatalf("after next_window: active index = %d, want 1\nbody=%s", idx, post)
	}
}

// TestHandle_NextWindow_WithAlertAccepted pins that the optional
// `with_alert` flag is accepted by the schema and reaches the
// controller. The exact "skip to alerted window" semantics are tmux-
// version sensitive (and would require synthesising an activity event),
// so the assertion stays loose: a session with only fresh windows
// produces a ack — proving the field is wired through and the schema
// permits the value — without trying to pin tmux's interactive
// alert-tracking behaviour.
func TestHandle_NextWindow_WithAlertAccepted(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "nwa", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "nwa"}}))
	})
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "nwa", "name": "second", "command": "/bin/sh", "select": false,
	})

	// We don't strictly enforce success here because tmux's `-a` step
	// can fail with "no alerts" on some builds when there's literally
	// nothing alerted. What matters for the boundary contract is that
	// the schema accepts the field and the dispatcher reaches the
	// controller — i.e. the response is not the schema-shaped
	// CodeInvalidParams (-32602).
	params := mustJSON(t, map[string]any{
		"name":      "next_window",
		"arguments": map[string]any{"target": "nwa", "with_alert": true},
	})
	_, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil && rerr.Code == errs.CodeInvalidParams {
		t.Fatalf("with_alert must be accepted by the schema, got CodeInvalidParams: %s", rerr.Message)
	}
}

// TestHandle_NextWindow_RejectsBadTarget guards the regex policy on the
// `target` argument: any string that would otherwise be passed to tmux
// must be refused with CodeInvalidParams up front, mirroring
// window_select's contract.
func TestHandle_NextWindow_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "next_window",
		"arguments": map[string]any{"target": "bad name with spaces"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_NextWindow_RejectsEmptyTarget locks the up-front guard on
// the target argument — otherwise the dispatcher would build a tmux
// `next-window -t ""` and let tmux reject it with a noisier error.
func TestHandle_NextWindow_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "next_window",
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

// TestHandle_NextWindow_MissingSessionMapsCode pins the wire contract:
// next_window against an unknown session must surface
// CodeSessionNotFound (-32000), mirroring window_select / swap_window.
func TestHandle_NextWindow_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor a real session so the dispatcher hits the "server up,
	// session missing" branch rather than the noisier "no server
	// running" path.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "anchor_nw", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "anchor_nw"}}))
	})

	params := mustJSON(t, map[string]any{
		"name":      "next_window",
		"arguments": map[string]any{"target": "definitely_does_not_exist_xyzzy"},
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

// TestHandle_NextWindow_RejectsUnknownProperty guards the
// additionalProperties:false contract from the schema. An agent that
// misnames a field gets a fast schema-shaped rejection rather than a
// silent no-op — the same pattern every other window tool upholds.
func TestHandle_NextWindow_RejectsUnknownProperty(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		name, _ := def["name"].(string)
		if name != "next_window" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		if got, ok := schema["additionalProperties"].(bool); !ok || got {
			t.Errorf("schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		req, _ := schema["required"].([]string)
		if len(req) != 1 || req[0] != "target" {
			t.Errorf("required = %v, want [target]", req)
		}
		return
	}
	t.Fatalf("tools/list missing 'next_window'")
}

// TestHandle_NextWindow_NotInReadOnlyAllowlist locks the policy that
// next_window mutates state (it changes which window is active) and
// therefore must be rejected when the server is armed with -read-only.
// The readonly_test list already pins it on the mutators side; this
// duplicate at the dispatch layer guards against a future contributor
// removing it from one place but not the other.
func TestHandle_NextWindow_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("next_window") {
		t.Fatal("next_window must not be on the read-only allowlist (it mutates the active window pointer)")
	}
}
