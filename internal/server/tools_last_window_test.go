package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_LastWindow_TogglesActiveFlag is the load-bearing
// integration: with three named windows in a session and the active
// flag walked through "first" → "second", a `last_window` call must
// flip the active flag back to "first". A buggy implementation that
// just decremented the index would land on "first" by coincidence
// going from index 2 to 1, so we verify by name.
func TestHandle_LastWindow_TogglesActiveFlag(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lwh", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "lwh"}}))
	})

	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "lwh", "name": "first", "command": "/bin/sh", "select": false,
	})
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "lwh", "name": "second", "command": "/bin/sh", "select": false,
	})

	// Walk the active flag through "first" → "second" so tmux records
	// "first" as the previous slot.
	callTool(t, tools, ctx, "window_select", map[string]any{
		"session": "lwh", "target": "first",
	})
	callTool(t, tools, ctx, "window_select", map[string]any{
		"session": "lwh", "target": "second",
	})

	// Sanity-check the baseline so the post-call assertion is
	// meaningful — without it a no-op last_window would still pass.
	body := extractText(t, callTool(t, tools, ctx,
		"list_windows", map[string]any{"session": "lwh"}))
	var pre struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(body), &pre); err != nil {
		t.Fatalf("decode list_windows pre: %v\nbody=%s", err, body)
	}
	preActive := ""
	for _, w := range pre.Windows {
		if active, _ := w["active"].(bool); active {
			preActive, _ = w["name"].(string)
		}
	}
	if preActive != "second" {
		t.Fatalf("baseline: expected active 'second', got %q (body=%s)", preActive, body)
	}

	got := extractText(t, callTool(t, tools, ctx, "last_window", map[string]any{
		"target": "lwh",
	}))
	if got != "ok" {
		t.Fatalf("last_window text = %q, want \"ok\"", got)
	}

	body = extractText(t, callTool(t, tools, ctx,
		"list_windows", map[string]any{"session": "lwh"}))
	var post struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(body), &post); err != nil {
		t.Fatalf("decode list_windows post: %v\nbody=%s", err, body)
	}
	postActive := ""
	for _, w := range post.Windows {
		if active, _ := w["active"].(bool); active {
			postActive, _ = w["name"].(string)
		}
	}
	if postActive != "first" {
		t.Fatalf("last_window did not toggle: active = %q, want %q (body=%s)",
			postActive, "first", body)
	}
}

// TestHandle_LastWindow_RejectsEmptyTarget pins the up-front guard:
// `target` is required, and a missing/empty value must be rejected
// with CodeInvalidParams before any tmux command runs (otherwise tmux
// would fall back to the current attached client, which is rarely
// what the caller meant).
func TestHandle_LastWindow_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "last_window",
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

// TestHandle_LastWindow_RejectsBadTarget guards the regex policy on
// the `target` argument: a string with shell metacharacters must be
// refused with CodeInvalidParams up front rather than letting it
// reach the tmux argv.
func TestHandle_LastWindow_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "last_window",
		"arguments": map[string]any{"target": "demo; rm -rf /"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_LastWindow_MissingSessionMapsCode pins the wire contract:
// last_window against an unknown session must surface
// CodeSessionNotFound (-32000), mirroring window_select / window_move /
// swap_window.
func TestHandle_LastWindow_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor a real session so the dispatcher hits the "server up,
	// session missing" branch — without it, tmux can emit "no server
	// running" which lands on a different code path.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "anchor", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "anchor"}}))
	})

	params := mustJSON(t, map[string]any{
		"name":      "last_window",
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

// TestHandle_LastWindow_RejectsUnknownField pins the
// additionalProperties:false contract — a typo like `session` (the
// natural reflex from window_select) must trip CodeInvalidParams up
// front rather than silently behave like a no-op against the default
// target.
func TestHandle_LastWindow_RejectsUnknownField(t *testing.T) {
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
		if name != "last_window" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		// additionalProperties:false is part of the contract — an agent
		// that misnames a field gets a fast schema-shaped rejection
		// rather than a silent no-op.
		if got, ok := schema["additionalProperties"].(bool); !ok || got {
			t.Errorf("schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		req, _ := schema["required"].([]string)
		if len(req) != 1 || req[0] != "target" {
			t.Errorf("required = %v, want [target]", req)
		}
		return
	}
	t.Fatalf("tools/list missing 'last_window'")
}

// TestHandle_LastWindow_NotInReadOnlyAllowlist pins the policy: a
// last_window call mutates the focused window pointer, so it must
// stay rejected under -read-only. Without the assertion a future
// contributor could accidentally fold last_window into the
// inspection allowlist along with capture / list_* / wait_for_text.
func TestHandle_LastWindow_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("last_window") {
		t.Fatal("IsReadOnlyTool(\"last_window\") = true, want false (last_window mutates the active window pointer)")
	}
}
