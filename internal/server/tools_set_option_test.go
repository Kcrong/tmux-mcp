package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// decodeSetOption pulls the {"set": ..., "unset": ..., "name": ..., "scope": ...}
// envelope out of the tools/call result so the assertions below stay
// focused on the field that matters for each scenario.
func decodeSetOption(t *testing.T, result any) (set, unset bool, name, scope string) {
	t.Helper()
	body := extractText(t, result)
	var obj struct {
		Set   bool   `json:"set"`
		Unset bool   `json:"unset"`
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode set_option body %q: %v", body, err)
	}
	return obj.Set, obj.Unset, obj.Name, obj.Scope
}

// TestHandle_SetOption_ServerScopeRoundTrip drives the dispatcher's
// scope=server happy path: set buffer-limit=75 via the JSON-RPC
// boundary, then read it back via the show_options tool to confirm the
// override actually landed on the server. buffer-limit is a long-
// standing tmux server option that accepts integer values across every
// supported version, so the assertion does not depend on a specific
// tmux version's defaults.
func TestHandle_SetOption_ServerScopeRoundTrip(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	const anchor = "opt_srv_anchor"
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": anchor, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": anchor},
			}))
	})

	set, unset, name, scope := decodeSetOption(t, callTool(t, tools, ctx, "set_option", map[string]any{
		"name":  "buffer-limit",
		"value": "75",
		"scope": "server",
	}))
	if !set || unset {
		t.Fatalf("set_option(server) envelope: set=%v unset=%v want true/false", set, unset)
	}
	if name != "buffer-limit" || scope != "server" {
		t.Fatalf("set_option(server) echo: name=%q scope=%q want buffer-limit/server", name, scope)
	}

	// Round-trip via show_options: the override should be visible in
	// the server-scope option map.
	body := extractText(t, callTool(t, tools, ctx, "show_options", map[string]any{
		"scope": "server",
	}))
	var got struct {
		Options map[string]string `json:"options"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode show_options body %q: %v", body, err)
	}
	if got.Options["buffer-limit"] != "75" {
		t.Fatalf("buffer-limit = %q, want 75 (full options=%v)", got.Options["buffer-limit"], got.Options)
	}
}

// TestHandle_SetOption_SessionScopeRoundTrip pins the dispatcher's
// scope=session happy path: set status-interval on a freshly created
// session, then assert show_options (without -g) reports the override.
// The default scope (session) is also exercised implicitly — omitting
// `scope` should land us in the session branch.
func TestHandle_SetOption_SessionScopeRoundTrip(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	const sess = "opt_sess_target"
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": sess, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": sess},
			}))
	})

	// Default scope path: omit `scope` entirely so the handler's
	// "default to session" branch is exercised.
	set, unset, name, scope := decodeSetOption(t, callTool(t, tools, ctx, "set_option", map[string]any{
		"name":   "status-interval",
		"value":  "7",
		"target": sess,
	}))
	if !set || unset {
		t.Fatalf("set_option(default) envelope: set=%v unset=%v want true/false", set, unset)
	}
	if name != "status-interval" {
		t.Fatalf("name echo = %q, want status-interval", name)
	}
	if scope != "session" {
		t.Fatalf("scope echo = %q, want session (default)", scope)
	}

	body := extractText(t, callTool(t, tools, ctx, "show_options", map[string]any{
		"scope":   "session",
		"session": sess,
	}))
	var got struct {
		Options map[string]string `json:"options"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode show_options body %q: %v", body, err)
	}
	if got.Options["status-interval"] != "7" {
		t.Fatalf("status-interval = %q, want 7 (full options=%v)", got.Options["status-interval"], got.Options)
	}
}

