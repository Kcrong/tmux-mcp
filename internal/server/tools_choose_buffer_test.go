package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_ChooseBuffer_HappyPath_EntersBufferMode is the load-bearing
// end-to-end check: a tools/call into choose_buffer with a valid target
// must put the pane into tmux's buffer-mode picker. We verify that
// post-call by chaining a display_message tools/call against
// `#{?pane_in_mode,1,0}` and `#{pane_mode}` — the first must read "1",
// the second must contain "buffer-mode". Without this pin a refactor
// that swaps tmux flags (e.g. "choose-tree" instead of "choose-buffer")
// would still compile, but the chooser would land in the wrong mode.
func TestHandle_ChooseBuffer_HappyPath_EntersBufferMode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cb1", "command": "/bin/sh",
	})
	callTool(t, tools, ctx, "set_buffer", map[string]any{
		"data": "hello-from-test",
	})

	body := extractText(t, callTool(t, tools, ctx, "choose_buffer", map[string]any{
		"target": "cb1",
	}))
	var obj struct {
		Entered bool `json:"entered"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode choose_buffer: %v\nbody=%s", err, body)
	}
	if !obj.Entered {
		t.Fatalf("entered = false, want true; body=%s", body)
	}

	in := extractText(t, callTool(t, tools, ctx, "display_message", map[string]any{
		"format": "#{?pane_in_mode,1,0}", "session": "cb1",
	}))
	var pim struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(in), &pim); err != nil {
		t.Fatalf("decode display_message(pane_in_mode): %v\nbody=%s", err, in)
	}
	if pim.Value != "1" {
		t.Fatalf("pane_in_mode = %q, want 1 — choose_buffer must put the pane into a mode", pim.Value)
	}
	mode := extractText(t, callTool(t, tools, ctx, "display_message", map[string]any{
		"format": "#{pane_mode}", "session": "cb1",
	}))
	var pm struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(mode), &pm); err != nil {
		t.Fatalf("decode display_message(pane_mode): %v\nbody=%s", err, mode)
	}
	if !strings.Contains(pm.Value, "buffer-mode") {
		t.Fatalf("pane_mode = %q, want it to mention buffer-mode", pm.Value)
	}
}

// TestHandle_ChooseBuffer_AcceptsNullArguments guards the "raw is empty"
// branch — the dispatcher hands choose_buffer a nil-ish payload when
// the caller sends `arguments: {}` (or omits the field entirely). The
// handler must accept it and exercise the "no flags emitted" path
// rather than rejecting it as malformed.
func TestHandle_ChooseBuffer_AcceptsNullArguments(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cbnull", "command": "/bin/sh",
	})

	// Construct params manually so we can omit the "arguments" key
	// entirely — that's the path that exercises the len(raw) == 0
	// branch in the handler.
	params := mustJSON(t, map[string]any{"name": "choose_buffer"})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("choose_buffer (no arguments): %s", rerr.Message)
	}
	body := extractText(t, res)
	if !strings.Contains(body, `"entered":true`) {
		t.Fatalf("expected entered=true on null-arguments call, got %s", body)
	}
}

// TestHandle_ChooseBuffer_MissingTargetMapsCode pins the wire contract
// that asking for a non-existent pane surfaces CodeSessionNotFound
// rather than a generic internal-error code, mirroring choose_tree /
// list_windows / session_kill / clear_history.
func TestHandle_ChooseBuffer_MissingTargetMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cbanchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "choose_buffer",
		"arguments": map[string]any{
			"target": "ghost_session_xyzzy",
		},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error for missing target, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d), msg=%q",
			rerr.Code, errs.CodeSessionNotFound, rerr.Message)
	}
}

// TestHandle_ChooseBuffer_RejectsBadTarget guards the regex/length
// policy on the optional `target` argument. A typo / shell metachar
// must be refused with CodeInvalidParams up front before tmux is
// consulted — same contract every other pane-targeting tool upholds.
func TestHandle_ChooseBuffer_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "choose_buffer",
		"arguments": map[string]any{
			"target": "bad target with spaces",
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

// TestHandle_ChooseBuffer_RejectsNewlineInFormat pins the up-front
// guard on the format-style strings: a literal newline in `format`
// would split the JSON-RPC frame budget and confuse tmux's chooser
// renderer, so we refuse it with CodeInvalidParams before any tmux
// call runs. Same guard fires for `filter` and `template` — picking
// `format` here keeps the test focused on the regex error path.
func TestHandle_ChooseBuffer_RejectsNewlineInFormat(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "choose_buffer",
		"arguments": map[string]any{
			"format": "#{buffer_name}\n#{buffer_size}",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for embedded newline in format")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ChooseBuffer_RejectsBadSortOrder pins the sort-order
// whitelist. A value outside {time, name, size} must be refused with
// CodeInvalidParams before tmux is consulted — even though the schema
// enum already filters this surface, the defensive switch in the
// handler must reject hand-crafted calls that bypass schema
// validation, so a regression where the validator drifts from the
// schema is loud.
func TestHandle_ChooseBuffer_RejectsBadSortOrder(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "choose_buffer",
		"arguments": map[string]any{
			"sort_order": "totally-not-a-mode",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for unknown sort_order")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ChooseBuffer_RejectsBadKeyFormat guards the conservative
// alnum / dot / dash / underscore class on the optional `key_format`
// argument. Whitespace or shell metachars would never resolve to a
// valid tmux key descriptor anyway, so refusing them up front keeps
// the failure close to the operator's typo instead of leaking a
// confusing tmux stderr per call.
func TestHandle_ChooseBuffer_RejectsBadKeyFormat(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "choose_buffer",
		"arguments": map[string]any{
			"key_format": "C-c with spaces",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad key_format")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ToolsList_IncludesChooseBuffer ensures tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. A regression in init() registration would
// otherwise hide the tool from the surface even though the
// dispatcher case still works for a hardcoded call. We also pin the
// schema invariants the description promises:
// additionalProperties:false (no silent typos), the sort_order enum
// is exactly the {time, name, size} triple, and every documented
// field is exposed.
func TestHandle_ToolsList_IncludesChooseBuffer(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "choose_buffer" {
			schema, _ := def["inputSchema"].(map[string]any)
			if got, ok := schema["additionalProperties"].(bool); !ok || got {
				t.Errorf("choose_buffer schema additionalProperties = %v, want false",
					schema["additionalProperties"])
			}
			props, _ := schema["properties"].(map[string]any)
			for _, want := range []string{
				"target", "format", "filter", "key_format", "sort_order",
				"template", "no_preview", "zoom", "reverse",
			} {
				if _, ok := props[want].(map[string]any); !ok {
					t.Errorf("choose_buffer schema missing property %q: %v", want, props)
				}
			}
			sortOrder, _ := props["sort_order"].(map[string]any)
			enum, _ := sortOrder["enum"].([]string)
			if len(enum) != 3 {
				t.Errorf("choose_buffer sort_order.enum = %v, want 3 entries", sortOrder["enum"])
			}
			return
		}
	}
	t.Fatalf("tools/list missing choose_buffer: %v", listing)
}

// TestHandle_ChooseBuffer_FullFlagPayload rounds out the matrix:
// firing a tools/call with every documented field set (target,
// format, filter, key_format, sort_order, template, plus all three
// boolean toggles) must succeed end-to-end. This catches argv-builder
// regressions where adding a new flag or changing the order between
// the controller and tools.go silently breaks for clients that
// supply the full payload.
func TestHandle_ChooseBuffer_FullFlagPayload(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cbfull", "command": "/bin/sh",
	})
	callTool(t, tools, ctx, "set_buffer", map[string]any{
		"data": "row-1",
	})

	body := extractText(t, callTool(t, tools, ctx, "choose_buffer", map[string]any{
		"target":     "cbfull",
		"format":     "#{buffer_name}",
		"filter":     "#{>=:#{buffer_size},1}",
		"key_format": "Q",
		"sort_order": "time",
		"template":   "display-message picked",
		"no_preview": true,
		"zoom":       true,
		"reverse":    true,
	}))
	if !strings.Contains(body, `"entered":true`) {
		t.Fatalf("expected entered=true after full-flag call, got %s", body)
	}
}
