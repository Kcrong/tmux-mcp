package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_LockSession_HappyPath drives the full round-trip through
// the dispatcher: session_create → lock_session → assert the
// `{"locked": true}` ack. tmux's `lock-session -t` against a headless
// server exits 0 because the loop over attached clients is empty —
// the load-bearing case for the headless servers tmux-mcp owns. No
// human-visible state changes (the lock-command never runs without a
// terminal to lock), but the wire contract is the same: tmux returned
// success, so the boundary returns the ack.
func TestHandle_LockSession_HappyPath(t *testing.T) {
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

	const name = "lock_rt"
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

	body := extractText(t, call("lock_session", map[string]any{
		"session": name,
	}))
	var got struct {
		Locked bool `json:"locked"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode lock_session: %v\nbody=%s", err, body)
	}
	if !got.Locked {
		t.Fatalf("lock_session locked flag = false, want true; body=%s", body)
	}
}

// TestHandle_LockSession_RejectsMissingSession pins the required-field
// path: omitting `session` must come back as CodeInvalidParams rather
// than falling through to tmux with an empty -t value (which tmux would
// resolve to whatever session it considers current).
func TestHandle_LockSession_RejectsMissingSession(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "lock_session",
		"arguments": map[string]any{},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing session")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_LockSession_RejectsBadName locks the regex/length check on
// `session` so a stray colon, space, or shell metachar can't slip
// through to the tmux argv.
func TestHandle_LockSession_RejectsBadName(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []struct {
		label, name string
	}{
		{"with spaces", "bad name"},
		{"colon target", "demo:0"},
		{"dot target", "demo.0"},
		{"shell metachar", "demo;rm"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			params := mustJSON(t, map[string]any{
				"name":      "lock_session",
				"arguments": map[string]any{"session": tc.name},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params error for session=%q", tc.name)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_LockSession_MissingSessionMapsCode pins the wire contract
// that lock_session against an unknown session surfaces
// CodeSessionNotFound (-32000), mirroring session_kill / clear_history.
func TestHandle_LockSession_MissingSessionMapsCode(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, session
	// missing" rather than "no server" (different stderr shape).
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "anchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name": "lock_session",
		"arguments": map[string]any{
			"session": "definitely_does_not_exist_xyzzy",
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

// TestHandle_LockSession_RejectsAdditionalProperties guards the schema-
// level strictness: with additionalProperties:false a stray field beyond
// `session` must be refused before tmux is consulted. The dispatcher's
// json.Unmarshal alone tolerates unknown fields, so this test exercises
// the round-trip through validateSessionName for `name=""` (the field
// the schema does not declare) — which today comes back as
// invalid-params from the up-front empty-session guard, keeping the
// wire contract uniform.
func TestHandle_LockSession_RejectsExtraField(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	// Send `name` instead of `session` (typo case the schema would
	// reject if a strict JSON-schema validator ran) — the unmarshal
	// silently drops it and the empty-session guard fires.
	params := mustJSON(t, map[string]any{
		"name":      "lock_session",
		"arguments": map[string]any{"name": "demo"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error when session field is missing")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ToolsList_IncludesLockSession makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Also verifies the schema is locked
// (additionalProperties=false, required=["session"]).
func TestHandle_ToolsList_IncludesLockSession(t *testing.T) {
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] == "lock_session" {
			schema, ok := def["inputSchema"].(map[string]any)
			if !ok {
				t.Fatalf("lock_session inputSchema not a map: %#v", def["inputSchema"])
			}
			if got := schema["additionalProperties"]; got != false {
				t.Errorf("lock_session additionalProperties = %v, want false", got)
			}
			req, _ := schema["required"].([]string)
			if len(req) != 1 || req[0] != "session" {
				t.Errorf("lock_session required = %v, want [session]", req)
			}
			return
		}
	}
	t.Fatalf("tools/list missing lock_session")
}

// TestLockSession_NotInReadOnlyAllowlist pins the policy decision in
// readonly.go: lock_session mutates the visible state of every attached
// client (the lock screen replaces whatever they were watching), so it
// must NOT be reachable when the operator armed -read-only. A future
// contributor adding lock_session to the allowlist by mistake would
// trip this test.
func TestLockSession_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("lock_session") {
		t.Fatal("IsReadOnlyTool(\"lock_session\") = true, want false " +
			"(lock-session writes the lock screen to attached clients and must not be inspection-allowed)")
	}
}
