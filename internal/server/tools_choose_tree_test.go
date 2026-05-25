package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_ChooseTree_DefaultScopeListsEverything drives the happy
// "no args" path: with two sessions on the server, the default
// (scope=all) snapshot must surface windows from both — symmetric to
// list_windows's no-args contract.
func TestHandle_ChooseTree_DefaultScopeListsEverything(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	for _, name := range []string{"ct1", "ct2"} {
		callTool(t, tools, ctx, "session_create", map[string]any{
			"name": name, "command": "/bin/sh",
		})
	}

	body := extractText(t, callTool(t, tools, ctx, "choose_tree", map[string]any{}))
	var obj struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode choose_tree: %v\nbody=%s", err, body)
	}
	if len(obj.Rows) < 2 {
		t.Fatalf("expected at least 2 rows across 2 sessions, got %d (%s)", len(obj.Rows), body)
	}
	seen := map[string]bool{}
	for _, r := range obj.Rows {
		if s, _ := r["session"].(string); s != "" {
			seen[s] = true
		}
		if _, ok := r["window_index"].(float64); !ok {
			t.Errorf("row missing window_index: %v", r)
		}
		if _, ok := r["window_name"].(string); !ok {
			t.Errorf("row missing window_name: %v", r)
		}
		if _, ok := r["pane_count"].(float64); !ok {
			t.Errorf("row missing pane_count: %v", r)
		}
		if _, ok := r["active"].(bool); !ok {
			t.Errorf("row missing active flag: %v", r)
		}
	}
	for _, n := range []string{"ct1", "ct2"} {
		if !seen[n] {
			t.Errorf("missing session %q in choose_tree snapshot: %s", n, body)
		}
	}
}

// TestHandle_ChooseTree_SessionScope pins the session-scoped path: a
// choose_tree call with scope="session" and a session name must return
// only windows of that session.
func TestHandle_ChooseTree_SessionScope(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "ctmain", "command": "/bin/sh",
	})
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "ctother", "command": "/bin/sh",
	})
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "ctmain", "name": "side", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "choose_tree", map[string]any{
		"scope": "session", "session": "ctmain",
	}))
	var obj struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode choose_tree: %v\nbody=%s", err, body)
	}
	if len(obj.Rows) != 2 {
		t.Fatalf("expected 2 rows for ctmain, got %d (%s)", len(obj.Rows), body)
	}
	for _, r := range obj.Rows {
		if s, _ := r["session"].(string); s != "ctmain" {
			t.Errorf("row session = %q, want ctmain", s)
		}
	}
}

// TestHandle_ChooseTree_WindowScope pins the window-scoped path: a
// choose_tree call with scope="window" and (session, window) must
// return exactly one row matching that window.
func TestHandle_ChooseTree_WindowScope(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "ctws", "command": "/bin/sh",
	})
	callTool(t, tools, ctx, "window_create", map[string]any{
		"session": "ctws", "name": "build", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "choose_tree", map[string]any{
		"scope": "window", "session": "ctws", "window": "build",
	}))
	var obj struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode choose_tree: %v\nbody=%s", err, body)
	}
	if len(obj.Rows) != 1 {
		t.Fatalf("expected exactly 1 row for window scope, got %d (%s)", len(obj.Rows), body)
	}
	r := obj.Rows[0]
	if s, _ := r["session"].(string); s != "ctws" {
		t.Errorf("row session = %q, want ctws", s)
	}
	if name, _ := r["window_name"].(string); name != "build" {
		t.Errorf("row window_name = %q, want build", name)
	}
}

