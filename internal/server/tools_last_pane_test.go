package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_LastPane_TogglesActivePane drives the happy-path wire
// contract: the dispatcher accepts a target_window pointing at a
// two-pane window and tmux flips the active flag to the previously-
// active pane. We confirm via list_panes that the active flag actually
// moved — anything less would let a future regression silently pass a
// no-op call.
func TestHandle_LastPane_TogglesActivePane(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lpt", "command": "/bin/sh",
	})
	// Split so the window has two panes; pane_split focuses the new
	// pane by default, giving us a known "previously active" pane to
	// flip back to.
	callTool(t, tools, ctx, "pane_split", map[string]any{
		"session": "lpt", "direction": "horizontal", "command": "/bin/sh",
	})

	beforeRes := callTool(t, tools, ctx, "list_panes", map[string]any{"session": "lpt"})
	beforeActive := readActivePaneID(t, beforeRes)
	if beforeActive == "" {
		t.Fatalf("no active pane in baseline; body=%s", extractText(t, beforeRes))
	}

	res := callTool(t, tools, ctx, "last_pane", map[string]any{"target_window": "lpt:0"})
	if got := extractText(t, res); got != "ok" {
		t.Fatalf("last_pane text = %q, want ok", got)
	}

	afterRes := callTool(t, tools, ctx, "list_panes", map[string]any{"session": "lpt"})
	afterActive := readActivePaneID(t, afterRes)
	if afterActive == "" {
		t.Fatalf("no active pane after last_pane; body=%s", extractText(t, afterRes))
	}
	if afterActive == beforeActive {
		t.Fatalf("active pane did not move: before=%s after=%s", beforeActive, afterActive)
	}
}

// TestHandle_LastPane_AcceptsEmptyArguments guards the "raw is empty"
// branch — the dispatcher hands last_pane a nil-ish payload when the
// caller sends `arguments: {}`. The handler must accept it as the
// "no flags, current window" form rather than rejecting it as
// malformed. We anchor a session so tmux has a current window to
// resolve against; with no target the call may fail to find a "last"
// pane (single-pane window has no history), so we just confirm the
// dispatch path itself does not surface CodeInvalidParams.
func TestHandle_LastPane_AcceptsEmptyArguments(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lpempty", "command": "/bin/sh",
	})

	// Construct params manually so we can omit the "arguments" key
	// entirely — that's the path that exercises the len(raw) == 0
	// branch in the handler.
	params := mustJSON(t, map[string]any{"name": "last_pane"})
	_, rerr := tools.Handle(ctx, "tools/call", params)
	// Either the call succeeds or tmux complains about no last pane;
	// what we care about is that the handler did NOT reject the
	// payload as malformed (-32602 from the json.Unmarshal branch).
	if rerr != nil && rerr.Code == errs.CodeInvalidParams &&
		strings.Contains(rerr.Message, "json:") {
		t.Fatalf("unexpected json-level rejection on empty arguments: %s", rerr.Message)
	}
}

// TestHandle_LastPane_RejectsMutuallyExclusiveFlags pins the validation
// gate: setting both disable_input and enable_input must return
// CodeInvalidParams up front, before any tmux call runs. tmux itself
// would silently honour whichever flag came last, which is a
// confusing footgun — refusing the request keeps the contract loud.
func TestHandle_LastPane_RejectsMutuallyExclusiveFlags(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "last_pane",
		"arguments": map[string]any{
			"target_window": "demo:0",
			"disable_input": true,
			"enable_input":  true,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected error for mutually-exclusive disable_input/enable_input")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
	if !strings.Contains(rerr.Message, "mutually exclusive") {
		t.Errorf("message = %q, want a 'mutually exclusive' explanation", rerr.Message)
	}
}

// TestHandle_LastPane_MissingSessionMapsCode pins the wire contract that
// asking for a non-existent session surfaces CodeSessionNotFound rather
// than a generic internal-error code, mirroring window_select /
// list_windows / session_kill.
func TestHandle_LastPane_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor a real session so we exercise "server up, target missing"
	// rather than the "no server running" branch.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lpanchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "last_pane",
		"arguments": map[string]any{
			"target_window": "definitely_does_not_exist_xyzzy:0",
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

// TestHandle_LastPane_RejectsBadTargetShape pins the boundary
// validation: a target_window without a `:` separator must be refused
// with CodeInvalidParams up front so tmux is never asked to resolve
// an ambiguous reference.
func TestHandle_LastPane_RejectsBadTargetShape(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "last_pane",
		"arguments": map[string]any{
			"target_window": "no_colon_here",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected error for missing ':' in target_window")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_LastPane_RejectsBadSession guards the regex/length policy
// on the session half of target_window: a typo / shell metachar must
// be refused before tmux is consulted, mirroring choose_tree's session
// validation.
func TestHandle_LastPane_RejectsBadSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "last_pane",
		"arguments": map[string]any{
			"target_window": "bad name with spaces:0",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected error for bad session name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_LastPane_RejectsBadWindow guards the regex/length policy
// on the window half of target_window: a malformed window name must be
// refused before tmux is consulted.
func TestHandle_LastPane_RejectsBadWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "last_pane",
		"arguments": map[string]any{
			"target_window": "ok:bad win",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected error for bad window name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ToolsList_IncludesLastPane makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Mirrors the smoke check every other tool ships
// with — a regression in init() registration would otherwise hide the
// tool from the surface even though the dispatcher case still works
// for a hardcoded call. We also assert the schema's
// additionalProperties:false flag is set so an agent that misnames a
// field gets a fast schema-shaped rejection.
func TestHandle_ToolsList_IncludesLastPane(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "last_pane" {
			schema, _ := def["inputSchema"].(map[string]any)
			if got, ok := schema["additionalProperties"].(bool); !ok || got {
				t.Errorf("last_pane schema additionalProperties = %v, want false", schema["additionalProperties"])
			}
			props, _ := schema["properties"].(map[string]any)
			for _, want := range []string{"target_window", "disable_input", "enable_input", "zoom_toggle"} {
				if _, ok := props[want]; !ok {
					t.Errorf("last_pane schema missing %q property: %v", want, props)
				}
			}
			return
		}
	}
	t.Fatalf("tools/list missing last_pane: %v", listing)
}

// readActivePaneID extracts the pane id (`#{pane_id}`) of the
// currently-active pane from a list_panes call result. Returns the
// empty string when no pane is marked active. Lives in this file so
// the suite stays self-contained — no other tests need this exact
// shape today.
func readActivePaneID(t *testing.T, res any) string {
	t.Helper()
	body := extractText(t, res)
	var obj struct {
		Panes []map[string]any `json:"panes"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_panes: %v\nbody=%s", err, body)
	}
	for _, p := range obj.Panes {
		active, _ := p["active"].(bool)
		if !active {
			continue
		}
		id, _ := p["id"].(string)
		return id
	}
	return ""
}
