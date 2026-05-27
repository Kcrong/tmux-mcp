package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// activeWindowIndexFromTools returns the numeric index of the currently active
// window in the named session. Tests use it to assert which slot
// previous_window landed on without re-encoding the JSON parsing in
// every case. A missing-active result is reported via -1 so callers
// can fail the test with a meaningful diagnostic rather than an
// out-of-range panic.
func activeWindowIndexFromTools(t *testing.T, tools *Tools, ctx context.Context, session string) int {
	t.Helper()
	body := extractText(t, callTool(t, tools, ctx, "list_windows", map[string]any{"session": session}))
	var obj struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_windows: %v\nbody=%s", err, body)
	}
	for _, w := range obj.Windows {
		if active, _ := w["active"].(bool); active {
			// JSON numbers decode as float64; round-trip through int so
			// callers compare against integer literals cleanly.
			idx, _ := w["index"].(float64)
			return int(idx)
		}
	}
	t.Fatalf("no active window in session %q: %s", session, body)
	return -1
}

// TestHandle_PreviousWindow_StepsBackward pins the integration: with
// three windows in a session and the active flag on index 2,
// previous_window must move it to index 1 so a chained
// `previous_window -> capture` lands on the expected sibling.
func TestHandle_PreviousWindow_StepsBackward(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "pws", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "pws"}}))
	})

	// Two extra windows so the session has indices {0, 1, 2}; the
	// `select=true` on the second create lands the active flag on
	// index 2, our starting point for the step-backward assertion.
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "pws", "name": "mid", "command": "/bin/sh", "select": false,
	})
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "pws", "name": "last", "command": "/bin/sh", "select": true,
	})

	if got := activeWindowIndexFromTools(t, tools, ctx, "pws"); got != 2 {
		t.Fatalf("baseline broken: active index = %d, want 2", got)
	}

	body := extractText(t, callTool(t, tools, ctx, "previous_window", map[string]any{
		"target": "pws",
	}))
	if !strings.Contains(body, `"moved":true`) {
		t.Fatalf("previous_window response = %q, want to contain \"moved\":true", body)
	}

	if got := activeWindowIndexFromTools(t, tools, ctx, "pws"); got != 1 {
		t.Fatalf("active index after previous_window = %d, want 1", got)
	}
}

// TestHandle_PreviousWindow_RejectsBadTarget guards the regex policy on
// the `target` argument: a string that would otherwise be passed to
// tmux must be refused with CodeInvalidParams up front, mirroring
// every other window tool's input contract.
func TestHandle_PreviousWindow_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "previous_window",
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

// TestHandle_PreviousWindow_RejectsEmptyTarget locks the up-front
// guard on the target argument — otherwise the dispatcher would build
// `tmux previous-window -t ""` and let tmux interpret it against the
// current/global state, almost never what the caller meant.
func TestHandle_PreviousWindow_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "previous_window",
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

// TestHandle_PreviousWindow_MissingSessionMapsCode pins the
// CodeSessionNotFound contract: previous_window against an unknown
// session must surface -32000, mirroring the rest of the window tools.
func TestHandle_PreviousWindow_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor session so tmux's server is up — without it, the missing
	// target produces "no server running" and lands on a different
	// branch from the one we want to pin.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "anchor_pw", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name":      "previous_window",
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

// TestHandle_PreviousWindow_NotInReadOnlyAllowlist pins the policy
// decision: previous_window mutates state (it shifts the session's
// active window pointer) so it must be rejected when the operator
// has armed -read-only. Without this guard a future contributor
// adding the name to the read-only allowlist would silently widen
// the inspection surface.
func TestHandle_PreviousWindow_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("previous_window") {
		t.Fatal("IsReadOnlyTool(\"previous_window\") = true, want false (mutating tools must not be inspection-allowed)")
	}
}

// TestHandle_ToolsList_IncludesPreviousWindow verifies the dispatch
// surface advertises the new tool so MCP clients can discover it via
// tools/list. We also pin `additionalProperties: false` on the schema
// because the contract documents that any field other than `target` /
// `with_alert` is rejected up front.
func TestHandle_ToolsList_IncludesPreviousWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "previous_window" {
			schema, _ := def["inputSchema"].(map[string]any)
			if got, ok := schema["additionalProperties"].(bool); !ok || got {
				t.Errorf("previous_window schema additionalProperties = %v, want false", schema["additionalProperties"])
			}
			return
		}
	}
	t.Fatal("tools/list missing \"previous_window\"")
}
