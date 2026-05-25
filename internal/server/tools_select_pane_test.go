package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_SelectPane_AcceptsTarget pins the happy path through the
// tool surface: a bare `target` (no flags) drives the controller and
// returns the same "ok" status block pane_select would have produced.
// This is the back-compat baseline — adding the optional flags must not
// regress callers that just want "make this pane active".
func TestHandle_SelectPane_AcceptsTarget(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
		"name": "spsel", "command": "/bin/sh", "width": 80, "height": 20,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "spsel"}}))
	})

	out := call("select_pane", map[string]any{"target": "spsel:0.0"})
	if got := extractText(t, out); got != "ok" {
		t.Fatalf("select_pane = %q, want \"ok\"", got)
	}
}

// TestHandle_SelectPane_DirectionFlipsActive proves the direction flag
// reaches tmux: split a session into two panes, walk "down" with
// select_pane, and confirm the bottom pane carries the active flag in a
// follow-up list_panes. This is the load-bearing extra capability over
// pane_select; without it there would be no reason to land the new tool.
func TestHandle_SelectPane_DirectionFlipsActive(t *testing.T) {
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
		"name": "spdir", "command": "/bin/sh", "width": 80, "height": 40,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "spdir"}}))
	})
	call("pane_split", map[string]any{
		"session": "spdir", "direction": "vertical", "detach": true,
	})

	// Anchor the active pane at index 0 first, then walk down.
	call("select_pane", map[string]any{"target": "spdir:0.0"})
	call("select_pane", map[string]any{"target": "spdir:0.0", "direction": "down"})

	body := extractText(t, call("list_panes", map[string]any{"session": "spdir"}))
	var got struct {
		Panes []map[string]any `json:"panes"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode list_panes: %v\nbody=%s", err, body)
	}
	if len(got.Panes) < 2 {
		t.Fatalf("expected 2 panes, got %d (body=%s)", len(got.Panes), body)
	}
	var activeIdx float64 = -1
	for _, p := range got.Panes {
		if active, _ := p["active"].(bool); active {
			activeIdx, _ = p["index"].(float64)
			break
		}
	}
	if activeIdx == 0 {
		t.Fatalf("expected directional walk to leave pane.index!=0 active, got %v", activeIdx)
	}
}

// TestHandle_SelectPane_RejectsConflictingFlags pins the up-front
// validation: requesting both mark and unmark (or both
// enable_input and disable_input) trips CodeInvalidParams before any
// tmux command runs, so a buggy caller fails loudly instead of relying
// on tmux's silent last-flag-wins behaviour.
func TestHandle_SelectPane_RejectsConflictingFlags(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "mark+unmark",
			args: map[string]any{"target": "demo:0.0", "mark": true, "unmark": true},
			want: "mark and unmark",
		},
		{
			name: "enable+disable",
			args: map[string]any{"target": "demo:0.0", "enable_input": true, "disable_input": true},
			want: "enable_input and disable_input",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name":      "select_pane",
				"arguments": tc.args,
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatal("expected invalid params error for conflicting flags")
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
					rerr.Code, errs.CodeInvalidParams, rerr.Message)
			}
			if !strings.Contains(rerr.Message, tc.want) {
				t.Fatalf("message %q does not contain %q", rerr.Message, tc.want)
			}
		})
	}
}

// TestHandle_SelectPane_RejectsBadDirection confirms direction enum
// validation runs at the boundary, not the controller — a typo lands as
// CodeInvalidParams (-32602) so the caller can fix the argument shape
// without parsing tmux's stderr.
func TestHandle_SelectPane_RejectsBadDirection(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "select_pane",
		"arguments": map[string]any{
			"target": "demo:0.0", "direction": "diagonal",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected error for invalid direction")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "diagonal") {
		t.Fatalf("message %q does not name the offending direction", rerr.Message)
	}
}

// TestHandle_SelectPane_RejectsEmptyTarget guards the zero-arg caller —
// the schema lists target as required, but the handler must also reject
// the empty string at runtime (mirrors the pane_select contract).
func TestHandle_SelectPane_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "select_pane",
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

// TestHandle_SelectPane_MissingTargetMapsCode pins the wire contract
// that select_pane against an unknown session surfaces
// CodeSessionNotFound rather than the generic internal-error code, so
// the tool stays consistent with pane_select / pane_resize.
func TestHandle_SelectPane_MissingTargetMapsCode(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "spanchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name":      "select_pane",
		"arguments": map[string]any{"target": "definitely_does_not_exist_xyzzy:0.0"},
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

// TestHandle_ToolsList_IncludesSelectPane confirms tools/list advertises
// the new tool so MCP clients can discover it. Mirrors the existing
// pane-tool listing assertion so a future contributor renaming or
// deleting the tool gets a clear signal.
func TestHandle_ToolsList_IncludesSelectPane(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "select_pane" {
			return
		}
	}
	t.Fatal("tools/list missing \"select_pane\"")
}
