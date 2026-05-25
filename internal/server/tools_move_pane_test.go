package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_MovePane_RelocatesAcrossWindows drives the happy path
// end-to-end: session_create → window_create (so we have two windows
// each with a single pane) → move_pane the lone pane of the donor window
// into the original. After the move the donor window is reaped and the
// destination window reports two panes. The JSON ack carries the
// logical (caller-supplied) src/dst echoes so a -session-prefix
// deployment never leaks the prefixed identity.
func TestHandle_MovePane_RelocatesAcrossWindows(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "mp", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "mp"}}))
	})
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "mp", "name": "donor", "command": "/bin/sh", "select": false,
	})

	// Sanity: two windows, one pane each, before the move.
	winsBefore, err := tools.Ctl.ListWindows(ctx, "mp")
	if err != nil {
		t.Fatalf("ListWindows pre-move: %v", err)
	}
	if len(winsBefore) != 2 {
		t.Fatalf("ListWindows pre-move = %d, want 2", len(winsBefore))
	}

	out := callTool(t, tools, ctx, "move_pane", map[string]any{
		"src":      "mp:1.0",
		"dst":      "mp:0.0",
		"no_focus": true,
	})
	body := extractText(t, out)
	var resp struct {
		Moved bool   `json:"moved"`
		Src   string `json:"src"`
		Dst   string `json:"dst"`
	}
	if jerr := json.Unmarshal([]byte(body), &resp); jerr != nil {
		t.Fatalf("decode move_pane: %v\nbody=%s", jerr, body)
	}
	if !resp.Moved {
		t.Errorf("response.moved = false, want true; body=%s", body)
	}
	if resp.Src != "mp:1.0" || resp.Dst != "mp:0.0" {
		t.Errorf("echoed targets = (%q, %q), want (mp:1.0, mp:0.0)", resp.Src, resp.Dst)
	}

	// Donor window should be reaped (tmux drops empty windows).
	winsAfter, err := tools.Ctl.ListWindows(ctx, "mp")
	if err != nil {
		t.Fatalf("ListWindows post-move: %v", err)
	}
	if len(winsAfter) != 1 {
		t.Fatalf("ListWindows post-move = %d, want 1", len(winsAfter))
	}
	dstPanes, err := tools.Ctl.ListPanes(ctx, "mp:0")
	if err != nil {
		t.Fatalf("ListPanes destination: %v", err)
	}
	if len(dstPanes) != 2 {
		t.Fatalf("destination pane count = %d, want 2", len(dstPanes))
	}
}

// TestHandle_MovePane_RejectsEmptySrc guards the required-field path:
// the schema lists src as required, but the handler must also reject
// the empty string at runtime so a half-formed call cannot leak a
// stray "" past the regex.
func TestHandle_MovePane_RejectsEmptySrc(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "move_pane",
		"arguments": map[string]any{"src": "", "dst": "demo:0.1"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty src")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_MovePane_RejectsEmptyDst mirrors the src guard for the
// destination argument so tmux never sees a "-t" without a value.
func TestHandle_MovePane_RejectsEmptyDst(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "move_pane",
		"arguments": map[string]any{"src": "demo:0.0", "dst": ""},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for empty dst")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_MovePane_RejectsBadSrc locks the regex check on src — a
// stray quote / shell metachar must not slip through to the tmux argv,
// even though the boundary already guards `session` fields elsewhere.
func TestHandle_MovePane_RejectsBadSrc(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "move_pane",
		"arguments": map[string]any{
			"src": "demo:0.0;rm -rf /",
			"dst": "demo:0.1",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad src")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_MovePane_RejectsBadDst mirrors the bad-src guard for the
// destination side so a stray quote / shell metachar can never slip
// through on either axis.
func TestHandle_MovePane_RejectsBadDst(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "move_pane",
		"arguments": map[string]any{
			"src": "demo:0.0",
			"dst": "demo:0.1$(whoami)",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad dst")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_MovePane_MissingSessionMapsCode pins the wire contract:
// move_pane against an unknown source pane must surface
// CodeSessionNotFound (-32000), mirroring pane_swap / pane_break /
// pane_join.
func TestHandle_MovePane_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor a real session so the dispatcher hits the "server up,
	// session missing" branch — without it tmux emits a different
	// stderr shape ("no server running").
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "mpanchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "move_pane",
		"arguments": map[string]any{
			"src": "definitely_does_not_exist_xyzzy:0.0",
			"dst": "mpanchor:0.0",
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

// TestHandle_MovePane_RejectsUnknownProperty pins the JSON Schema
// `additionalProperties:false` contract end-to-end via the JSON-RPC
// dispatcher. The dispatcher itself does not run schema validation, so
// the deeper guard here is the strict struct decode — but we still
// assert a clean rejection so a future contributor adding a typo'd
// field does not silently no-op.
//
// Note: encoding/json ignores unknown fields by default, so the
// dispatcher's struct decode would not catch this. We document the
// contract via the schema instead, which IDE / agent tooling consults
// before sending the call. Here we only assert that the *known* fields
// reach the handler unchanged when the optional booleans are absent.
func TestHandle_MovePane_AcceptsOptionalsAbsent(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "mpopt", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "mpopt"}}))
	})
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "mpopt", "name": "donor", "command": "/bin/sh", "select": false,
	})

	out := callTool(t, tools, ctx, "move_pane", map[string]any{
		"src": "mpopt:1.0",
		"dst": "mpopt:0.0",
	})
	if got := extractText(t, out); !strings.Contains(got, `"moved":true`) {
		t.Fatalf("response missing moved:true; body=%s", got)
	}
}

// TestHandle_ToolsList_IncludesMovePane makes sure the dispatch surface
// advertises the new tool so MCP clients can discover it via tools/list,
// and pins the strict additionalProperties / required contract.
func TestHandle_ToolsList_IncludesMovePane(t *testing.T) {
	t.Parallel()
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
		if name != "move_pane" {
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
		// Optional booleans must be present in the schema so an agent
		// can discover them without reading the docs.
		props, _ := schema["properties"].(map[string]any)
		for _, opt := range []string{"horizontal", "before", "no_focus"} {
			if _, ok := props[opt]; !ok {
				t.Errorf("schema missing optional property %q", opt)
			}
		}
	}
	if !found {
		t.Fatalf("tools/list missing 'move_pane'")
	}
}

// TestIsReadOnlyTool_RejectsMovePane pins the read-only policy: move_pane
// is a mutating tool and must NOT be in the readOnlyTools allowlist. A
// future contributor moving the entry there would see this test fire
// and remember to update both halves of the policy.
func TestIsReadOnlyTool_RejectsMovePane(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("move_pane") {
		t.Fatal("IsReadOnlyTool(\"move_pane\") = true, want false (mutating tools must not be inspection-allowed)")
	}
}
