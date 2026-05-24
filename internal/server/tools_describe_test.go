package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestSessionDescribe_HappyPath exercises the full round-trip through
// the JSON-RPC dispatcher: create a session, describe it, then assert
// the response envelope decodes cleanly and every field looks sensible.
func TestSessionDescribe_HappyPath(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	const name = "describe_rt"
	call("session_create", map[string]any{
		"name": name, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": name},
			}))
	})

	body := extractText(t, call("session_describe", map[string]any{"name": name}))

	// 3. body must be valid JSON.
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode session_describe body %q: %v", body, err)
	}

	// Field-by-field checks. JSON numbers decode as float64.
	if got["name"] != name {
		t.Errorf("name = %v, want %q", got["name"], name)
	}
	wins, _ := got["windows"].(float64)
	if wins < 1 {
		t.Errorf("windows = %v, want >= 1", got["windows"])
	}
	panes, _ := got["panes"].(float64)
	if panes < 1 {
		t.Errorf("panes = %v, want >= 1", got["panes"])
	}
	width, _ := got["width"].(float64)
	if width < 20 {
		t.Errorf("width = %v, want >= 20", got["width"])
	}
	height, _ := got["height"].(float64)
	if height < 5 {
		t.Errorf("height = %v, want >= 5", got["height"])
	}
	created, _ := got["created_at"].(string)
	if created == "" {
		t.Fatal("created_at missing or empty")
	}
	if _, err := time.Parse(time.RFC3339, created); err != nil {
		t.Errorf("created_at %q is not RFC3339: %v", created, err)
	}
}

// TestSessionDescribe_UnknownSessionMapsCode pins the wire contract for
// "describe a session that does not exist": the JSON-RPC error code
// must be errs.CodeSessionNotFound (-32000) so MCP clients can branch
// on a stable code rather than the (version-specific) tmux stderr text.
func TestSessionDescribe_UnknownSessionMapsCode(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Anchor the tmux server with a real session so the controller
	// hits the "server up, named session missing" branch (a fresh
	// controller has no socket file and produces a different
	// "error connecting" message that ListSessions swallows but
	// has-session does not).
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "anchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name":      "session_describe",
		"arguments": map[string]any{"name": "definitely_missing_xyzzy"},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error describing unknown session, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("expected code %d (CodeSessionNotFound), got %d (msg=%q)",
			errs.CodeSessionNotFound, rerr.Code, rerr.Message)
	}
}

// TestSessionDescribe_RejectsInvalidName verifies the input-validation
// path runs before any tmux call is made — bad names yield the standard
// JSON-RPC invalid-params code.
func TestSessionDescribe_RejectsInvalidName(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)

	params := mustJSON(t, map[string]any{
		"name":      "session_describe",
		"arguments": map[string]any{"name": "bad name with spaces"},
	})
	res, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected invalid-params error, got result %#v", res)
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams (%d), got %d", errs.CodeInvalidParams, rerr.Code)
	}
}

// TestSessionDescribe_ListedInTools confirms the init()-time
// registration actually wired session_describe into tools/list.
func TestSessionDescribe_ListedInTools(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] == "session_describe" {
			return
		}
	}
	t.Fatal("tools/list missing session_describe")
}
