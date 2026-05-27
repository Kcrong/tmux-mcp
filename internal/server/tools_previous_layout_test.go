package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// seedSessionWithSplitsForLayout creates a session, splits its first
// window twice (so the layout has three panes the presets can
// reshape), and registers a session_kill cleanup. Returns the
// "session:window" target subsequent previous_layout calls hand back
// to tmux. tmux refuses most preset transitions on a single-pane
// window because the dump shape doesn't change, so the multi-pane
// prologue is load-bearing for the assertions.
func seedSessionWithSplitsForLayout(t *testing.T, tools *Tools, ctx context.Context, name string) string {
	t.Helper()
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": name, "command": "/bin/sh", "width": 120, "height": 40,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": name}}))
	})
	callTool(t, tools, ctx, "pane_split", map[string]any{
		"session": name, "direction": "vertical", "detach": true,
	})
	callTool(t, tools, ctx, "pane_split", map[string]any{
		"session": name, "direction": "horizontal", "detach": true,
	})
	return name + ":0"
}

// TestHandle_PreviousLayout_CyclesReturnsAck drives the happy path:
// previous_layout against a freshly-split window must come back with
// the documented `{"cycled":true}` JSON ack. Catches the dispatcher
// wiring (case "previous_layout":) and the controller's argv
// translation in one shot.
func TestHandle_PreviousLayout_CyclesReturnsAck(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	target := seedSessionWithSplitsForLayout(t, tools, ctx, "plh")

	got := extractText(t, callTool(t, tools, ctx, "previous_layout", map[string]any{
		"target": target,
	}))
	if !strings.Contains(got, `"cycled":true`) {
		t.Fatalf("previous_layout text = %q, want it to contain 'cycled:true'", got)
	}
}

// TestHandle_PreviousLayout_RejectsBadTarget guards the regex policy
// on the `target` argument. Each case is a single bad shape so the
// failure message names which validator regressed.
func TestHandle_PreviousLayout_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []struct {
		name   string
		target string
	}{
		{"empty target", ""},
		{"target without colon", "demo"},
		{"bad target session", "bad name:0"},
		{"bad target window", "demo:bad name"},
		{"empty window half", "demo:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name":      "previous_layout",
				"arguments": map[string]any{"target": tc.target},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params for %s", tc.name)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_PreviousLayout_RejectsUnknownProperty pins the schema's
// `additionalProperties:false` contract from the spec-client side.
// The handler decodes into a typed struct so extra fields are dropped
// at the language boundary (matching every other tool in this server),
// but a spec-driven MCP client validates against the published schema
// and surfaces -32602 to the caller. This test asserts the schema
// itself locks the surface so the contract documented to clients
// stays in effect — and pins that the only allowed key on
// `arguments` is `target`.
func TestHandle_PreviousLayout_RejectsUnknownProperty(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "previous_layout" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("previous_layout schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		props, _ := schema["properties"].(map[string]any)
		if _, hasTarget := props["target"]; !hasTarget {
			t.Errorf("previous_layout schema missing `target` property")
		}
		if len(props) != 1 {
			t.Errorf("previous_layout schema has %d properties, want exactly 1 (target)", len(props))
		}
		return
	}
	t.Fatal("tools/list missing \"previous_layout\"")
}

// TestHandle_PreviousLayout_MissingSessionMapsCode pins the
// CodeSessionNotFound contract: previous_layout against an unknown
// session must surface -32000, mirroring select_layout /
// window_select / swap_window.
func TestHandle_PreviousLayout_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor session so tmux's server is up — without it, the missing
	// target produces "no server running" and lands on a different
	// branch from the one we want to pin.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "anchor_pl_srv", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name":      "previous_layout",
		"arguments": map[string]any{"target": "definitely_does_not_exist_xyzzy:0"},
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

// TestHandle_PreviousLayout_NotInReadOnlyAllowlist pins the policy
// decision: previous_layout mutates tmux state (it changes a
// window's pane arrangement) so it must be rejected when the
// operator has armed -read-only. Without this guard a future
// contributor adding the name to the read-only allowlist would
// silently widen the inspection surface.
func TestHandle_PreviousLayout_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("previous_layout") {
		t.Fatal("IsReadOnlyTool(\"previous_layout\") = true, want false (mutating tools must not be inspection-allowed)")
	}
}

// TestHandle_ToolsList_IncludesPreviousLayout verifies the dispatch
// surface advertises the new tool so MCP clients can discover it via
// tools/list. We also pin `additionalProperties: false` on the
// schema because the contract documents that any field other than
// `target` is rejected up front.
func TestHandle_ToolsList_IncludesPreviousLayout(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "previous_layout" {
			schema, _ := def["inputSchema"].(map[string]any)
			if got, ok := schema["additionalProperties"].(bool); !ok || got {
				t.Errorf("previous_layout schema additionalProperties = %v, want false", schema["additionalProperties"])
			}
			required, _ := schema["required"].([]string)
			if len(required) != 1 || required[0] != "target" {
				t.Errorf("previous_layout required = %v, want [target]", required)
			}
			return
		}
	}
	t.Fatal("tools/list missing \"previous_layout\"")
}
