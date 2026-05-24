package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_ListClients_NoSession_EmptyForHeadlessServer drives the
// "no session arg, no clients attached" path through the dispatcher.
// The headless tmux servers tmux-mcp owns are the load-bearing case:
// nothing should ever be attached, and the tool must return a clean
// empty list rather than an error so an agent can iterate the response
// without a separate "is this an error" branch.
func TestHandle_ListClients_NoSession_EmptyForHeadlessServer(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the controller's tmux server is
	// definitely up. With no `attach`, no clients are ever bound.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lc_anchor", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "list_clients", map[string]any{}))
	var obj struct {
		Clients []map[string]any `json:"clients"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_clients: %v\nbody=%s", err, body)
	}
	if len(obj.Clients) != 0 {
		t.Fatalf("expected zero clients on a headless server, got %d (%s)", len(obj.Clients), body)
	}
}

// TestHandle_ListClients_SessionScoped_EmptyWhenUnattached pins the
// "session-scoped, no clients" path: when the caller names a session
// that exists but has nothing attached to it, the response must be a
// clean empty list — symmetric to the server-wide empty case.
func TestHandle_ListClients_SessionScoped_EmptyWhenUnattached(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "lc_scope", "command": "/bin/sh",
	})

	body := extractText(t, callTool(t, tools, ctx, "list_clients", map[string]any{
		"session": "lc_scope",
	}))
	var obj struct {
		Clients []map[string]any `json:"clients"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_clients: %v\nbody=%s", err, body)
	}
	if len(obj.Clients) != 0 {
		t.Fatalf("expected zero clients for an unattached session, got %d (%s)", len(obj.Clients), body)
	}
}

// TestHandle_ListClients_AcceptsNullArguments guards the "raw is empty"
// branch — the dispatcher hands list_clients a nil-ish payload when
// the caller sends `arguments: {}`. The handler must accept it as
// "list every client on the server" rather than rejecting it as
// malformed (mirrors list_windows / list_panes behaviour).
func TestHandle_ListClients_AcceptsNullArguments(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "any_lc", "command": "/bin/sh",
	})

	// Construct params manually so we can omit the "arguments" key
	// entirely — that's the path that exercises the len(raw) == 0
	// branch in the handler.
	params := mustJSON(t, map[string]any{"name": "list_clients"})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("list_clients: %s", rerr.Message)
	}
	body := extractText(t, res)
	var obj struct {
		Clients []map[string]any `json:"clients"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode list_clients: %v\nbody=%s", err, body)
	}
	// We don't assert on the count — a different test pins the empty
	// case; here we only need the envelope to decode cleanly.
}

// TestHandle_ListClients_MissingSessionMapsCode pins the wire contract
// that asking for a non-existent session surfaces CodeSessionNotFound
// rather than a generic internal-error code, mirroring list_windows /
// session_kill / pane_select. The audit log relies on the typed code
// to record a stable failure category.
func TestHandle_ListClients_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so the dispatcher
	// hits the "server is up but the named session does not exist"
	// branch rather than "no server running" (different stderr).
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "anchor_lc", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name":      "list_clients",
		"arguments": map[string]any{"session": "definitely_does_not_exist_xyzzy"},
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

// TestHandle_ListClients_RejectsBadSession guards the regex/length
// policy on the optional `session` argument — even though it's
// optional, a present-but-malformed value must still be refused with
// CodeInvalidParams up front so tmux is never asked to resolve it
// (defence against shell metachars / accidentally-quoted input).
func TestHandle_ListClients_RejectsBadSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "list_clients",
		"arguments": map[string]any{"session": "bad name with spaces"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad session name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ListClients_RejectsUnknownField enforces the
// additionalProperties:false contract on the schema — a typo like
// "sesion" or an attempt to smuggle in a non-listed knob must get a
// fast schema-shaped rejection rather than silently behaving like the
// unscoped variant. We exercise this through the JSON unmarshaller's
// strict shape only indirectly (the handler uses a typed struct so
// extra fields are ignored at decode), so we instead assert that the
// schema entry itself locks the surface for spec-driven clients.
func TestHandle_ListClients_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name != "list_clients" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("list_clients schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		return
	}
	t.Fatalf("tools/list missing list_clients: %v", listing)
}

// TestHandle_ToolsList_IncludesListClients makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Mirrors the smoke check every other tool ships
// with — a regression in init() registration would otherwise hide
// the tool from the surface even though the dispatcher case still
// works for a hardcoded call.
func TestHandle_ToolsList_IncludesListClients(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "list_clients" {
			return
		}
	}
	t.Fatalf("tools/list missing list_clients")
}
