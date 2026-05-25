package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_ShowMessages_HeadlessReturnsEmptyList drives the
// load-bearing "no current client" path through the dispatcher. The
// headless tmux servers tmux-mcp owns rarely have a client attached;
// `tmux show-messages` reports "no current client" with rc=1 in that
// case, and the tool must surface a clean empty list rather than an
// error so an agent can introspect at any time without first having
// to attach a client.
func TestHandle_ShowMessages_HeadlessReturnsEmptyList(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the controller's tmux server is
	// definitely up. With nothing attached, show-messages still
	// reports "no current client" — but the boundary returns the
	// empty-list contract.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sm_anchor", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "show_messages", map[string]any{}))
	var obj struct {
		Messages []string `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode show_messages: %v\nbody=%s", err, body)
	}
	if obj.Messages == nil {
		t.Fatalf("expected non-nil messages slice (the wire shape must always be a list, never null); body=%s", body)
	}
	if len(obj.Messages) != 0 {
		t.Fatalf("expected zero messages on a headless server, got %d (%s)", len(obj.Messages), body)
	}
}

// TestHandle_ShowMessages_AcceptsNullArguments guards the "raw is
// empty" branch — the dispatcher hands show_messages a nil-ish payload
// when the caller sends `arguments: {}` or omits the field entirely.
// The handler must accept it as "current client, no flags" rather
// than rejecting it as malformed.
func TestHandle_ShowMessages_AcceptsNullArguments(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sm_any", "command": "/bin/sh",
	})

	// Construct params manually so we can omit the "arguments" key
	// entirely — that's the path that exercises the len(raw) == 0
	// branch in the handler.
	params := mustJSON(t, map[string]any{"name": "show_messages"})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("show_messages: %s", rerr.Message)
	}
	body := extractText(t, res)
	var obj struct {
		Messages []string `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode show_messages: %v\nbody=%s", err, body)
	}
}

// TestHandle_ShowMessages_MissingClientMapsCode pins the wire contract
// that asking for a non-existent client surfaces CodeSessionNotFound
// rather than a generic internal-error code, mirroring every other
// targeted inspection tool. The audit log relies on the typed code
// to record a stable failure category.
func TestHandle_ShowMessages_MissingClientMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so the dispatcher
	// hits the "server is up but the named client does not exist"
	// branch rather than "no server running" (different stderr).
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sm_missing_anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name":      "show_messages",
		"arguments": map[string]any{"client": "/dev/pts/ghost_does_not_exist"},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error for missing client, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d), msg=%q",
			rerr.Code, errs.CodeSessionNotFound, rerr.Message)
	}
}

// TestHandle_ShowMessages_RejectsBadClient guards the regex/length
// policy on the optional `client` argument — even though it's
// optional, a present-but-malformed value must still be refused with
// CodeInvalidParams up front so tmux is never asked to resolve it
// (defence against shell metachars / accidentally-quoted input).
func TestHandle_ShowMessages_RejectsBadClient(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "show_messages",
		"arguments": map[string]any{"client": "bad client with spaces"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad client name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ShowMessages_RejectsUnknownField enforces the
// additionalProperties:false contract on the schema — a typo like
// "client_id" or an attempt to smuggle in a non-listed knob must get
// a fast schema-shaped rejection rather than silently behaving like
// the no-arg variant. We exercise this through the schema entry
// itself (the handler uses a typed struct so extra fields are ignored
// at decode), so we instead assert that the schema entry locks the
// surface for spec-driven clients.
func TestHandle_ShowMessages_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "show_messages" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("show_messages schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		return
	}
	t.Fatalf("tools/list missing show_messages: %v", listing)
}

// TestHandle_ToolsList_IncludesShowMessages makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Mirrors the smoke check every other tool ships
// with — a regression in init() registration would otherwise hide
// the tool from the surface even though the dispatcher case still
// works for a hardcoded call.
func TestHandle_ToolsList_IncludesShowMessages(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "show_messages" {
			return
		}
	}
	t.Fatalf("tools/list missing show_messages")
}

// TestIsReadOnlyTool_AllowsShowMessages is the inverse of the
// rejects-mutators test in readonly_test.go: show_messages is a
// READER, so it must be on the inspection-allowed allowlist. A future
// refactor that drops the entry from readOnlyTools would silently
// break read-only deployments — pinning it here keeps the policy
// uniform with the other introspection tools.
func TestIsReadOnlyTool_AllowsShowMessages(t *testing.T) {
	t.Parallel()
	if !IsReadOnlyTool("show_messages") {
		t.Fatal("IsReadOnlyTool(\"show_messages\") = false, want true (the allowlist must accept this name)")
	}
}