// TestHandle_SetOption_UnsetClearsOverride drives the unset=true path:
// set an override, confirm it is visible, then unset and confirm it is
// gone. The envelope must report `unset: true, set: false` so a caller
// inspecting the response knows which branch was taken.
func TestHandle_SetOption_UnsetClearsOverride(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	const sess = "opt_unset_target"
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": sess, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": sess},
			}))
	})

	// Apply an override first.
	callTool(t, tools, ctx, "set_option", map[string]any{
		"name":   "status-interval",
		"value":  "9",
		"scope":  "session",
		"target": sess,
	})

	// Now unset.
	set, unset, _, _ := decodeSetOption(t, callTool(t, tools, ctx, "set_option", map[string]any{
		"name":   "status-interval",
		"scope":  "session",
		"target": sess,
		"unset":  true,
	}))
	if set || !unset {
		t.Fatalf("envelope after unset: set=%v unset=%v want false/true", set, unset)
	}

	body := extractText(t, callTool(t, tools, ctx, "show_options", map[string]any{
		"scope":   "session",
		"session": sess,
	}))
	var got struct {
		Options map[string]string `json:"options"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode show_options body %q: %v", body, err)
	}
	if v, present := got.Options["status-interval"]; present {
		t.Fatalf("status-interval still present after unset = %q (full options=%v)", v, got.Options)
	}
}

// TestHandle_SetOption_RejectsMissingName pins the up-front guard:
// without `name` the dispatcher must fail with CodeInvalidParams
// (-32602) before any tmux command runs.
func TestHandle_SetOption_RejectsMissingName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "set_option",
		"arguments": map[string]any{
			"value": "x",
			"scope": "server",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for missing name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_SetOption_RejectsBadName locks the regex check on `name`
// so a stray quote or whitespace cannot slip through to tmux's argv.
func TestHandle_SetOption_RejectsBadName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "set_option",
		"arguments": map[string]any{
			"name":  "bad name with spaces",
			"value": "x",
			"scope": "server",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for bad name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_SetOption_RejectsBadScope guards the enum on `scope`. A
// caller passing an unknown scope must see CodeInvalidParams before any
// tmux command runs.
func TestHandle_SetOption_RejectsBadScope(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "set_option",
		"arguments": map[string]any{
			"name":  "anything",
			"value": "x",
			"scope": "elsewhere",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for unknown scope")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_SetOption_RejectsMissingTargetForSession pins the
// session-scope contract: the handler must reject the call up front
// when `target` is omitted, before reaching tmux.
func TestHandle_SetOption_RejectsMissingTargetForSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "set_option",
		"arguments": map[string]any{
			"name":  "status-interval",
			"value": "5",
			"scope": "session",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for missing target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_SetOption_RejectsOversizedValue enforces the 4 KiB cap on
// `value`. A 4097-byte payload must fail with CodeInvalidParams before
// any tmux command runs.
func TestHandle_SetOption_RejectsOversizedValue(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	oversized := strings.Repeat("a", maxSetOptionValueLen+1)
	params := mustJSON(t, map[string]any{
		"name": "set_option",
		"arguments": map[string]any{
			"name":  "anything",
			"value": oversized,
			"scope": "server",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params for oversized value")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_SetOption_UnknownSessionMapsCode pins the contract relied
// on by the JSON-RPC layer: a target session that does not exist
// surfaces as CodeSessionNotFound (-32000), not the generic internal
// code, so clients can switch on the code reliably.
func TestHandle_SetOption_UnknownSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server so we exercise the "server up, named
	// session missing" branch (a fresh controller with no socket file
	// yet produces a different error message and would not match).
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "opt_anchor", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "opt_anchor"},
			}))
	})

	params := mustJSON(t, map[string]any{
		"name": "set_option",
		"arguments": map[string]any{
			"name":   "status-interval",
			"value":  "1",
			"scope":  "session",
			"target": "definitely_does_not_exist_xyzzy",
		},
	})
	_, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatal("expected error for unknown session")
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d)", rerr.Code, errs.CodeSessionNotFound)
	}
}

// TestHandle_ToolsList_IncludesSetOption makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint.
func TestHandle_ToolsList_IncludesSetOption(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "set_option" {
			return
		}
	}
	t.Fatal("tools/list missing set_option")
}
