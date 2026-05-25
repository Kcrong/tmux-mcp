package server

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// skipIfNoServerAccessIntegration gates the dispatcher-level cycle
// test on the same TMUX_MCP_TEST_SERVER_ACCESS opt-in the controller
// tests use. Tests that only exercise validation / schema shape do
// NOT need the env var; only the live add/delete cycle does.
func skipIfNoServerAccessIntegration(t *testing.T) string {
	t.Helper()
	skipIfNoTmux(t)
	user := strings.TrimSpace(os.Getenv("TMUX_MCP_TEST_SERVER_ACCESS"))
	if user == "" {
		t.Skip("set TMUX_MCP_TEST_SERVER_ACCESS to a real OS username " +
			"to enable server_access integration tests")
	}
	return user
}

// TestHandle_ServerAccess_ListHeadless pins the load-bearing
// "no daemon" contract end-to-end: a tools/call for op=list against a
// freshly constructed *Tools (no anchoring session) must come back
// with `{"entries":[]}` rather than an error. Without this branch
// every caller would have to write its own "is the server up?" probe
// before invoking the tool.
func TestHandle_ServerAccess_ListHeadless(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	res := callTool(t, tools, ctx, "server_access", map[string]any{
		"op": "list",
	})
	body := extractText(t, res)
	var got struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	if got.Entries == nil {
		t.Fatalf("entries must be a non-nil slice (was nil); body=%q", body)
	}
	if len(got.Entries) != 0 {
		t.Fatalf("expected empty entries on headless tool, got %v", got.Entries)
	}
}

// TestHandle_ServerAccess_RejectsMissingOp pins the schema's `op`
// requirement. Calls without an op must come back as -32602 before
// the dispatcher even reaches tmux — every other op-style tool on
// the surface enforces the same up-front check.
func TestHandle_ServerAccess_RejectsMissingOp(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "server_access",
		"arguments": map[string]any{},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing op")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "op required") {
		t.Fatalf("unexpected message: %q", rerr.Message)
	}
}

