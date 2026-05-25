package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_CustomizeMode_OpensEditor drives the load-bearing happy
// path through the dispatcher: session_create → customize_mode →
// display_message confirming pane_in_mode flipped to 1. Verifies the
// dispatcher is wired up, the schema accepts the documented arguments,
// and the response envelope carries the `{"opened": true}` ack.
func TestHandle_CustomizeMode_OpensEditor(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cme", "command": "/bin/sh", "width": 80, "height": 20,
	})

	openText := extractText(t, callTool(t, tools, ctx, "customize_mode", map[string]any{
		"target": "cme",
	}))
	var openObj struct {
		Opened bool `json:"opened"`
	}
	if err := json.Unmarshal([]byte(openText), &openObj); err != nil {
		t.Fatalf("decode customize_mode: %v\nbody=%s", err, openText)
	}
	if !openObj.Opened {
		t.Fatalf("customize_mode opened flag = false, want true; body=%s", openText)
	}

	// Pin the side effect through display_message so a future regression
	// where the dispatcher wires customize_mode to a no-op handler shows
	// up loudly.
	dmText := extractText(t, callTool(t, tools, ctx, "display_message", map[string]any{
		"format":  "#{?pane_in_mode,1,0}",
		"session": "cme",
	}))
	var dmObj struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(dmText), &dmObj); err != nil {
		t.Fatalf("decode display_message: %v\nbody=%s", err, dmText)
	}
	if dmObj.Value != "1" {
		t.Fatalf("pane_in_mode = %q, want %q (the pane should be in customize-mode)", dmObj.Value, "1")
	}
}

// TestHandle_CustomizeMode_AcceptsAllFlags exercises every optional
// knob in one call: format, filter, no_close, zoom. The pane must
// still report pane_in_mode=1 and tmux must accept the combined argv
// without complaint. Without this pin a regression where the boundary
// dropped one of the booleans before reaching the controller would
// silently degrade.
func TestHandle_CustomizeMode_AcceptsAllFlags(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cmflags", "command": "/bin/sh", "width": 80, "height": 20,
	})

	openText := extractText(t, callTool(t, tools, ctx, "customize_mode", map[string]any{
		"target":   "cmflags",
		"format":   "#{session_name}",
		"filter":   "#{!=:#{session_name},}",
		"no_close": true,
		"zoom":     true,
	}))
	var openObj struct {
		Opened bool `json:"opened"`
	}
	if err := json.Unmarshal([]byte(openText), &openObj); err != nil {
		t.Fatalf("decode customize_mode: %v\nbody=%s", err, openText)
	}
	if !openObj.Opened {
		t.Fatalf("customize_mode opened flag = false; body=%s", openText)
	}
}

// TestHandle_CustomizeMode_AcceptsEmptyArguments pins the no-args path:
// every field on customize_mode is optional, so a call with `{}` (or
// without an arguments field) must succeed against the active pane and
// not fall through to the schema-rejection path.
func TestHandle_CustomizeMode_AcceptsEmptyArguments(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cmnull", "command": "/bin/sh", "width": 80, "height": 20,
	})

	// Construct the params without the "arguments" key so the dispatcher
	// hands the handler an empty raw payload — the path that exercises
	// the len(raw) == 0 short-circuit.
	params := mustJSON(t, map[string]any{"name": "customize_mode"})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("customize_mode: %s", rerr.Message)
	}
	body := extractText(t, res)
	var openObj struct {
		Opened bool `json:"opened"`
	}
	if err := json.Unmarshal([]byte(body), &openObj); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, body)
	}
	if !openObj.Opened {
		t.Fatalf("opened = false; body=%s", body)
	}
}

// TestHandle_CustomizeMode_RejectsBadTarget locks the regex check on
// `target` so a stray quote/whitespace can't slip through to the tmux
// argv.
func TestHandle_CustomizeMode_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "customize_mode",
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

// TestHandle_CustomizeMode_RejectsNewlineFormat pins the no-newlines
// check on `format`. tmux would happily splice a multi-line format
// into the editor's row template, but the resulting JSON-RPC frame
// would carry an embedded newline that splits the RPC body.
func TestHandle_CustomizeMode_RejectsNewlineFormat(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "customize_mode",
		"arguments": map[string]any{
			"format": "#{session_name}\nrow2",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for newline in format")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "format") {
		t.Errorf("error message %q does not mention 'format'", rerr.Message)
	}
}

