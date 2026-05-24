package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// listWindowIndexes pulls the integer indexes out of a list_windows
// response body. Centralising the decode keeps the move-window tests
// short and focused on assertions about the layout.
func listWindowIndexes(t *testing.T, body string) []int {
	t.Helper()
	var obj struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_windows: %v\nbody=%s", err, body)
	}
	out := make([]int, 0, len(obj.Windows))
	for _, w := range obj.Windows {
		// JSON numbers decode as float64 — coerce explicitly so the test
		// is not sensitive to encoder choices.
		idx, _ := w["index"].(float64)
		out = append(out, int(idx))
	}
	return out
}

// hasIndex reports whether haystack contains needle. Tiny helper so the
// table-driven assertions read naturally.
func hasIndex(haystack []int, needle int) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// TestHandle_WindowMove_RenumbersWithinSession runs the happy path: a
// session with three windows has window 1 relocated to slot 5; after
// the move list_windows must reflect the new layout (no window at the
// old index, a window at the new one).
func TestHandle_WindowMove_RenumbersWithinSession(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "wm", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "wm"}}))
	})

	// Build out a session with three windows so we have something to
	// move around. Background the new windows so the active flag stays
	// on the original — the move test cares about layout, not focus.
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "wm", "name": "second", "command": "/bin/sh", "select": false,
	})
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "wm", "name": "third", "command": "/bin/sh", "select": false,
	})

	// Sanity: indexes are 0,1,2 before the move so the assertion below
	// has something meaningful to compare against.
	pre := listWindowIndexes(t, extractText(t, callTool(t, tools, ctx,
		"list_windows", map[string]any{"session": "wm"})))
	if !hasIndex(pre, 1) {
		t.Fatalf("baseline: expected window at index 1, got %v", pre)
	}
	if hasIndex(pre, 5) {
		t.Fatalf("baseline: did not expect a window at index 5 yet, got %v", pre)
	}

	got := extractText(t, callTool(t, tools, ctx, "window_move", map[string]any{
		"src": "wm:1", "dst": "wm:5",
	}))
	if !strings.Contains(got, `"wm:1"`) || !strings.Contains(got, `"wm:5"`) {
		t.Fatalf("window_move text = %q, want both 'wm:1' and 'wm:5'", got)
	}

	post := listWindowIndexes(t, extractText(t, callTool(t, tools, ctx,
		"list_windows", map[string]any{"session": "wm"})))
	if hasIndex(post, 1) {
		t.Fatalf("window still present at old index 1 after move: %v", post)
	}
	if !hasIndex(post, 5) {
		t.Fatalf("window missing at new index 5 after move: %v", post)
	}
}

// TestHandle_WindowMove_MissingSessionMapsCode pins the wire contract:
// window_move against an unknown source session must surface
// CodeSessionNotFound, mirroring the rest of the window tools so an MCP
// client can branch on a stable code rather than parsing tmux stderr.
func TestHandle_WindowMove_MissingSessionMapsCode(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Anchor a real session so the dispatcher hits the "server up,
	// session missing" branch — without it, tmux emits "no server
	// running" instead of "can't find session", which would land on a
	// different code path.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "window_move",
		"arguments": map[string]any{
			"src": "definitely_does_not_exist_xyzzy:0",
			"dst": "anchor:9",
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

// TestHandle_WindowMove_DuplicateIndexSurfacesError pins the
// "destination already taken" path: tmux move-window refuses to land on
// an occupied index with "index in use", and the boundary must surface
// the failure (rather than silently swallowing it) so an agent can
// react. CodeInternal is the right shape — this is not an invalid-
// params error (the schema validated cleanly) and not a missing-session
// error (both sessions exist).
func TestHandle_WindowMove_DuplicateIndexSurfacesError(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "wmd", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "wmd"}}))
	})
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "wmd", "name": "second", "command": "/bin/sh", "select": false,
	})

	// Indexes 0 and 1 are both occupied — moving 0 onto 1 must fail.
	params := mustJSON(t, map[string]any{
		"name":      "window_move",
		"arguments": map[string]any{"src": "wmd:0", "dst": "wmd:1"},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error moving onto an occupied index, got result %#v", res)
	}
	if rerr.Code != errs.CodeInternal {
		t.Fatalf("code = %d, want CodeInternal (%d), msg=%q",
			rerr.Code, errs.CodeInternal, rerr.Message)
	}
	// tmux's wording for "destination already taken" is "index in use" —
	// guard the message so a future tmux that rephrases it doesn't
	// silently regress the contract.
	if !strings.Contains(strings.ToLower(rerr.Message), "in use") {
		t.Errorf("error message %q should reference the duplicate index", rerr.Message)
	}
}

