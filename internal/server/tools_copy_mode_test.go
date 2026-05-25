package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_CopyMode_EnterAndExit drives the round-trip happy path
// end-to-end through the dispatcher: session_create gives us a pane,
// the first copy_mode flips pane_in_mode to 1, a second copy_mode with
// exit=true brings it back to 0. Both transitions are pinned because a
// future contributor that breaks one direction would silently leave a
// pane stuck in the wrong mode.
func TestHandle_CopyMode_EnterAndExit(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cm", "command": "/bin/bash", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "cm"}}))
	})

	if got := readPaneInMode(t, tools, ctx, "cm:0.0"); got != "0" {
		t.Fatalf("pre-enter pane_in_mode = %q, want \"0\"", got)
	}

	out := callTool(t, tools, ctx, "copy_mode", map[string]any{"target": "cm:0.0"})
	body := extractText(t, out)
	var resp struct {
		OK     bool   `json:"ok"`
		Target string `json:"target"`
		Exit   bool   `json:"exit"`
	}
	if jerr := json.Unmarshal([]byte(body), &resp); jerr != nil {
		t.Fatalf("decode copy_mode enter: %v\nbody=%s", jerr, body)
	}
	if !resp.OK {
		t.Errorf("response.ok = false, want true; body=%s", body)
	}
	if resp.Target != "cm:0.0" {
		t.Errorf("response.target = %q, want cm:0.0", resp.Target)
	}
	if resp.Exit {
		t.Errorf("response.exit = true on enter, want false; body=%s", body)
	}
	if got := readPaneInMode(t, tools, ctx, "cm:0.0"); got != "1" {
		t.Fatalf("post-enter pane_in_mode = %q, want \"1\"", got)
	}

	// Exit the mode and confirm we're back to a normal shell pane.
	callTool(t, tools, ctx, "copy_mode", map[string]any{
		"target": "cm:0.0",
		"exit":   true,
	})
	if got := readPaneInMode(t, tools, ctx, "cm:0.0"); got != "0" {
		t.Fatalf("post-exit pane_in_mode = %q, want \"0\"", got)
	}
}

// TestHandle_CopyMode_WithSrcPane covers the `src_pane` (-s) flag end-
// to-end: a session with two panes enters copy-mode on one pane while
// referencing the other as the scrollback source. The destination pane
// must report pane_in_mode=1 and the JSON ack must echo the logical
// src_pane the caller supplied.
func TestHandle_CopyMode_WithSrcPane(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cms", "command": "/bin/bash", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "cms"}}))
	})
	callTool(t, tools, ctx, "pane_split", map[string]any{
		"session": "cms", "direction": "vertical", "command": "/bin/bash", "detach": true,
	})

	out := callTool(t, tools, ctx, "copy_mode", map[string]any{
		"target":   "cms:0.0",
		"src_pane": "cms:0.1",
	})
	body := extractText(t, out)
	if !strings.Contains(body, `"src_pane":"cms:0.1"`) {
		t.Fatalf("response missing src_pane echo; body=%s", body)
	}
	if got := readPaneInMode(t, tools, ctx, "cms:0.0"); got != "1" {
		t.Fatalf("post-enter pane_in_mode = %q, want \"1\"", got)
	}
}

// TestHandle_CopyMode_RejectsEmptyTarget guards the required-field
// path: the schema lists target as required, but the handler must also
// reject the empty string at runtime so a half-formed call cannot leak
// a stray "" past the regex.
func TestHandle_CopyMode_RejectsEmptyTarget(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "copy_mode",
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

// TestHandle_CopyMode_RejectsBadTarget locks the regex check on the
// target argument — a stray quote / shell metachar must not slip
// through to the tmux argv.
func TestHandle_CopyMode_RejectsBadTarget(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "copy_mode",
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

// TestHandle_CopyMode_MissingSessionMapsCode pins the wire contract:
// copy_mode against an unknown target pane must surface
// CodeSessionNotFound (-32000), mirroring move_pane / pane_swap.
func TestHandle_CopyMode_MissingSessionMapsCode(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Anchor a real session so the dispatcher hits the "server up,
	// target missing" branch — without it tmux emits a different stderr
	// shape ("no server running").
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cmanchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "copy_mode",
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

// TestHandle_CopyMode_RejectsUnknownProperty pins the schema-side
// `additionalProperties:false` contract via the strict struct decode:
// encoding/json silently ignores unknown fields, so the only way the
// boundary surfaces a typo at call time is via the schema. Here we
// document the path operators (and IDE / agent tooling) actually use:
// asserting the schema entry exists with the strict flag set. The
// tools/list assertion in TestHandle_ToolsList_IncludesCopyMode pins
// the schema shape end-to-end.
func TestHandle_CopyMode_AcceptsOptionalsAbsent(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "cmopt", "command": "/bin/bash", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "cmopt"}}))
	})

	out := callTool(t, tools, ctx, "copy_mode", map[string]any{
		"target": "cmopt:0.0",
	})
	if got := extractText(t, out); !strings.Contains(got, `"ok":true`) {
		t.Fatalf("response missing ok:true; body=%s", got)
	}
}

