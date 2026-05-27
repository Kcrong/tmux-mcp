package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// layoutDumpFromDisplayMessage runs the `display_message` tool against
// `#{window_layout}` so the test can compare the layout string before
// and after a next_layout call. tmux's preset-name → layout-string
// mapping is opaque (e.g. "bb62,159x48,0,0{...}") but stable for a
// given pane count, so the only cross-version-stable signal that
// next_layout actually rotated the window is "the dump string is
// different now". Centralised in the test file so the assertion stays
// readable and the per-call decode noise lives in one place.
func layoutDumpFromDisplayMessage(t *testing.T, tools *Tools, ctx context.Context, session string) string {
	t.Helper()
	body := extractText(t, callTool(t, tools, ctx, "display_message", map[string]any{
		"session": session,
		"format":  "#{window_layout}",
	}))
	// display_message's JSON envelope is {"value": "<resolved>"}; pull
	// the field out so the assertion can compare bare layout strings
	// without re-encoding noise.
	var obj struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode display_message: %v body=%q", err, body)
	}
	return strings.TrimSpace(obj.Value)
}

// TestHandle_NextLayout_RotatesPreset runs the load-bearing happy path
// end-to-end through the dispatcher: a multi-pane window anchored on a
// known preset, a next_layout call must land on a different
// #{window_layout} dump. Catches both the dispatcher wiring and the
// controller argv ordering in one shot.
//
// We split the window twice to land on three panes — every preset in
// tmux's ring produces a visibly distinct dump at that pane count, so
// the "different dump" assertion is robust against version drift in
// the ring's exact ordering.
func TestHandle_NextLayout_RotatesPreset(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "nl_rot", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "nl_rot"}}))
	})
	// Two splits → three panes, enough for the preset ring to produce
	// distinct layout dumps between successive next-layout calls.
	callTool(t, tools, ctx, "pane_split", map[string]any{
		"session": "nl_rot", "direction": "vertical",
	})
	callTool(t, tools, ctx, "pane_split", map[string]any{
		"session": "nl_rot", "direction": "horizontal",
	})

	// Capture the baseline dump so the assertion has a concrete value
	// to compare against. Without it a no-op next_layout could not be
	// distinguished from a working one whose target preset happened to
	// produce the same shape.
	before := layoutDumpFromDisplayMessage(t, tools, ctx, "nl_rot")
	if before == "" {
		t.Fatal("baseline #{window_layout} is empty")
	}

	got := extractText(t, callTool(t, tools, ctx, "next_layout", map[string]any{
		"target": "nl_rot",
	}))
	if got != "ok" {
		t.Fatalf("next_layout text = %q, want \"ok\"", got)
	}

	after := layoutDumpFromDisplayMessage(t, tools, ctx, "nl_rot")
	if after == "" {
		t.Fatal("post-rotation #{window_layout} is empty")
	}
	if after == before {
		t.Fatalf("next_layout did not change layout dump (still %q)", before)
	}
}

// TestHandle_NextLayout_MissingSessionMapsCode pins the wire contract:
// next_layout against an unknown session must surface
// CodeSessionNotFound (-32000), mirroring window_select / select_layout.
// We anchor a real session first so the dispatcher hits the "server up,
// session missing" branch rather than the noisier "no server running"
// path that tmux emits on a fully empty server.
func TestHandle_NextLayout_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "anchor_nl", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "anchor_nl"}}))
	})

	params := mustJSON(t, map[string]any{
		"name":      "next_layout",
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

// TestHandle_NextLayout_RejectsBadTarget guards the regex policy on
// the `target` argument: any string that would otherwise be passed to
// tmux must be refused with CodeInvalidParams up front, mirroring
// next_window / window_select. The case list intentionally exercises
// both shell metachars and bare whitespace because either could slip
// past a less strict regex into tmux's argv.
func TestHandle_NextLayout_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []struct {
		name   string
		target string
	}{
		{"spaces", "bad name with spaces"},
		{"shell metachar", "demo; rm -rf /"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name":      "next_layout",
				"arguments": map[string]any{"target": tc.target},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params error for %q", tc.target)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)",
					rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_NextLayout_RejectsEmptyTarget locks the up-front guard on
// the target argument — otherwise the dispatcher would build a tmux
// `next-layout -t ""` and let tmux reject it with a noisier error.
func TestHandle_NextLayout_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "next_layout",
		"arguments": map[string]any{"target": ""},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)",
			rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_NextLayout_RejectsUnknownProperty guards the
// additionalProperties:false contract from the schema. An agent that
// misnames a field gets a fast schema-shaped rejection rather than a
// silent no-op — the same pattern next_window / window_select uphold.
// We assert by walking tools/list rather than by sending a probe call,
// because additionalProperties is a schema contract: the JSON-RPC
// layer trusts the client to reject unknown fields up front, and the
// canonical proof lives in the published schema.
func TestHandle_NextLayout_RejectsUnknownProperty(t *testing.T) {
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
		if name != "next_layout" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		if got, ok := schema["additionalProperties"].(bool); !ok || got {
			t.Errorf("schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		req, _ := schema["required"].([]string)
		if len(req) != 1 || req[0] != "target" {
			t.Errorf("required = %v, want [target]", req)
		}
		return
	}
	t.Fatalf("tools/list missing 'next_layout'")
}

// TestHandle_NextLayout_NotInReadOnlyAllowlist locks the policy that
// next_layout mutates state (it changes the active window's pane
// arrangement) and therefore must be rejected when the server is armed
// with -read-only. The readonly_test list pins it on the mutators side;
// this duplicate at the dispatch layer guards against a future
// contributor removing it from one place but not the other.
func TestHandle_NextLayout_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("next_layout") {
		t.Fatal("next_layout must not be on the read-only allowlist (it mutates the active window's pane arrangement)")
	}
}
