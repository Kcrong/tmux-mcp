package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_DisplayMenu_ListedInTools confirms init() actually wired
// display_menu into tools/list. Without this pin a regression in
// init() would hide the surface from MCP clients even though the
// hardcoded dispatcher case still works.
func TestHandle_DisplayMenu_ListedInTools(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "display_menu" {
			return
		}
	}
	t.Fatalf("tools/list missing display_menu")
}

// TestHandle_DisplayMenu_SchemaShape pins the load-bearing schema
// invariants: items is required and minItems:1, the per-item schema
// also bans extra properties, and `additionalProperties:false` is
// locked at the top level so a typoed field gets a fast schema-shaped
// rejection.
func TestHandle_DisplayMenu_SchemaShape(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "display_menu" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("display_menu schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		req, ok := schema["required"].([]string)
		if !ok || len(req) != 1 || req[0] != "items" {
			t.Fatalf("display_menu schema required = %v, want [items]", schema["required"])
		}
		props, _ := schema["properties"].(map[string]any)
		items, _ := props["items"].(map[string]any)
		if items["minItems"] != 1 {
			t.Errorf("display_menu items.minItems = %v, want 1", items["minItems"])
		}
		// Per-item schema must also be strict — verify it.
		itemSchema, _ := items["items"].(map[string]any)
		ip, ok := itemSchema["additionalProperties"].(bool)
		if !ok || ip {
			t.Errorf("display_menu item schema additionalProperties = %v, want false", itemSchema["additionalProperties"])
		}
		ireq, _ := itemSchema["required"].([]string)
		if len(ireq) != 1 || ireq[0] != "name" {
			t.Errorf("display_menu item schema required = %v, want [name]", itemSchema["required"])
		}
		// Top-level properties present.
		want := []string{
			"target_pane", "target_client", "title", "border_lines",
			"border_style", "selected_style", "starting_choice", "x", "y",
			"no_callbacks", "items",
		}
		for _, n := range want {
			if _, ok := props[n]; !ok {
				t.Errorf("display_menu.properties missing %q", n)
			}
		}
		return
	}
	t.Fatalf("tools/list missing display_menu")
}

// TestHandle_DisplayMenu_NotInReadOnlyAllowlist guards the
// classification: display_menu mutates UI state on the client and
// must not be reachable from a -read-only server.
func TestHandle_DisplayMenu_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("display_menu") {
		t.Fatal("IsReadOnlyTool(display_menu) = true, want false (it draws on the client)")
	}
}

// TestHandle_DisplayMenu_RejectsEmptyItems pins the boundary's
// up-front guard that mirrors the schema's minItems:1 contract: a
// payload with `items: []` must surface InvalidParams before reaching
// the controller, so the JSON-RPC error code is stable for clients
// that branch on it.
func TestHandle_DisplayMenu_RejectsEmptyItems(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "display_menu",
		"arguments": map[string]any{"items": []any{}},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for empty items")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "items") {
		t.Errorf("message = %q, want substring 'items'", rerr.Message)
	}
}

// TestHandle_DisplayMenu_RejectsItemMissingName pins the per-item
// guard at the boundary: a row whose name is empty would render as a
// no-op separator the user cannot navigate to, so the dispatcher
// refuses the shape with InvalidParams.
func TestHandle_DisplayMenu_RejectsItemMissingName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "display_menu",
		"arguments": map[string]any{
			"items": []map[string]any{
				{"name": "Run", "key": "r", "command": "display ok"},
				{"name": "", "key": "", "command": ""},
			},
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for empty item name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "items[1].name is required") {
		t.Errorf("message = %q, want substring 'items[1].name is required'", rerr.Message)
	}
}

// TestHandle_DisplayMenu_RejectsBadClient pins the validateClientRef
// hookup: a target_client value that fails the shared regex must be
// surfaced as InvalidParams — and the message must be re-prefixed
// from "show_messages:" to "display_menu:" so callers see a
// consistent surface.
func TestHandle_DisplayMenu_RejectsBadClient(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "display_menu",
		"arguments": map[string]any{
			"target_client": "bad client with spaces",
			"items":         []map[string]any{{"name": "X", "key": "x", "command": "display ok"}},
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for bad target_client")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if strings.Contains(rerr.Message, "show_messages") {
		t.Errorf("message %q still mentions show_messages; expected re-prefix to display_menu", rerr.Message)
	}
}

// TestHandle_DisplayMenu_HeadlessMapsCode pins the wire contract for
// the headless server case: the controller's tmux server has no
// client attached, so display-menu's "no current client" diagnostic
// must surface the standard CodeInternal (we deliberately do NOT wrap
// the headless shape into ErrSessionNotFound — the agent may legit
// retry by passing target_client). The test asserts the failure is
// *some* mapped code, not a panic, which is the load-bearing
// invariant.
func TestHandle_DisplayMenu_HeadlessMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we hit the "server up, no clients"
	// branch rather than the entirely-different "no server running"
	// stderr shape.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "dm_headless_anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "display_menu",
		"arguments": map[string]any{
			"items": []map[string]any{{"name": "Quit", "key": "q", "command": `display "bye"`}},
		},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error, got result %#v", res)
	}
	// Either CodeInternal (forwarded "no current client") or
	// CodeSessionNotFound (wrapped "can't find client" path) is
	// acceptable; the load-bearing invariant is that we mapped the
	// error rather than crashing.
	if rerr.Code != errs.CodeInternal && rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeInternal (%d) or CodeSessionNotFound (%d), msg=%q",
			rerr.Code, errs.CodeInternal, errs.CodeSessionNotFound, rerr.Message)
	}
}

