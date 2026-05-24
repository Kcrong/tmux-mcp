package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestSessionInspect_HappyPath exercises the full round-trip through
// the JSON-RPC dispatcher: create a session, inspect it, then assert
// the response envelope decodes cleanly and every field looks sensible.
func TestSessionInspect_HappyPath(t *testing.T) {
	t.Parallel()
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

	const name = "inspect_rt"
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

	body := extractText(t, call("session_inspect", map[string]any{"session": name}))

	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode session_inspect body %q: %v", body, err)
	}

	if got["name"] != name {
		t.Errorf("name = %v, want %q", got["name"], name)
	}
	// JSON numbers decode as float64.
	pid, _ := got["pid"].(float64)
	if pid <= 0 {
		t.Errorf("pid = %v, want > 0", got["pid"])
	}
	cwd, _ := got["cwd"].(string)
	if cwd == "" {
		t.Error("cwd missing or empty")
	}
	if cwd != "" && !filepath.IsAbs(cwd) {
		t.Errorf("cwd = %q, want absolute path", cwd)
	}
	cmd, _ := got["command"].(string)
	if cmd == "" {
		t.Error("command missing or empty")
	}

	// Inspect deliberately omits environment variables — the schema
	// has no env field and the response object must not start
	// surfacing one without a corresponding security review.
	if _, leaked := got["env"]; leaked {
		t.Errorf("session_inspect must not return env (got key: %v)", got["env"])
	}
}

// TestSessionInspect_UnknownSessionMapsCode pins the wire contract for
// "inspect a session that does not exist": the JSON-RPC error code
// must be errs.CodeSessionNotFound (-32000) so MCP clients can branch
// on a stable code rather than the (version-specific) tmux stderr text.
func TestSessionInspect_UnknownSessionMapsCode(t *testing.T) {
	t.Parallel()
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
		"arguments": map[string]any{"name": "anchor_inspect", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name":      "session_inspect",
		"arguments": map[string]any{"session": "definitely_missing_xyzzy"},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error inspecting unknown session, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("expected code %d (CodeSessionNotFound), got %d (msg=%q)",
			errs.CodeSessionNotFound, rerr.Code, rerr.Message)
	}
}

// TestSessionInspect_RejectsInvalidName verifies the input-validation
// path runs before any tmux call is made — bad session refs yield the
// standard JSON-RPC invalid-params code.
func TestSessionInspect_RejectsInvalidName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	params := mustJSON(t, map[string]any{
		"name":      "session_inspect",
		"arguments": map[string]any{"session": "bad name with spaces"},
	})
	res, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected invalid-params error, got result %#v", res)
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams (%d), got %d", errs.CodeInvalidParams, rerr.Code)
	}
}

// TestSessionInspect_ListedInTools confirms the init()-time
// registration actually wired session_inspect into tools/list.
func TestSessionInspect_ListedInTools(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] == "session_inspect" {
			return
		}
	}
	t.Fatal("tools/list missing session_inspect")
}