// TestHandle_WindowMove_RejectsBadSrc covers two boundary failure modes
// in one table — a src that isn't `<session>:<window>` form, and a src
// whose pieces violate the regex policy. Both must fail with
// CodeInvalidParams before tmux is consulted.
func TestHandle_WindowMove_RejectsBadSrc(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []struct {
		name string
		src  string
	}{
		{"missing colon", "wm0"},
		{"empty source window", "wm:"},
		{"bad session", "bad name with spaces:0"},
		{"bad window", "wm:bad name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := mustJSON(t, map[string]any{
				"name":      "window_move",
				"arguments": map[string]any{"src": tc.src, "dst": "wm:5"},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params for src %q", tc.src)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d) for src %q",
					rerr.Code, errs.CodeInvalidParams, tc.src)
			}
		})
	}
}

// TestHandle_WindowMove_RejectsBadDst pins the dst-side boundary
// guards. An empty window part is *allowed* (lets tmux pick), so the
// table only catches the genuinely-invalid forms.
func TestHandle_WindowMove_RejectsBadDst(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []struct {
		name string
		dst  string
	}{
		{"missing colon", "wm5"},
		{"bad session", "bad name:5"},
		{"bad window", "wm:bad name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := mustJSON(t, map[string]any{
				"name":      "window_move",
				"arguments": map[string]any{"src": "wm:0", "dst": tc.dst},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params for dst %q", tc.dst)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d) for dst %q",
					rerr.Code, errs.CodeInvalidParams, tc.dst)
			}
		})
	}
}

// TestHandle_WindowMove_RejectsEmptyArgs locks the up-front empty-string
// guards so the dispatcher never builds a partial tmux target.
func TestHandle_WindowMove_RejectsEmptyArgs(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []struct {
		name string
		args map[string]any
	}{
		{"empty src", map[string]any{"src": "", "dst": "wm:5"}},
		{"empty dst", map[string]any{"src": "wm:0", "dst": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := mustJSON(t, map[string]any{
				"name": "window_move", "arguments": tc.args,
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params")
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_WindowMove_AcceptsEmptyDstWindow exercises tmux's
// "next free index" mode: a dst of `<session>:` should let tmux pick.
// Without explicit assertions an integration regression here would slip
// through every other test in the suite.
func TestHandle_WindowMove_AcceptsEmptyDstWindow(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "src", "command": "/bin/sh",
	})
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "dst", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "src"}}))
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "dst"}}))
	})

	// Source has its only window at index 0; need a second so the move
	// doesn't reduce src to zero windows (which would tear down the
	// session and confuse the assertion).
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "src", "name": "moveme", "command": "/bin/sh", "select": false,
	})

	got := extractText(t, callTool(t, tools, ctx, "window_move", map[string]any{
		"src": "src:1", "dst": "dst:",
	}))
	if !strings.Contains(got, `"src:1"`) || !strings.Contains(got, `"dst:"`) {
		t.Fatalf("window_move text = %q, want references to 'src:1' and 'dst:'", got)
	}

	// Destination must now have two windows (its original plus the
	// migrated one); the source must be back down to one.
	dstWins := listWindowIndexes(t, extractText(t, callTool(t, tools, ctx,
		"list_windows", map[string]any{"session": "dst"})))
	if len(dstWins) != 2 {
		t.Fatalf("dst window count = %d, want 2 (%v)", len(dstWins), dstWins)
	}
	srcWins := listWindowIndexes(t, extractText(t, callTool(t, tools, ctx,
		"list_windows", map[string]any{"session": "src"})))
	if len(srcWins) != 1 {
		t.Fatalf("src window count = %d, want 1 (%v)", len(srcWins), srcWins)
	}
}

// TestHandle_ToolsList_IncludesWindowMove makes sure the dispatch
// surface advertises the new tool so MCP clients can discover it via
// tools/list — including the strict additionalProperties contract every
// other window tool upholds.
func TestHandle_ToolsList_IncludesWindowMove(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	found := false
	for _, def := range listing {
		name, _ := def["name"].(string)
		if name != "window_move" {
			continue
		}
		found = true
		schema, _ := def["inputSchema"].(map[string]any)
		// additionalProperties:false is part of the contract — an agent
		// that misnames a field gets a fast schema-shaped rejection
		// rather than a silent no-op.
		if got, ok := schema["additionalProperties"].(bool); !ok || got {
			t.Errorf("schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		req, _ := schema["required"].([]string)
		if len(req) != 2 {
			t.Errorf("required = %v, want [src dst]", req)
		}
	}
	if !found {
		t.Fatalf("tools/list missing 'window_move'")
	}
}
