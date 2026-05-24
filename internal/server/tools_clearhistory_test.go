package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_ClearHistory_DropsScrollback drives the happy path
// end-to-end through the dispatcher: session_create → send_keys to
// fill the scrollback → capture(scrollback) confirming the buffer is
// big → clear_history → capture(scrollback) confirming the buffer
// shrunk. Verifies the dispatcher is wired up, the schema accepts the
// documented arguments, and the response envelope carries the
// `{"cleared": true}` ack.
func TestHandle_ClearHistory_DropsScrollback(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

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
		"name": "ch", "command": "/bin/sh",
		// Small terminal so the printed lines spill into scrollback fast.
		"width": 80, "height": 10,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "ch"}}))
	})

	// Drive the loop, then wait for the last printed line so the
	// scrollback is definitely populated before we sample it.
	call("send_keys", map[string]any{
		"session": "ch",
		"keys":    []string{"for i in $(seq 1 500); do echo line-$i; done", "Enter"},
	})
	call("wait_for_text", map[string]any{
		"session": "ch", "pattern": "line-500", "timeout_ms": 8000,
	})

	preText := extractText(t, call("capture", map[string]any{
		"session": "ch", "mode": "scrollback",
	}))
	var preObj struct {
		Snapshot string `json:"snapshot"`
	}
	if err := json.Unmarshal([]byte(preText), &preObj); err != nil {
		t.Fatalf("decode capture pre: %v\nbody=%s", err, preText)
	}
	preLines := strings.Count(preObj.Snapshot, "\n")
	if preLines < 100 {
		t.Fatalf("scrollback pre-clear has %d lines, expected >=100; body=%q",
			preLines, preObj.Snapshot)
	}

	clearText := extractText(t, call("clear_history", map[string]any{
		"target": "ch",
	}))
	var clearObj struct {
		Cleared bool `json:"cleared"`
	}
	if err := json.Unmarshal([]byte(clearText), &clearObj); err != nil {
		t.Fatalf("decode clear_history: %v\nbody=%s", err, clearText)
	}
	if !clearObj.Cleared {
		t.Fatalf("clear_history cleared flag = false, want true; body=%s", clearText)
	}

	postText := extractText(t, call("capture", map[string]any{
		"session": "ch", "mode": "scrollback",
	}))
	var postObj struct {
		Snapshot string `json:"snapshot"`
	}
	if err := json.Unmarshal([]byte(postText), &postObj); err != nil {
		t.Fatalf("decode capture post: %v\nbody=%s", err, postText)
	}
	postLines := strings.Count(postObj.Snapshot, "\n")
	if postLines >= preLines {
		t.Fatalf("scrollback post-clear (%d) did not shrink from pre (%d)", postLines, preLines)
	}
	if postLines > 30 {
		t.Fatalf("scrollback post-clear has %d lines, expected <=30 (pane height ~10)", postLines)
	}
}

// TestHandle_ClearHistory_RejectsMissingTarget pins the required-field
// path: omitting `target` must come back as CodeInvalidParams rather
// than falling through to tmux with an empty -t value (which tmux
// would resolve to whatever pane it considers current).
func TestHandle_ClearHistory_RejectsMissingTarget(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "clear_history",
		"arguments": map[string]any{},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ClearHistory_RejectsBadTarget locks the regex check on
// `target` so a stray quote/whitespace can't slip through to the tmux
// argv.
func TestHandle_ClearHistory_RejectsBadTarget(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "clear_history",
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

// TestHandle_ClearHistory_MissingSessionMapsCode pins the wire contract
// that clear_history against a target on an unknown session surfaces
// CodeSessionNotFound (-32000), mirroring pane_kill / pane_select.
func TestHandle_ClearHistory_MissingSessionMapsCode(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Anchor with a real session so we exercise "server up, pane missing"
	// rather than "no server" (different stderr shape).
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "anchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name": "clear_history",
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

// TestHandle_ToolsList_IncludesClearHistory makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint.
func TestHandle_ToolsList_IncludesClearHistory(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "clear_history" {
			return
		}
	}
	t.Fatalf("tools/list missing clear_history")
}
