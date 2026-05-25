package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestSessionRename_HappyPath drives the full round-trip: create a
// session, rename it, then confirm session_describe resolves the new
// name and rejects the old one. Mirrors TestSessionDescribe_HappyPath
// so the registration+dispatch path stays observable end-to-end.
func TestSessionRename_HappyPath(t *testing.T) {
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

	const oldName = "rt_old"
	const newName = "rt_new"
	call("session_create", map[string]any{
		"name": oldName, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		// Best-effort cleanup of whichever name is still alive.
		for _, n := range []string{oldName, newName} {
			_, _ = tools.Handle(context.Background(), "tools/call",
				mustJSON(t, map[string]any{
					"name":      "session_kill",
					"arguments": map[string]any{"name": n},
				}))
		}
	})

	body := extractText(t, call("session_rename", map[string]any{
		"name": oldName, "new_name": newName,
	}))
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode session_rename body %q: %v", body, err)
	}
	if got["old_name"] != oldName {
		t.Errorf("old_name = %v, want %q", got["old_name"], oldName)
	}
	if got["new_name"] != newName {
		t.Errorf("new_name = %v, want %q", got["new_name"], newName)
	}

	// Old name must be gone.
	missingParams := mustJSON(t, map[string]any{
		"name":      "session_describe",
		"arguments": map[string]any{"name": oldName},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", missingParams); rerr == nil {
		t.Fatalf("session_describe(%q) should fail after rename", oldName)
	} else if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("session_describe(%q) code = %d, want CodeSessionNotFound (%d)",
			oldName, rerr.Code, errs.CodeSessionNotFound)
	}

	// New name must resolve.
	descBody := extractText(t, call("session_describe", map[string]any{"name": newName}))
	var desc map[string]any
	if err := json.Unmarshal([]byte(descBody), &desc); err != nil {
		t.Fatalf("decode session_describe(%q): %v", newName, err)
	}
	if desc["name"] != newName {
		t.Errorf("describe.name = %v, want %q", desc["name"], newName)
	}

	// session_list must include the new name and exclude the old one.
	listText := extractText(t, call("session_list", map[string]any{}))
	if strings.Contains(listText, `"`+oldName+`"`) {
		t.Errorf("session_list still contains old name %q: %s", oldName, listText)
	}
	if !strings.Contains(listText, `"`+newName+`"`) {
		t.Errorf("session_list missing new name %q: %s", newName, listText)
	}
}

// TestSessionRename_UnknownSourceMapsCode pins the wire contract for
// "rename a session that does not exist": the JSON-RPC error code must
// be CodeSessionNotFound (-32000).
func TestSessionRename_UnknownSourceMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server so we hit "server up, session missing"
	// (a fresh controller produces a different "no server running"
	// message that ListSessions swallows).
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "anchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name": "session_rename",
		"arguments": map[string]any{
			"name": "definitely_missing_xyzzy", "new_name": "anything",
		},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error renaming unknown session, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("expected CodeSessionNotFound (%d), got %d (msg=%q)",
			errs.CodeSessionNotFound, rerr.Code, rerr.Message)
	}
}

// TestSessionRename_DuplicateNewMapsCode covers the collision path on
// the wire: the dispatcher must surface CodeSessionExists (-32004) so
// clients can distinguish "name in use" from the more familiar
// CodeSessionNotFound (-32000).
func TestSessionRename_DuplicateNewMapsCode(t *testing.T) {
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
	call("session_create", map[string]any{"name": "dup_a", "command": "/bin/sh"})
	call("session_create", map[string]any{"name": "dup_b", "command": "/bin/sh"})
	t.Cleanup(func() {
		for _, n := range []string{"dup_a", "dup_b"} {
			_, _ = tools.Handle(context.Background(), "tools/call",
				mustJSON(t, map[string]any{
					"name":      "session_kill",
					"arguments": map[string]any{"name": n},
				}))
		}
	})

	params := mustJSON(t, map[string]any{
		"name":      "session_rename",
		"arguments": map[string]any{"name": "dup_a", "new_name": "dup_b"},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error renaming to a duplicate name, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionExists {
		t.Fatalf("expected CodeSessionExists (%d), got %d (msg=%q)",
			errs.CodeSessionExists, rerr.Code, rerr.Message)
	}
}

// TestSessionRename_RejectsInvalidNewName proves the cheap regex/length
// check on `new_name` runs before any tmux call. Anything that would
// confuse tmux's target parser (spaces, colons, dots) must surface as
// CodeInvalidParams up front.
func TestSessionRename_RejectsInvalidNewName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	cases := []struct {
		name    string
		newName string
	}{
		{"empty", ""},
		{"with spaces", "bad name"},
		{"colon target", "demo:0"},
		{"dot target", "demo.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "session_rename",
				"arguments": map[string]any{
					"name": "demo", "new_name": tc.newName,
				},
			})
			res, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid-params error for new_name=%q, got result %#v",
					tc.newName, res)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
					rerr.Code, errs.CodeInvalidParams, rerr.Message)
			}
		})
	}
}

// TestSessionRename_RejectsInvalidOldName guards the same policy on the
// `name` argument so a malformed source name never reaches tmux.
func TestSessionRename_RejectsInvalidOldName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	params := mustJSON(t, map[string]any{
		"name":      "session_rename",
		"arguments": map[string]any{"name": "bad name", "new_name": "ok"},
	})
	res, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected invalid-params error, got result %#v", res)
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)",
			rerr.Code, errs.CodeInvalidParams)
	}
}

// TestSessionRename_RejectsEqualNames pins the friendly nothing-to-do
// branch — calling rename with the same source and destination is
// almost certainly a bug and we surface a clear -32602 instead of
// letting tmux emit the more-confusing "duplicate session" path that
// would map to CodeSessionExists.
func TestSessionRename_RejectsEqualNames(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	params := mustJSON(t, map[string]any{
		"name":      "session_rename",
		"arguments": map[string]any{"name": "same", "new_name": "same"},
	})
	res, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected invalid-params error, got result %#v", res)
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
}

// TestSessionRename_RejectsAdditionalProperties guards the schema-level
// strictness. With additionalProperties:false any field beyond name /
// new_name must be refused before tmux sees anything; today we only
// enforce that on a typed unmarshal failure, but the dispatcher's
// invalidParams response keeps the wire contract honest.
func TestSessionRename_RejectsMissingArgs(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	// Missing new_name → invalid-params from the up-front empty check.
	params := mustJSON(t, map[string]any{
		"name":      "session_rename",
		"arguments": map[string]any{"name": "ok"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected error when new_name is missing")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
}

// TestSessionRename_ListedInTools confirms the init()-time registration
// actually wired session_rename into tools/list.
func TestSessionRename_ListedInTools(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] == "session_rename" {
			schema, ok := def["inputSchema"].(map[string]any)
			if !ok {
				t.Fatalf("session_rename inputSchema not a map: %#v", def["inputSchema"])
			}
			if got := schema["additionalProperties"]; got != false {
				t.Errorf("session_rename additionalProperties = %v, want false", got)
			}
			req, _ := schema["required"].([]string)
			gotReq := map[string]bool{}
			for _, r := range req {
				gotReq[r] = true
			}
			for _, want := range []string{"name", "new_name"} {
				if !gotReq[want] {
					t.Errorf("session_rename schema missing required=%q (got %v)", want, req)
				}
			}
			return
		}
	}
	t.Fatal("tools/list missing session_rename")
}
