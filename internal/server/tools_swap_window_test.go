package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// listWindowNames pulls (index → name) pairs out of a list_windows
// response body. Centralising the decode keeps the swap_window tests
// short and focused on assertions about the layout flipping.
func listWindowNames(t *testing.T, body string) map[int]string {
	t.Helper()
	var obj struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_windows: %v\nbody=%s", err, body)
	}
	out := make(map[int]string, len(obj.Windows))
	for _, w := range obj.Windows {
		// JSON numbers decode as float64 — coerce explicitly so the test
		// is not sensitive to encoder choices.
		idx, _ := w["index"].(float64)
		name, _ := w["name"].(string)
		out[int(idx)] = name
	}
	return out
}

// TestHandle_SwapWindow_TradesIndices runs the happy path: a session
// with two named windows has them swapped via the boundary, and a
// follow-up list_windows reflects the new layout (the names attached to
// indices flipped). Catches the dispatcher wiring and the controller
// argv ordering in one shot.
func TestHandle_SwapWindow_TradesIndices(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "swh", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "swh"}}))
	})
	// Detached create so the active flag stays on index 0 — the swap
	// test cares about layout, not focus (no_select coverage lives in
	// the dedicated test below).
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "swh", "name": "second", "command": "/bin/sh", "select": false,
	})

	pre := listWindowNames(t, extractText(t, callTool(t, tools, ctx,
		"list_windows", map[string]any{"session": "swh"})))
	if pre[1] != "second" {
		t.Fatalf("baseline: expected window 1 named %q, got %v", "second", pre)
	}

	got := extractText(t, callTool(t, tools, ctx, "swap_window", map[string]any{
		"session": "swh", "src": "0", "dst": "1", "no_select": true,
	}))
	if !strings.Contains(got, `"swapped":true`) {
		t.Fatalf("swap_window text = %q, want it to contain 'swapped:true'", got)
	}

	post := listWindowNames(t, extractText(t, callTool(t, tools, ctx,
		"list_windows", map[string]any{"session": "swh"})))
	if post[0] != "second" {
		t.Errorf("post-swap: index 0 = %q, want %q (full=%v)", post[0], "second", post)
	}
	if post[1] != pre[0] {
		t.Errorf("post-swap: index 1 = %q, want %q (full=%v)", post[1], pre[0], post)
	}
}

// TestHandle_SwapWindow_NoSelectDefaultsFalse pins the documented
// default for no_select. With the field omitted the schema declares a
// false default, so a chained list_windows after the swap must still
// see the active window flag follow tmux's interactive behaviour. The
// assertion is intentionally loose — the test only proves that omitting
// the field is accepted and the call returns a normal ack — because
// tmux's exact "follow active across the swap" semantics are version-
// sensitive enough that locking them in here would be brittle.
func TestHandle_SwapWindow_NoSelectDefaultsFalse(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "swd", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "swd"}}))
	})
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "swd", "name": "second", "command": "/bin/sh", "select": false,
	})

	got := extractText(t, callTool(t, tools, ctx, "swap_window", map[string]any{
		"session": "swd", "src": "0", "dst": "1",
	}))
	if !strings.Contains(got, `"swapped":true`) {
		t.Fatalf("swap_window text = %q, want it to contain 'swapped:true'", got)
	}
}

// TestHandle_SwapWindow_RejectsSameSrcDst pins the up-front "src and
// dst must differ" guard. Letting tmux be the one to refuse a no-op
// swap would emit a less informative error than the boundary's own
// CodeInvalidParams response.
func TestHandle_SwapWindow_RejectsSameSrcDst(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "swap_window",
		"arguments": map[string]any{"session": "demo", "src": "0", "dst": "0"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for src==dst")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "must differ") {
		t.Errorf("error message %q should reference the difference requirement", rerr.Message)
	}
}

// TestHandle_SwapWindow_RejectsEmptyArgs locks the up-front empty-
// string guards so the dispatcher never builds a partial tmux target.
func TestHandle_SwapWindow_RejectsEmptyArgs(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []struct {
		name string
		args map[string]any
	}{
		{"empty session", map[string]any{"session": "", "src": "0", "dst": "1"}},
		{"empty src", map[string]any{"session": "demo", "src": "", "dst": "1"}},
		{"empty dst", map[string]any{"session": "demo", "src": "0", "dst": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "swap_window", "arguments": tc.args,
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

// TestHandle_SwapWindow_RejectsBadTargets covers the regex check for
// both src and dst — a stray quote / shell metachar must not slip
// through to the tmux argv, even though the boundary already guards
// `session` separately.
func TestHandle_SwapWindow_RejectsBadTargets(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []struct {
		name string
		args map[string]any
	}{
		{"bad src", map[string]any{"session": "demo", "src": "0; rm -rf /", "dst": "1"}},
		{"bad dst", map[string]any{"session": "demo", "src": "0", "dst": "bad name"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "swap_window", "arguments": tc.args,
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

// TestHandle_SwapWindow_MissingSessionMapsCode pins the wire contract:
// swap_window against an unknown session must surface
// CodeSessionNotFound (-32000), mirroring window_select / window_move.
func TestHandle_SwapWindow_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor a real session so the dispatcher hits the "server up,
	// session missing" branch — without it, tmux emits "no server
	// running" instead of "can't find window", which would land on a
	// different code path.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "anchor", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "anchor"}}))
	})

	params := mustJSON(t, map[string]any{
		"name": "swap_window",
		"arguments": map[string]any{
			"session": "definitely_does_not_exist_xyzzy",
			"src":     "0", "dst": "1",
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

// TestHandle_ToolsList_IncludesSwapWindow makes sure the dispatch
// surface advertises the new tool so MCP clients can discover it via
// tools/list — including the strict additionalProperties contract every
// other window tool upholds.
func TestHandle_ToolsList_IncludesSwapWindow(t *testing.T) {
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
		if name != "swap_window" {
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
		if len(req) != 3 {
			t.Errorf("required = %v, want [session src dst]", req)
		}
		return
	}
	t.Fatalf("tools/list missing 'swap_window'")
}
