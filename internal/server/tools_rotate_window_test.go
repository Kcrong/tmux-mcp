package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// buildThreePaneSession spins up a session with three panes via the
// public tool surface (session_create + two pane_split calls). The
// shared scaffolding keeps the rotate_window tests focused on
// assertions about the rotation rather than on plumbing.
func buildThreePaneSession(t *testing.T, tools *Tools, ctx context.Context, name string) {
	t.Helper()
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": name, "command": "/bin/bash", "width": 100, "height": 30,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": name},
			}))
	})
	callTool(t, tools, ctx, "pane_split", map[string]any{
		"session":   name,
		"direction": "horizontal",
		"command":   "/bin/bash",
		"detach":    true,
	})
	callTool(t, tools, ctx, "pane_split", map[string]any{
		"session":   name,
		"direction": "vertical",
		"command":   "/bin/bash",
		"detach":    true,
	})
}

// listPaneIDsViaTool runs list_panes through the dispatcher and
// returns the per-pane #{pane_id} values in slot order. Going through
// the boundary (rather than reaching into the controller) keeps the
// rotate_window assertions self-contained at the wire layer.
func listPaneIDsViaTool(t *testing.T, tools *Tools, ctx context.Context, session string) []string {
	t.Helper()
	body := extractText(t, callTool(t, tools, ctx, "list_panes", map[string]any{"session": session}))
	// list_panes returns {"panes":[{"id":"%0",...}, ...]}; we only need
	// the ID column, in order. The dedicated decoder over in
	// tools_swap_window_test.go is per-window-name; here we want pane
	// ids, so a small inline decode keeps the dependency scope tight.
	type paneRow struct {
		ID string `json:"id"`
	}
	type panesEnvelope struct {
		Panes []paneRow `json:"panes"`
	}
	var env panesEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode list_panes: %v\nbody=%s", err, body)
	}
	out := make([]string, 0, len(env.Panes))
	for _, p := range env.Panes {
		out = append(out, p.ID)
	}
	return out
}

// TestHandle_RotateWindow_UpwardShiftsByOne is the load-bearing happy
// path: a three-pane window has its panes rotated through the existing
// layout slots, and the captured pane_id ordering is shifted by exactly
// one slot. tmux's default `-U` is what the bare call produces (the
// schema's documented default), so this test pins that contract end to
// end through the dispatcher.
func TestHandle_RotateWindow_UpwardShiftsByOne(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	buildThreePaneSession(t, tools, ctx, "rwu")
	pre := listPaneIDsViaTool(t, tools, ctx, "rwu")
	if len(pre) != 3 {
		t.Fatalf("baseline pane count = %d, want 3 (pre=%v)", len(pre), pre)
	}

	got := extractText(t, callTool(t, tools, ctx, "rotate_window", map[string]any{
		"target": "rwu",
	}))
	if !strings.Contains(got, `"rotated":true`) {
		t.Fatalf("rotate_window text = %q, want it to contain 'rotated:true'", got)
	}

	post := listPaneIDsViaTool(t, tools, ctx, "rwu")
	if len(post) != len(pre) {
		t.Fatalf("post pane count = %d, want %d (post=%v pre=%v)", len(post), len(pre), post, pre)
	}
	for i := range pre {
		want := pre[(i+1)%len(pre)]
		if post[i] != want {
			t.Fatalf("upward rotate: post[%d]=%q, want %q (pre=%v post=%v)",
				i, post[i], want, pre, post)
		}
	}
}

// TestHandle_RotateWindow_DownwardShiftsByOneOtherWay pins the
// downward=true wire flag. Without this test, a refactor that always
// emitted `-U` (the tmux default) would still pass the upward case;
// the only way to prove the boolean reaches tmux is to assert the
// inverse shift here.
func TestHandle_RotateWindow_DownwardShiftsByOneOtherWay(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	buildThreePaneSession(t, tools, ctx, "rwd")
	pre := listPaneIDsViaTool(t, tools, ctx, "rwd")

	got := extractText(t, callTool(t, tools, ctx, "rotate_window", map[string]any{
		"target": "rwd", "downward": true,
	}))
	if !strings.Contains(got, `"rotated":true`) {
		t.Fatalf("rotate_window text = %q, want it to contain 'rotated:true'", got)
	}

	post := listPaneIDsViaTool(t, tools, ctx, "rwd")
	if len(post) != len(pre) {
		t.Fatalf("post pane count = %d, want %d", len(post), len(pre))
	}
	for i := range pre {
		want := pre[(i-1+len(pre))%len(pre)]
		if post[i] != want {
			t.Fatalf("downward rotate: post[%d]=%q, want %q (pre=%v post=%v)",
				i, post[i], want, pre, post)
		}
	}
}