// TestHandle_DisplayMenu_RejectsOversizedTitle pins the length cap on
// the title field. tmux happily parses long titles but a 10kB title
// is almost certainly a typo or hostile caller; the boundary must
// surface a typed error before tmux gets the chance to allocate a
// frame for it.
func TestHandle_DisplayMenu_RejectsOversizedTitle(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	huge := strings.Repeat("x", maxDisplayMenuTitleLen+1)
	params := mustJSON(t, map[string]any{
		"name": "display_menu",
		"arguments": map[string]any{
			"title": huge,
			"items": []map[string]any{{"name": "Run", "key": "r", "command": "display ok"}},
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for oversized title")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "exceeds") {
		t.Errorf("message = %q, want substring 'exceeds'", rerr.Message)
	}
}

// TestHandle_DisplayMenu_RejectsBadPosition pins the validator for
// the X/Y position fields against representative bad shapes. tmux
// would otherwise either accept the value (and render in an
// unexpected place) or fail with a parse error the boundary should
// have caught first. Subtests give per-case isolation so a regression
// in one branch is named precisely.
func TestHandle_DisplayMenu_RejectsBadPosition(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	cases := []struct {
		name, field, value string
	}{
		{"x_with_quote", "x", `5"`},
		{"y_with_space", "y", "5 5"},
		{"x_unknown_letter", "x", "Z"},
		{"y_unknown_letter", "y", "X"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tools := newTools(t)
			params := mustJSON(t, map[string]any{
				"name": "display_menu",
				"arguments": map[string]any{
					tc.field: tc.value,
					"items":  []map[string]any{{"name": "X", "key": "x", "command": "display ok"}},
				},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params for %s=%q", tc.field, tc.value)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_DisplayMenu_AcceptsValidPosition is the inverse of the
// reject test: every documented position shape (integer, magic
// letter, format) must be accepted by the validator. We drive the
// call against a headless tmux so it fails at the "no current
// client" step, not at validation — the test asserts validation
// passed by checking the error code is NOT CodeInvalidParams.
func TestHandle_DisplayMenu_AcceptsValidPosition(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	cases := []struct {
		name, value string
	}{
		{"integer", "10"},
		{"center", "C"},
		{"right", "R"},
		{"format", "#{popup_centre_x}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tools := newTools(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			callTool(t, tools, ctx, "session_create", map[string]any{
				"name": "dm_pos_" + tc.name, "command": "/bin/sh",
			})
			params := mustJSON(t, map[string]any{
				"name": "display_menu",
				"arguments": map[string]any{
					"x":     tc.value,
					"y":     tc.value,
					"items": []map[string]any{{"name": "X", "key": "x", "command": "display ok"}},
				},
			})
			_, rerr := tools.Handle(ctx, "tools/call", params)
			if rerr == nil {
				return // unexpected but not a regression
			}
			if rerr.Code == errs.CodeInvalidParams {
				t.Fatalf("validation rejected valid position %q: %s", tc.value, rerr.Message)
			}
		})
	}
}

// TestHandle_DisplayMenu_RejectsMissingItems pins the contract that a
// payload with no `items` key at all surfaces InvalidParams. The
// schema declares `required: [items]` but Go's typed unmarshal would
// silently leave the slice nil; the handler must catch that.
func TestHandle_DisplayMenu_RejectsMissingItems(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "display_menu",
		"arguments": map[string]any{"title": "Menu"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for missing items")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_DisplayMenu_RejectsOmittedArguments pins the no-arguments
// path: display_menu requires items, so a tools/call with no
// arguments key at all must surface InvalidParams (vs. choose_client
// which permits `arguments: {}`).
func TestHandle_DisplayMenu_RejectsOmittedArguments(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{"name": "display_menu"})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for omitted arguments (items is required)")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_DisplayMenu_RejectsMalformedJSON pins the up-front
// invariant that a malformed payload (non-object arguments) surfaces
// CodeInvalidParams from json.Unmarshal, not CodeInternal.
func TestHandle_DisplayMenu_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := json.RawMessage(`{"name":"display_menu","arguments":42}`)
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for non-object arguments")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_DisplayMenu_RejectsBadKey pins the per-item key
// validation: a key with embedded whitespace is almost certainly a
// typo and tmux's bind-key parser would reject it anyway. Catching
// it here gives a typed error.
func TestHandle_DisplayMenu_RejectsBadKey(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "display_menu",
		"arguments": map[string]any{
			"items": []map[string]any{{"name": "Run", "key": "r r", "command": "display ok"}},
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for key with whitespace")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_DisplayMenu_RejectsCommandWithNewline mirrors the
// no-newlines guard on the command field. tmux would split the row
// on the embedded newline and break the menu shape; the boundary
// rejects up front.
func TestHandle_DisplayMenu_RejectsCommandWithNewline(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "display_menu",
		"arguments": map[string]any{
			"items": []map[string]any{{"name": "Run", "key": "r", "command": "display\nok"}},
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for command with newline")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}