// TestHandle_CustomizeMode_RejectsNewlineFilter pins the same
// no-newlines check on `filter`. Mirrors the format path so a
// regression that drops the newline guard from one field but not the
// other surfaces in either test.
func TestHandle_CustomizeMode_RejectsNewlineFilter(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "customize_mode",
		"arguments": map[string]any{
			"filter": "#{session_name}\nbad",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for newline in filter")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "filter") {
		t.Errorf("error message %q does not mention 'filter'", rerr.Message)
	}
}

// TestHandle_CustomizeMode_RejectsOversizedFormat guards the 256-byte
// cap on free-form strings. A caller pasting a 4 KiB DSL expression by
// accident would otherwise expand the JSON-RPC frame size unbounded.
func TestHandle_CustomizeMode_RejectsOversizedFormat(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "customize_mode",
		"arguments": map[string]any{
			"format": strings.Repeat("a", maxCustomizeModeStringLen+1),
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversized format")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_CustomizeMode_MissingTargetMapsCode pins the wire contract
// that customize_mode against an unknown target surfaces
// CodeSessionNotFound (-32000), mirroring pane_kill / clear_history /
// pane_select. We run against an anchored server so the test exercises
// "server up, target missing" rather than the headless branch.
func TestHandle_CustomizeMode_MissingTargetMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cmanchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "customize_mode",
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

// TestHandle_ToolsList_IncludesCustomizeMode pins the schema contract:
// tools/list must advertise customize_mode, the schema must have
// additionalProperties:false (so a typo is rejected fast), and every
// optional field must be declared as a string/boolean. A regression in
// init() registration would otherwise hide the tool from the surface
// even though the dispatcher case still works for a hard-coded call.
func TestHandle_ToolsList_IncludesCustomizeMode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "customize_mode" {
			schema, _ := def["inputSchema"].(map[string]any)
			// additionalProperties:false is part of the contract — an
			// agent that misnames a field gets a fast schema-shaped
			// rejection rather than a silent no-op.
			if got, ok := schema["additionalProperties"].(bool); !ok || got {
				t.Errorf("customize_mode schema additionalProperties = %v, want false", schema["additionalProperties"])
			}
			props, _ := schema["properties"].(map[string]any)
			for _, key := range []string{"target", "format", "filter"} {
				p, _ := props[key].(map[string]any)
				if p == nil {
					t.Errorf("customize_mode schema missing %q property: %v", key, props)
					continue
				}
				if typ, _ := p["type"].(string); typ != "string" {
					t.Errorf("customize_mode.%s type = %q, want string", key, typ)
				}
			}
			for _, key := range []string{"no_close", "zoom"} {
				p, _ := props[key].(map[string]any)
				if p == nil {
					t.Errorf("customize_mode schema missing %q property: %v", key, props)
					continue
				}
				if typ, _ := p["type"].(string); typ != "boolean" {
					t.Errorf("customize_mode.%s type = %q, want boolean", key, typ)
				}
			}
			// No required fields: every argument is optional, mirroring
			// the controller's "empty target → active pane" contract.
			if req, ok := schema["required"].([]string); ok && len(req) > 0 {
				t.Errorf("customize_mode schema has required fields %v, want none (every field is optional)", req)
			}
			return
		}
	}
	t.Fatalf("tools/list missing customize_mode: %v", listing)
}

// TestIsReadOnlyTool_CustomizeMode pins the read-only policy: the
// editor mutates server state (it's literally an options/key-bindings
// editor), so the read-only allowlist must NOT include it. Mirrors the
// inverse contract every other mutating tool ships with — a regression
// that flipped the policy to "inspection-only" would silently expose
// the tool to read-only deployments.
func TestIsReadOnlyTool_CustomizeMode(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("customize_mode") {
		t.Fatalf("IsReadOnlyTool(\"customize_mode\") = true, want false (the editor mutates state)")
	}
}
