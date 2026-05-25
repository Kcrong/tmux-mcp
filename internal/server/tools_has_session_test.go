package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHasSession_TrueForExistingSession exercises the happy path: after
// session_create, has_session must answer `{"exists": true}` with no
// JSON-RPC error. We pin the full envelope shape so a future contributor
// who refactors jsonBlock cannot silently drop the boolean.
func TestHasSession_TrueForExistingSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	const name = "has_true"
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

	body := extractText(t, call("has_session", map[string]any{"name": name}))
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode has_session body %q: %v", body, err)
	}
	exists, ok := got["exists"].(bool)
	if !ok {
		t.Fatalf("exists field missing or wrong type: %#v", got)
	}
	if !exists {
		t.Fatalf("exists = false for live session %q; body=%s", name, body)
	}
}

// TestHasSession_FalseForMissingSession is the load-bearing contract:
// a session that does not exist must come back as `{"exists": false}`
// with NO JSON-RPC error. This is what makes has_session useful — an
// agent can ask "is X there?" without first having to catch a -32000.
//
// We anchor the tmux server with a real session so the controller hits
// the "server up, named session missing" branch (a fresh controller
// has no socket file at all and produces a different "error
// connecting" message; the contract still has to hold there too, so
// the second test below covers it).
func TestHasSession_FalseForMissingSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "anchor_has", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name":      "has_session",
		"arguments": map[string]any{"name": "definitely_missing_xyzzy"},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("has_session must NOT error on a missing session; got code=%d msg=%q",
			rerr.Code, rerr.Message)
	}
	body := extractText(t, res)
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode has_session body %q: %v", body, err)
	}
	exists, ok := got["exists"].(bool)
	if !ok {
		t.Fatalf("exists field missing or wrong type: %#v", got)
	}
	if exists {
		t.Fatalf("exists = true for missing session; body=%s", body)
	}
}

// TestHasSession_FalseOnEmptyServer pins the cold-start case: a fresh
// controller whose tmux server has not been spawned yet must still
// answer `{"exists": false}` cleanly. ListSessions absorbs the "no
// server running" / "error connecting" / "No such file or directory"
// stderr forms, so HasSession sees the empty-list branch and returns
// false-without-error — and that has to round-trip to the JSON-RPC
// envelope unchanged.
func TestHasSession_FalseOnEmptyServer(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	params := mustJSON(t, map[string]any{
		"name":      "has_session",
		"arguments": map[string]any{"name": "no_server_yet"},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("has_session on empty server must NOT error; got code=%d msg=%q",
			rerr.Code, rerr.Message)
	}
	body := extractText(t, res)
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode has_session body %q: %v", body, err)
	}
	exists, ok := got["exists"].(bool)
	if !ok {
		t.Fatalf("exists field missing or wrong type: %#v", got)
	}
	if exists {
		t.Fatalf("exists = true for missing session on empty server; body=%s", body)
	}
}

// TestHasSession_RejectsInvalidName verifies the validator runs before
// any tmux call: the "bad name" inputs that validateSessionName
// rejects must surface as -32602, not as an "exists: false" answer.
// Pin a few representative shapes (empty, regex violation, oversize)
// so a future loosening of the regex still has to account for these.
func TestHasSession_RejectsInvalidName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)

	cases := []struct {
		label string
		name  string
		sub   string
	}{
		{"empty", "", "session name required"},
		{"with spaces", "bad name with spaces", "must match"},
		{"colon", "demo:colon", "must match"},
		{"too long", strings.Repeat("a", 65), "out of range"},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			tools := &Tools{}
			params := mustJSON(t, map[string]any{
				"name":      "has_session",
				"arguments": map[string]any{"name": tc.name},
			})
			res, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid-params error, got result %#v", res)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("expected CodeInvalidParams (%d), got %d (msg=%q)",
					errs.CodeInvalidParams, rerr.Code, rerr.Message)
			}
			if !strings.Contains(rerr.Message, tc.sub) {
				t.Fatalf("error message %q missing %q", rerr.Message, tc.sub)
			}
		})
	}
}

// TestHasSession_ListedInTools confirms the init()-time registration
// actually wired has_session into tools/list — without this, an MCP
// client running tools/list would not even see the surface.
func TestHasSession_ListedInTools(t *testing.T) {
	t.Parallel()
	tools := &Tools{}
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] != "has_session" {
			continue
		}
		// Pin the load-bearing schema bits so a future schema edit that
		// drops "name" or relaxes additionalProperties has to update
		// this test too.
		schema, _ := def["inputSchema"].(map[string]any)
		if schema == nil {
			t.Fatalf("inputSchema missing on has_session def: %#v", def)
		}
		req, _ := schema["required"].([]string)
		if len(req) != 1 || req[0] != "name" {
			t.Fatalf("required = %#v, want [\"name\"]", schema["required"])
		}
		if addl, ok := schema["additionalProperties"].(bool); !ok || addl {
			t.Fatalf("additionalProperties must be false; got %#v", schema["additionalProperties"])
		}
		return
	}
	t.Fatal("tools/list missing has_session")
}

// TestHasSession_AllowedUnderReadOnly is the read-only contract: the
// allowlist in readonly.go must include has_session so a server
// running with -read-only still exposes the existence probe. Without
// this pin, the entry could silently fall out of the table during a
// later refactor.
func TestHasSession_AllowedUnderReadOnly(t *testing.T) {
	t.Parallel()
	if !IsReadOnlyTool("has_session") {
		t.Fatal("IsReadOnlyTool(\"has_session\") = false; allowlist must accept it")
	}
}
