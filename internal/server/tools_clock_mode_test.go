package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_ClockMode_HappyPath drives the full path through the
// dispatcher: session_create → clock_mode against the new pane →
// display_message confirming pane_in_mode=1 / pane_mode=clock-mode.
// Verifies the dispatcher is wired up, the schema accepts the
// documented argument, and clock-mode actually took hold on the pane.
func TestHandle_ClockMode_HappyPath(t *testing.T) {
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
		"name": "cm", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "cm"}}))
	})

	ackText := extractText(t, call("clock_mode", map[string]any{"target": "cm"}))
	var ackObj struct {
		ClockMode bool `json:"clock_mode"`
	}
	if err := json.Unmarshal([]byte(ackText), &ackObj); err != nil {
		t.Fatalf("decode clock_mode ack: %v\nbody=%s", err, ackText)
	}
	if !ackObj.ClockMode {
		t.Fatalf("clock_mode ack flag = false, want true; body=%s", ackText)
	}

	// Confirm the pane is actually in clock-mode now. The format
	// `#{pane_in_mode}|#{pane_mode}` resolves to "1|clock-mode" once
	// clock-mode has taken hold; without this the handler could
	// silently no-op (e.g. argv-shape regression in the controller)
	// and the ack alone would not catch it.
	dmText := extractText(t, call("display_message", map[string]any{
		"format":  "#{pane_in_mode}|#{pane_mode}",
		"session": "cm",
	}))
	var dmObj struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(dmText), &dmObj); err != nil {
		t.Fatalf("decode display_message: %v\nbody=%s", err, dmText)
	}
	if dmObj.Value != "1|clock-mode" {
		t.Fatalf("pane mode after clock_mode = %q, want %q", dmObj.Value, "1|clock-mode")
	}
}

// TestHandle_ClockMode_MissingTargetMapsCode pins the wire contract
// that clock_mode against an unknown session surfaces
// CodeSessionNotFound (-32000), mirroring clear_history /
// pane_select.
func TestHandle_ClockMode_MissingTargetMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, pane
	// missing" rather than the headless branch (different stderr).
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "anchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name": "clock_mode",
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

// TestHandle_ClockMode_RejectsMalformedTarget locks the regex check
// on `target` so a stray quote / shell metachar / path-injection
// can't slip through to the tmux argv. The error code must be the
// JSON-RPC standard CodeInvalidParams (-32602) so a hostile caller
// sees a fast rejection before any tmux command runs.
func TestHandle_ClockMode_RejectsMalformedTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "clock_mode",
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

// TestHandle_ClockMode_SchemaPinsAdditionalPropertiesFalse keeps the
// JSON Schema honest: clock_mode's surface is locked to the single
// optional `target` field, and an unknown property must be rejected
// up-front rather than silently ignored. Without this pin a future
// contributor adding a sibling field would inadvertently relax the
// schema (defaulting to additionalProperties=true) and let typos
// through.
func TestHandle_ClockMode_SchemaPinsAdditionalPropertiesFalse(t *testing.T) {
	t.Parallel()
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "clock_mode" {
			schema, ok := def["inputSchema"].(map[string]any)
			if !ok {
				t.Fatalf("clock_mode inputSchema not a map: %#v", def["inputSchema"])
			}
			ap, ok := schema["additionalProperties"].(bool)
			if !ok {
				t.Fatalf("clock_mode additionalProperties not a bool: %#v", schema["additionalProperties"])
			}
			if ap {
				t.Fatalf("clock_mode additionalProperties = true, want false")
			}
			return
		}
	}
	t.Fatal("tools/list did not advertise clock_mode")
}

// TestHandle_ToolsList_IncludesClockMode makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint.
func TestHandle_ToolsList_IncludesClockMode(t *testing.T) {
	t.Parallel()
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "clock_mode" {
			if desc, _ := def["description"].(string); !strings.Contains(strings.ToLower(desc), "clock") {
				t.Fatalf("clock_mode description does not mention 'clock': %q", desc)
			}
			return
		}
	}
	t.Fatal("tools/list missing clock_mode")
}

// TestHandle_ClockMode_NotReadOnly pins the inverse of the
// inspection-only allowlist: clock_mode mutates the pane's display
// state (it takes over the pane until the next key arrives), so it
// must NOT be in the read-only allowlist. A future contributor who
// flipped the wrong policy would otherwise let a clock_mode call slip
// past -read-only into the dispatch path.
func TestHandle_ClockMode_NotReadOnly(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("clock_mode") {
		t.Fatal("IsReadOnlyTool(\"clock_mode\") = true, want false (clock_mode mutates pane display state)")
	}
}