// TestHandle_ServerAccess_RejectsUnknownOp guards the enum check on
// `op`. A typo (e.g. `"deny"`) must be refused before any tmux call
// happens — this is the only line of defence against a future agent
// inventing op names that mean different things across deployments.
func TestHandle_ServerAccess_RejectsUnknownOp(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "server_access",
		"arguments": map[string]any{
			"op":   "deny",
			"user": "alice",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for unknown op")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ServerAccess_RejectsEmptyUserOnAdd pins the per-op
// "user required" guard for add (and, by extension, every mutating
// op). The handler runs the validator branch-by-branch so a regression
// where add silently no-op'd with no user would surface here as a
// successful call.
func TestHandle_ServerAccess_RejectsEmptyUserOnAdd(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	for _, op := range []string{"add", "delete", "read_only", "write"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "server_access",
				"arguments": map[string]any{
					"op": op,
				},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("op=%s: expected invalid params for empty user", op)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("op=%s: code = %d, want CodeInvalidParams (%d)", op, rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_ServerAccess_ListRejectsUserField pins the inverse
// constraint: op=list must not carry a `user`. Both shapes (user set
// alongside op=list) would silently no-op on tmux's CLI — the
// boundary refuses up front so a buggy caller does not leave thinking
// they just listed access for one specific user.
func TestHandle_ServerAccess_ListRejectsUserField(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "server_access",
		"arguments": map[string]any{
			"op":   "list",
			"user": "alice",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for op=list with user")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ServerAccess_RejectsBadUser pins the regex/length policy
// at the dispatcher boundary. Each case names the category that the
// validator must catch — a stray quote, leading digit, control byte,
// uppercase letter, and an over-length input. Failure points at the
// specific sub-case so the regression diagnostic is focused.
func TestHandle_ServerAccess_RejectsBadUser(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := map[string]string{
		"shell metachar":     `bob"; rm -rf /tmp`,
		"leading digit":      "1user",
		"control byte":       "bob\x01evil",
		"uppercase rejected": "Bob",
		"too long":           strings.Repeat("a", maxServerAccessUserLen+1),
	}
	for label, name := range cases {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "server_access",
				"arguments": map[string]any{
					"op":   "add",
					"user": name,
				},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params for %s (%q)", label, name)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_ServerAccess_AdditionalPropertiesLocked enforces the
// schema's additionalProperties:false contract. A typo like
// `"username"` instead of `"user"` must surface through the schema
// rather than being silently swallowed at decode time, so a future
// contributor relaxing the lock trips this test alongside the
// dispatcher's behavioural ones.
func TestHandle_ServerAccess_AdditionalPropertiesLocked(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] != "server_access" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("server_access schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		props, _ := schema["properties"].(map[string]any)
		for _, want := range []string{"op", "user"} {
			if _, ok := props[want]; !ok {
				t.Fatalf("server_access schema missing property %q", want)
			}
		}
		// `op` is the sole required field — `user` is per-op enforced
		// by the handler because plain JSON Schema cannot express
		// "required only when op != 'list'".
		req, _ := schema["required"].([]string)
		if len(req) != 1 || req[0] != "op" {
			t.Fatalf("server_access required = %v, want [op]", req)
		}
		// Confirm the enum carries every accepted op so a future
		// contributor adding a new op has to update both the schema
		// and the handler in one PR.
		opSchema, _ := props["op"].(map[string]any)
		enum, _ := opSchema["enum"].([]string)
		want := map[string]bool{"add": true, "delete": true, "list": true, "read_only": true, "write": true}
		if len(enum) != len(want) {
			t.Fatalf("op enum length = %d, want %d (got %v)", len(enum), len(want), enum)
		}
		for _, e := range enum {
			if !want[e] {
				t.Fatalf("op enum has unexpected value %q (must be one of %v)", e, want)
			}
			delete(want, e)
		}
		if len(want) != 0 {
			t.Fatalf("op enum missing values: %v", want)
		}
		return
	}
	t.Fatal("tools/list missing server_access")
}

// TestHandle_ToolsList_IncludesServerAccess makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Mirrors the smoke check every other tool ships
// with — a regression in init() registration would otherwise hide
// the tool from the surface even though the dispatcher case still
// works for a hardcoded call.
func TestHandle_ToolsList_IncludesServerAccess(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] == "server_access" {
			return
		}
	}
	t.Fatal("tools/list missing server_access")
}

// TestServerAccess_NotInReadOnlyAllowlist pins the policy: the whole
// server_access tool surface is gated as a unit because most ops
// mutate state. Adding it to the read-only allowlist would let a
// -read-only operator invoke `op=add` / `op=delete` even though the
// schema's enum exposes both alongside `op=list`.
func TestServerAccess_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("server_access") {
		t.Fatal("server_access must NOT be in the read-only allowlist (op=add/delete/read_only/write all mutate)")
	}
}

// TestHandle_ServerAccess_AddCycle exercises the live add → list →
// write → list → read_only → list → delete → list cycle through the
// dispatcher. Gated on TMUX_MCP_TEST_SERVER_ACCESS so unattended
// runners skip cleanly. This is the only test that flips a real OS
// user's permission, hence the env-var opt-in.
func TestHandle_ServerAccess_AddCycle(t *testing.T) {
	t.Parallel()
	peer := skipIfNoServerAccessIntegration(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	// Anchor the daemon — server-access requires it to be running.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sa_cycle", "command": "/bin/sh",
	})

	// Pre-clean any leaked entry from a previous run. tmux returns an
	// error when the entry is missing, so the result is ignored — the
	// goal state is "user not in list".
	_, _ = tools.Handle(ctx, "tools/call", mustJSON(t, map[string]any{
		"name":      "server_access",
		"arguments": map[string]any{"op": "delete", "user": peer},
	}))
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = tools.Handle(cleanupCtx, "tools/call", mustJSON(t, map[string]any{
			"name":      "server_access",
			"arguments": map[string]any{"op": "delete", "user": peer},
		}))
	})

	mustOK := func(label, op string) {
		t.Helper()
		res := callTool(t, tools, ctx, "server_access", map[string]any{
			"op": op, "user": peer,
		})
		body := extractText(t, res)
		var ack struct {
			OK   bool   `json:"ok"`
			Op   string `json:"op"`
			User string `json:"user"`
		}
		if err := json.Unmarshal([]byte(body), &ack); err != nil {
			t.Fatalf("%s: decode %q: %v", label, body, err)
		}
		if !ack.OK || ack.Op != op || ack.User != peer {
			t.Fatalf("%s: ack=%+v want ok=true op=%s user=%s", label, ack, op, peer)
		}
	}

	listEntries := func(label string) []map[string]any {
		t.Helper()
		res := callTool(t, tools, ctx, "server_access", map[string]any{"op": "list"})
		body := extractText(t, res)
		var got struct {
			Entries []map[string]any `json:"entries"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("%s: decode %q: %v", label, body, err)
		}
		return got.Entries
	}

	mustOK("add", "add")
	if !findEntryWith(listEntries("after-add"), peer, "R") {
		t.Fatalf("after add: expected R for %q in %v", peer, listEntries("after-add-debug"))
	}
	mustOK("write", "write")
	if !findEntryWith(listEntries("after-write"), peer, "R/W") {
		t.Fatalf("after write: expected R/W for %q in %v", peer, listEntries("after-write-debug"))
	}
	mustOK("read_only", "read_only")
	if !findEntryWith(listEntries("after-readonly"), peer, "R") {
		t.Fatalf("after read_only: expected R for %q in %v", peer, listEntries("after-readonly-debug"))
	}
	mustOK("delete", "delete")
	for _, e := range listEntries("after-delete") {
		if u, _ := e["user"].(string); u == peer {
			t.Fatalf("entry for %q still present after delete: %v", peer, e)
		}
	}
}

// findEntryWith walks the JSON-decoded entries slice for a row whose
// `user` matches and `permission` token equals perm. Pulled out so the
// cycle test stays readable — every assertion uses the same observation
// surface.
func findEntryWith(entries []map[string]any, user, perm string) bool {
	for _, e := range entries {
		u, _ := e["user"].(string)
		p, _ := e["permission"].(string)
		if u == user && p == perm {
			return true
		}
	}
	return false
}
