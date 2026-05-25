package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_SuspendClient_HeadlessNoOp pins the load-bearing
// "headless server, nobody attached" path: a tools/call for
// suspend_client with no target_client must return {"suspended":true}
// rather than an error, because the underlying tmux suspend-client
// emits "no current client" stderr when no clients are attached and
// the boundary maps that onto a clean no-op. This is the common case
// for the headless tmux servers tmux-mcp owns; without this contract
// every caller would have to substring-match tmux stderr to tell the
// "nobody is watching" case apart from a real failure.
func TestHandle_SuspendClient_HeadlessNoOp(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	// Anchor the daemon so the call hits the "server up, no clients"
	// branch rather than "no server running".
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sc_hn", "command": "/bin/sh",
	})

	res := callTool(t, tools, ctx, "suspend_client", map[string]any{})
	body := extractText(t, res)
	var ack struct {
		Suspended bool `json:"suspended"`
	}
	if err := json.Unmarshal([]byte(body), &ack); err != nil {
		t.Fatalf("decode suspend_client response: %v\nbody=%s", err, body)
	}
	if !ack.Suspended {
		t.Fatalf("expected suspended=true on headless no-op, got body=%s", body)
	}
}

// TestHandle_SuspendClient_AbsentArgsObject pins the empty-payload
// path: tmux's suspend-client semantics treat "no target" as
// meaningful (suspend the current client), and on the headless
// servers tmux-mcp owns that resolves to a clean no-op. The handler
// must accept tools/call without an `arguments` field at all and
// produce the same ack as an explicit `{}`.
func TestHandle_SuspendClient_AbsentArgsObject(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sc_ab", "command": "/bin/sh",
	})

	// Build the tools/call payload without an "arguments" key.
	params := mustJSON(t, map[string]any{"name": "suspend_client"})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("suspend_client (no args): %s", rerr.Message)
	}
	body := extractText(t, res)
	var ack struct {
		Suspended bool `json:"suspended"`
	}
	if err := json.Unmarshal([]byte(body), &ack); err != nil {
		t.Fatalf("decode response: %v\nbody=%s", err, body)
	}
	if !ack.Suspended {
		t.Fatalf("expected suspended=true on absent-args call, got body=%s", body)
	}
}

// TestHandle_SuspendClient_MissingTargetMapsCode pins the wire
// contract for "suspend a target that does not exist": the JSON-RPC
// error code must be CodeSessionNotFound (-32000) so MCP clients can
// branch on the same stable code list_clients / session_kill /
// detach_client surface for "the named target is not present".
func TestHandle_SuspendClient_MissingTargetMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the failure surfaces as "client
	// missing" rather than "no server running".
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "sc_mt", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "suspend_client",
		"arguments": map[string]any{
			"target_client": "ghost-client-target-pts99",
		},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error suspending unknown target, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d) (msg=%q)",
			rerr.Code, errs.CodeSessionNotFound, rerr.Message)
	}
}

// TestHandle_SuspendClient_RejectsBadTarget pins the regex/length
// validator on `target_client`. A value with a shell metachar (here a
// space and a backtick) must be refused with CodeInvalidParams up
// front so tmux is never asked to resolve it — defence-in-depth
// against accidentally-quoted input or hostile callers.
func TestHandle_SuspendClient_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "suspend_client",
		"arguments": map[string]any{
			"target_client": "evil target with `backticks`",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad target_client")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "target_client") {
		t.Fatalf("error message %q should mention target_client", rerr.Message)
	}
}

// TestHandle_SuspendClient_RejectsTooLongTarget pins the upper bound
// on the `target_client` argument so an agent cannot smuggle a
// megabyte payload onto tmux's argv before the boundary validates
// anything else.
func TestHandle_SuspendClient_RejectsTooLongTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	huge := make([]byte, maxSuspendClientTargetLen+1)
	for i := range huge {
		huge[i] = 'a'
	}
	params := mustJSON(t, map[string]any{
		"name": "suspend_client",
		"arguments": map[string]any{
			"target_client": string(huge),
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversized target_client")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_SuspendClient_AdditionalPropertiesLocked enforces the
// additionalProperties:false contract on the schema — a typo like
// "client" (instead of "target_client") must surface through the
// schema rather than being silently swallowed at decode time. The
// schema is the surface MCP clients consume, so locking it here means
// a future contributor relaxing the constraint trips this test.
func TestHandle_SuspendClient_AdditionalPropertiesLocked(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] != "suspend_client" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("suspend_client schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props["target_client"]; !ok {
			t.Fatal("suspend_client schema missing `target_client` property")
		}
		// Negative pins: the schema must not expose the alternate
		// names a contributor might reach for if they did not check
		// this test first.
		if _, leaked := props["client"]; leaked {
			t.Fatal("suspend_client schema must not expose a `client` property (use `target_client`)")
		}
		if _, leaked := props["target"]; leaked {
			t.Fatal("suspend_client schema must not expose a `target` property (use `target_client`)")
		}
		return
	}
	t.Fatal("tools/list missing suspend_client")
}

// TestHandle_ToolsList_IncludesSuspendClient makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint. Mirrors the smoke check every other tool ships
// with — a regression in init() registration would otherwise hide
// the tool from the surface even though the dispatcher case still
// works for a hardcoded call.
func TestHandle_ToolsList_IncludesSuspendClient(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] == "suspend_client" {
			return
		}
	}
	t.Fatal("tools/list missing suspend_client")
}

// TestSuspendClient_NotInReadOnlyAllowlist pins the policy:
// suspend_client mutates a client process state (sends SIGTSTP), so a
// -read-only operator must not be able to invoke it. Adding the tool
// to the allowlist would silently let a read-only agent freeze a
// client they only meant to inspect — a strictly less destructive
// counterpart to detach_client, but still mutating.
func TestSuspendClient_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("suspend_client") {
		t.Fatal("suspend_client must NOT be in the read-only allowlist (it sends SIGTSTP)")
	}
}

// TestValidateSuspendClientTarget_Variants pins the regex/length
// policy with a table covering the realistic surface: TTY paths,
// session-qualified names, internal "%client-id" handles, and the
// rejected shapes (spaces, backticks, control bytes). Keeps the
// validator behaviour visible at the source level so a future
// contributor tweaking the regex sees the load-bearing inputs.
func TestValidateSuspendClientTarget_Variants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{"empty allowed", "", false},
		{"tty path", "/dev/pts/3", false},
		{"session reference", "demo", false},
		{"qualified target", "demo:0.1", false},
		{"window-only target", "demo:0", false},
		{"internal handle", "%client-1", false},
		{"alphanumeric mixed", "Demo_42-foo", false},
		{"reject space", "has space", true},
		{"reject backtick", "evil`cmd`", true},
		{"reject quote", "evil'quote", true},
		{"reject newline", "line\nbreak", true},
		{"reject control byte", "ctrl\x01char", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rerr := validateSuspendClientTarget(tc.target)
			if tc.wantErr && rerr == nil {
				t.Fatalf("validateSuspendClientTarget(%q) = nil, want error", tc.target)
			}
			if !tc.wantErr && rerr != nil {
				t.Fatalf("validateSuspendClientTarget(%q) = %v, want nil", tc.target, rerr.Message)
			}
		})
	}
}
