package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_ChooseClient_ListedInTools confirms the init()-time
// registration actually wired choose_client into tools/list — a
// regression in init() would otherwise hide the tool from the surface
// even though the dispatcher case still works for a hardcoded call.
func TestHandle_ChooseClient_ListedInTools(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "choose_client" {
			return
		}
	}
	t.Fatalf("tools/list missing choose_client")
}

// TestHandle_ChooseClient_SchemaLocksAdditionalProperties pins the
// additionalProperties:false contract on the schema. A typo like
// "key-format" (dash) must get a fast schema-shaped rejection rather
// than silently behaving like the default-flag variant.
func TestHandle_ChooseClient_SchemaLocksAdditionalProperties(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "choose_client" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("choose_client schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		// `required` is intentionally absent — every argument is
		// optional. Confirm so a future contributor adding a required
		// arg is forced to update this test.
		if req, present := schema["required"]; present {
			t.Fatalf("choose_client schema required = %v, want absent (every arg optional)", req)
		}
		return
	}
	t.Fatalf("tools/list missing choose_client")
}

// TestHandle_ChooseClient_SchemaPropertyShape confirms the JSON Schema
// advertises every flag the tmux man page surfaces, so an MCP client
// driving the tool from tools/list does not have to guess which knobs
// are available. Locking the property names also catches a refactor
// that renames a field without updating the schema.
func TestHandle_ChooseClient_SchemaPropertyShape(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "choose_client" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		want := []string{
			"target", "format", "filter", "key_format",
			"sort_order", "template", "no_preview", "zoom", "reverse",
		}
		for _, n := range want {
			if _, ok := props[n]; !ok {
				t.Errorf("choose_client.properties missing %q (props=%v)", n, props)
			}
		}
		return
	}
	t.Fatalf("tools/list missing choose_client")
}

// TestHandle_ChooseClient_RejectsUnknownArg pins the dispatch-side
// behaviour for a typoed argument. Even though Go's typed unmarshal
// silently drops unknown fields, the test exercises the call path so
// a future strict-decode refactor is covered.
func TestHandle_ChooseClient_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "choose_client",
		"arguments": map[string]any{
			"target": "bad pane with spaces",
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

// TestHandle_ChooseClient_RejectsNewlineFormat enforces the up-front
// guard that every free-form format argument refuses an embedded
// newline. tmux would otherwise reflow the menu and break the
// schema's "single row per option" contract.
func TestHandle_ChooseClient_RejectsNewlineFormat(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	cases := []struct {
		field, value string
	}{
		{"format", "first\nsecond"},
		{"filter", "first\rsecond"},
		{"key_format", "first\nsecond"},
		{"sort_order", "first\nsecond"},
		{"template", "first\nsecond"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name":      "choose_client",
				"arguments": map[string]any{tc.field: tc.value},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params error for newline in %s", tc.field)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
			if !strings.Contains(rerr.Message, "must not contain newlines") {
				t.Errorf("message = %q, want substring %q", rerr.Message, "must not contain newlines")
			}
		})
	}
}

// TestHandle_ChooseClient_RejectsOversizedFormat pins the length cap on
// the free-form format arguments. tmux happily parses long format
// strings, but a 10kB filter is almost certainly a typo or hostile
// caller and the boundary should surface a typed error before tmux
// gets a chance to allocate a frame for it.
func TestHandle_ChooseClient_RejectsOversizedFormat(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	huge := strings.Repeat("x", maxChooseClientFormatLen+1)
	params := mustJSON(t, map[string]any{
		"name":      "choose_client",
		"arguments": map[string]any{"format": huge},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversized format")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "exceeds") {
		t.Errorf("message = %q, want substring %q", rerr.Message, "exceeds")
	}
}

// TestHandle_ChooseClient_HeadlessMapsCode pins the wire contract for
// the headless server case the controller refuses up front: the
// JSON-RPC error code must be CodeSessionNotFound (-32000) so MCP
// clients can branch on a stable code rather than parse the rejection
// message. The headless tmux servers tmux-mcp owns are the
// load-bearing case for this path — a chooser without a client
// attached has nowhere to render and would silently no-op without
// this guard.
func TestHandle_ChooseClient_HeadlessMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so the controller
	// hits the "server up, no clients" branch rather than the
	// entirely-different "no server running" stderr shape.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cc_headless_anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name":      "choose_client",
		"arguments": map[string]any{},
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

// TestHandle_ChooseClient_MissingTargetMapsCode pins the wire contract
// for an unknown target pane: the dispatcher must surface
// CodeSessionNotFound (-32000) regardless of which exact phrase tmux's
// `display-message` probe emitted ("can't find pane" vs "can't find
// session").
func TestHandle_ChooseClient_MissingTargetMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cc_missing_anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "choose_client",
		"arguments": map[string]any{
			"target": "%99999",
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

// TestHandle_ChooseClient_AcceptsOmittedArguments pins the
// "no arguments at all" path: the dispatcher hands the handler a
// nil-ish payload when the caller omits the arguments key entirely,
// and choose_client must accept that as "every flag at its default"
// rather than fail-fast on the missing field. We assert the call
// reaches the controller (and fails there with the expected headless
// sentinel) so a regression in the json.Unmarshal guard is loud.
func TestHandle_ChooseClient_AcceptsOmittedArguments(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cc_omit_anchor", "command": "/bin/sh",
	})

	// Construct params manually so we can omit the "arguments" key
	// entirely — that's the path that exercises the len(raw) == 0
	// branch in the handler.
	params := mustJSON(t, map[string]any{"name": "choose_client"})
	_, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatal("expected headless sentinel after omitted arguments")
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d) (handler must reach the controller, not fail on params)",
			rerr.Code, errs.CodeSessionNotFound)
	}
}

// TestHandle_ChooseClient_RejectsMalformedJSON pins the up-front guard
// that the handler's json.Unmarshal returns CodeInvalidParams on a
// genuinely malformed payload (as opposed to a missing one, which is
// allowed — see the AcceptsOmittedArguments test). Without this pin
// the JSON-RPC error code path could regress to CodeInternal and an
// agent's branching logic would treat parser failures as transient.
func TestHandle_ChooseClient_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	// Hand-craft a params blob that has a tools/call envelope but a
	// non-object "arguments". The handler must return CodeInvalidParams
	// before reaching the controller.
	params := json.RawMessage(`{"name":"choose_client","arguments":42}`)
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for non-object arguments")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}