// TestHandle_ToolsList_IncludesCopyMode makes sure the dispatch surface
// advertises the new tool so MCP clients can discover it via tools/list,
// and pins the strict additionalProperties / required contract.
func TestHandle_ToolsList_IncludesCopyMode(t *testing.T) {
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
		if name != "copy_mode" {
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
		if len(req) != 1 || req[0] != "target" {
			t.Errorf("required = %v, want [target]", req)
		}
		// Optional properties must be present so an agent can discover
		// them without reading the docs.
		props, _ := schema["properties"].(map[string]any)
		for _, opt := range []string{"src_pane", "exit", "scroll_down", "mouse", "drag_mode"} {
			if _, ok := props[opt]; !ok {
				t.Errorf("schema missing optional property %q", opt)
			}
		}
	}
	if !found {
		t.Fatalf("tools/list missing 'copy_mode'")
	}
}

// TestIsReadOnlyTool_RejectsCopyMode pins the read-only policy:
// copy_mode mutates pane state (puts the pane into copy-mode / takes it
// out) and must NOT be in the readOnlyTools allowlist. A future
// contributor moving the entry there would see this test fire and
// remember to update both halves of the policy.
func TestIsReadOnlyTool_RejectsCopyMode(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("copy_mode") {
		t.Fatal("IsReadOnlyTool(\"copy_mode\") = true, want false (mutating tools must not be inspection-allowed)")
	}
}

// readPaneInMode evaluates `#{?pane_in_mode,1,0}` against target via
// the dispatcher's display_message tool. tmux substitutes
// pane_in_mode=1 when the pane is in any mode (copy / view / clock /
// choose), which is exactly what the copy_mode contract pins. Going
// through the dispatcher (rather than the controller directly) keeps
// the test consistent with how an MCP client would observe the same
// state.
//
// display_message's tool surface takes session/window/pane as separate
// fields and resolves a `#{...}` formatter into a JSON value. We split
// the canonical "session:window.pane" target back into its components
// so the helper accepts the same target shape the rest of the test
// uses.
func readPaneInMode(t *testing.T, tools *Tools, ctx context.Context, target string) string {
	t.Helper()
	session, window, pane := splitPaneTarget(target)
	args := map[string]any{
		"format": "#{?pane_in_mode,1,0}",
	}
	if session != "" {
		args["session"] = session
	}
	if window != "" {
		args["window"] = window
	}
	if pane != "" {
		args["pane"] = pane
	}
	out, rerr := tools.Handle(ctx, "tools/call", mustJSON(t, map[string]any{
		"name":      "display_message",
		"arguments": args,
	}))
	if rerr != nil {
		t.Fatalf("display_message: %s", rerr.Message)
	}
	body := extractText(t, out)
	var resp struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode display_message: %v\nbody=%s", err, body)
	}
	return strings.TrimSpace(resp.Value)
}

// splitPaneTarget breaks a "session:window.pane" string back into its
// three components. Used by readPaneInMode because display_message's
// tool surface takes the parts as separate fields (so a stray colon /
// dot in a freeform target would never reach tmux unsanitised). Any
// component the caller omitted comes back as the empty string.
func splitPaneTarget(target string) (session, window, pane string) {
	colon := strings.IndexByte(target, ':')
	if colon < 0 {
		return target, "", ""
	}
	session = target[:colon]
	rest := target[colon+1:]
	dot := strings.IndexByte(rest, '.')
	if dot < 0 {
		return session, rest, ""
	}
	return session, rest[:dot], rest[dot+1:]
}