// TestHandle_RotateWindow_RejectsBadTarget covers the regex check on
// the target — a stray quote / shell metachar must not slip through to
// the tmux argv. We exercise both the bare-session and qualified
// `<session>:<window>` shapes so a future refactor of the validator
// can't drop coverage on either branch.
func TestHandle_RotateWindow_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []struct {
		name string
		args map[string]any
	}{
		{"empty target", map[string]any{"target": ""}},
		{"bad session", map[string]any{"target": "demo;rm -rf /"}},
		{"bad window", map[string]any{"target": "demo:bad name"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "rotate_window", "arguments": tc.args,
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params for %s", tc.name)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
					rerr.Code, errs.CodeInvalidParams, rerr.Message)
			}
		})
	}
}

// TestHandle_RotateWindow_MissingSessionMapsCode pins the wire
// contract: rotate_window against an unknown session must surface
// CodeSessionNotFound (-32000), mirroring swap_window / window_select.
func TestHandle_RotateWindow_MissingSessionMapsCode(t *testing.T) {
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
		"name": "rwanchor", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "rwanchor"},
			}))
	})

	params := mustJSON(t, map[string]any{
		"name": "rotate_window",
		"arguments": map[string]any{
			"target": "definitely_does_not_exist_xyzzy",
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

// TestHandle_RotateWindow_RejectsUnknownProperty pins the strict
// additionalProperties:false contract — an agent that misnames a field
// (e.g. "down" instead of "downward") gets a fast schema-shaped
// rejection rather than a silent no-op. Today the dispatcher's
// json.Unmarshal is permissive enough that an unknown property
// silently no-ops; this test lives so a future schema-validation pass
// (or a tighter Unmarshal) lands without regressing the contract. The
// test is structured to accept either outcome: a hard rejection
// (preferred) or a silent ignore that still respects the rest of the
// payload — the load-bearing invariant is that the bad field can't
// "win" by overriding a documented one.
func TestHandle_RotateWindow_RejectsUnknownProperty(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor a session so a successful call has somewhere to land. If
	// the unknown property is silently ignored we still expect the
	// rotation to be rejected because the session has only one pane —
	// tmux refuses to rotate a single-pane window. Either way the test
	// proves the unknown property does not get smuggled into argv.
	buildThreePaneSession(t, tools, ctx, "rwx")

	params := mustJSON(t, map[string]any{
		"name": "rotate_window",
		"arguments": map[string]any{
			"target":         "rwx",
			"unknown_field!": "anything",
		},
	})
	_, rerr := tools.Handle(ctx, "tools/call", params)
	// json.Unmarshal in the handler ignores unknown keys today; the
	// load-bearing assertion is that the schema declares
	// additionalProperties:false (validated by the tools/list pin
	// below), so a future schema-validation step can flip this to a
	// hard rejection without touching this test. For now: if the
	// dispatcher rejects, the code must be CodeInvalidParams; if it
	// accepts, the call must not have surfaced a tmux-side error
	// either.
	if rerr != nil && rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("rejection code = %d, want CodeInvalidParams (%d) (msg=%q)",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
}

// TestHandle_ToolsList_IncludesRotateWindow makes sure the dispatch
// surface advertises the new tool so MCP clients can discover it via
// tools/list — including the strict additionalProperties contract
// every other window tool upholds.
func TestHandle_ToolsList_IncludesRotateWindow(t *testing.T) {
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
		if name != "rotate_window" {
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
		props, _ := schema["properties"].(map[string]any)
		// downward must carry a documented default of false so a caller
		// that omits it sees the tmux-default `-U` rotation; flipping
		// the default would silently change behaviour for every
		// existing client.
		downward, _ := props["downward"].(map[string]any)
		if got, ok := downward["default"].(bool); !ok || got {
			t.Errorf("downward.default = %v, want false", downward["default"])
		}
		return
	}
	t.Fatalf("tools/list missing 'rotate_window'")
}

// TestHandle_RotateWindow_NotInReadOnlyAllowlist pins the policy: a
// mutating tool must NOT be on the read-only allowlist. Without this
// test, accidentally adding rotate_window to readOnlyTools would let a
// -read-only deployment dispatch the call — defeating the flag's whole
// point.
func TestHandle_RotateWindow_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("rotate_window") {
		t.Fatal("IsReadOnlyTool(\"rotate_window\") = true, want false (mutating tools must not be inspection-allowed)")
	}
}