// TestHandle_ChooseTree_MissingSessionMapsCode pins the wire contract
// that asking for a non-existent session surfaces CodeSessionNotFound
// rather than a generic internal-error code, mirroring list_windows /
// list_clients / session_kill.
func TestHandle_ChooseTree_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "ctanchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "choose_tree",
		"arguments": map[string]any{
			"scope": "session", "session": "definitely_does_not_exist_xyzzy",
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

// TestHandle_ChooseTree_RejectsBadSession guards the regex/length
// policy on the `session` argument when a scope that requires it is
// passed. Even though scope=session needs a name, a malformed value
// must be refused with CodeInvalidParams up front so tmux is never
// asked to resolve it.
func TestHandle_ChooseTree_RejectsBadSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "choose_tree",
		"arguments": map[string]any{
			"scope": "session", "session": "bad name with spaces",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad session name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ChooseTree_RejectsBadWindow guards the regex/length
// policy on the `window` argument under scope="window". A typo / shell
// metachar in the window name must be refused before tmux is consulted.
func TestHandle_ChooseTree_RejectsBadWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "choose_tree",
		"arguments": map[string]any{
			"scope": "window", "session": "ok", "window": "bad win",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad window name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ChooseTree_SessionScopeRequiresSession pins the
// "scope=session needs a name" branch: omitting `session` must fail
// with CodeInvalidParams rather than silently falling through to the
// unscoped form.
func TestHandle_ChooseTree_SessionScopeRequiresSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "choose_tree",
		"arguments": map[string]any{
			"scope": "session",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for scope=session without a session")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ChooseTree_WindowScopeRequiresWindow pins the
// "scope=window needs a window" branch: a window scope without the
// `window` argument must fail with CodeInvalidParams.
func TestHandle_ChooseTree_WindowScopeRequiresWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "choose_tree",
		"arguments": map[string]any{
			"scope": "window", "session": "ok",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for scope=window without a window")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ChooseTree_AllScopeRejectsSession pins the inverse of the
// session-scope contract: a caller that meant "session" but forgot to
// flip `scope` should not silently get a server-wide listing — pass
// session + scope=all and we error out so the typo is loud.
func TestHandle_ChooseTree_AllScopeRejectsSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "choose_tree",
		"arguments": map[string]any{
			"scope": "all", "session": "demo",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for scope=all with session set")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ChooseTree_AcceptsNullArguments guards the "raw is empty"
// branch — the dispatcher hands choose_tree a nil-ish payload when
// the caller sends `arguments: {}`. The handler must accept it as the
// default scope=all branch rather than rejecting it as malformed.
func TestHandle_ChooseTree_AcceptsNullArguments(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "ctnull", "command": "/bin/sh",
	})

	// Construct params manually so we can omit the "arguments" key
	// entirely — that's the path that exercises the len(raw) == 0
	// branch in the handler.
	params := mustJSON(t, map[string]any{"name": "choose_tree"})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("choose_tree: %s", rerr.Message)
	}
	body := extractText(t, res)
	var obj struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode choose_tree: %v\nbody=%s", err, body)
	}
	if len(obj.Rows) == 0 {
		t.Fatalf("expected at least one row, got empty body=%s", body)
	}
}

// TestHandle_ToolsList_IncludesChooseTree makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Mirrors the smoke check every other tool ships
// with — a regression in init() registration would otherwise hide the
// tool from the surface even though the dispatcher case still works
// for a hardcoded call.
func TestHandle_ToolsList_IncludesChooseTree(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "choose_tree" {
			schema, _ := def["inputSchema"].(map[string]any)
			// additionalProperties:false is part of the contract — an
			// agent that misnames a field gets a fast schema-shaped
			// rejection rather than a silent no-op.
			if got, ok := schema["additionalProperties"].(bool); !ok || got {
				t.Errorf("choose_tree schema additionalProperties = %v, want false", schema["additionalProperties"])
			}
			props, _ := schema["properties"].(map[string]any)
			scope, _ := props["scope"].(map[string]any)
			if scope == nil {
				t.Fatalf("choose_tree schema missing 'scope' property: %v", props)
			}
			if def, _ := scope["default"].(string); def != "all" {
				t.Errorf("choose_tree scope.default = %q, want all", def)
			}
			enum, _ := scope["enum"].([]string)
			if len(enum) != 3 {
				t.Errorf("choose_tree scope.enum = %v, want 3 entries", scope["enum"])
			}
			return
		}
	}
	t.Fatalf("tools/list missing choose_tree: %v", listing)
}
