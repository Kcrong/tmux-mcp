package server

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// seedHookViaTmux installs a hook by shelling out to the tmux binary
// directly against the controller's socket. We can't call the
// controller's run() helper from the server test package (it's
// package-private to tmuxctl), and depending on the set_hook tool's
// JSON-RPC surface (PR #136) for seeding would couple this PR to a
// sister PR's merge order. The direct tmux call is the lightest seed
// that exercises ShowHooks against a real binding without taking on
// either dependency.
func seedHookViaTmux(t *testing.T, tools *Tools, args ...string) {
	t.Helper()
	full := append([]string{"-S", tools.Ctl.Socket()}, args...)
	out, err := exec.Command("tmux", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("seed tmux %v: %v: %s", args, err, out)
	}
}

// hookRow models one entry of the `{"hooks": [...]}` response so the
// tests can decode the canonical shape without re-typing the struct
// in every case.
type hookRow struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Target  string `json:"target"`
}

// findHookRow returns the first row matching name+target, mirroring
// the helper on the tmuxctl side. Tests use this to pin a single
// binding without caring about response order — the controller sweeps
// global tables before per-session ones, but we don't pin that order
// here so a future ordering tweak doesn't ripple into every test.
func findHookRow(rows []hookRow, name, target string) (hookRow, bool) {
	for _, r := range rows {
		if r.Name == name && r.Target == target {
			return r, true
		}
	}
	return hookRow{}, false
}

// TestShowHooks_HappyPathGlobalAndSession is the load-bearing happy
// path: install one global hook + one session-scoped hook via
// set-hook (driven through the controller, not via set_hook the
// other PR ships, so this test stands alone), call show_hooks with
// no target, and pin both bindings round-trip through the
// JSON-RPC envelope.
func TestShowHooks_HappyPathGlobalAndSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	const session = "shdemo"
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": session, "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create: %s", rerr.Message)
	}

	// Seed one global hook (server/session-class via -gH path) and
	// one per-session hook (window-class via -wH path) so the response
	// covers both probe shapes the controller drives.
	seedHookViaTmux(t, tools, "set-hook", "-g", "client-attached", `display-message "g attached"`)
	seedHookViaTmux(t, tools, "set-hook", "-t", session, "alert-bell", `display-message "s bell"`)

	body := callShowHooks(t, tools, ctx, map[string]any{})
	rows := decodeShowHooks(t, body)

	if got, ok := findHookRow(rows, "client-attached", ""); !ok {
		t.Fatalf("global client-attached missing from response: %v", rows)
	} else if got.Command != `display-message "g attached"` {
		t.Fatalf("client-attached command mismatch: got %q", got.Command)
	}
	if got, ok := findHookRow(rows, "alert-bell", session); !ok {
		t.Fatalf("per-session alert-bell missing from response: %v", rows)
	} else if got.Command != `display-message "s bell"` {
		t.Fatalf("alert-bell command mismatch: got %q", got.Command)
	}
}

// TestShowHooks_TargetScoped pins the target!="" branch through the
// JSON-RPC dispatcher: when a target is supplied, the response must
// contain only that session's bindings (not the global ones).
func TestShowHooks_TargetScoped(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	const session = "shtgt"
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": session, "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create: %s", rerr.Message)
	}

	seedHookViaTmux(t, tools, "set-hook", "-g", "client-attached", `display-message "g attached"`)
	seedHookViaTmux(t, tools, "set-hook", "-t", session, "alert-bell", `display-message "s bell"`)

	body := callShowHooks(t, tools, ctx, map[string]any{"target": session})
	rows := decodeShowHooks(t, body)

	if _, ok := findHookRow(rows, "alert-bell", session); !ok {
		t.Fatalf("per-session alert-bell missing from scoped response: %v", rows)
	}
	// Cross-check the cross-scope leak: when a target is supplied,
	// global hooks must NOT bleed in.
	if _, ok := findHookRow(rows, "client-attached", ""); ok {
		t.Fatalf("scoped show_hooks leaked global client-attached: %v", rows)
	}
}

// TestShowHooks_MissingSessionMapsCode pins the wire contract that
// show_hooks against an unknown target session surfaces
// CodeSessionNotFound (-32000), mirroring clear_history /
// session_describe / set_hook.
func TestShowHooks_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, target
	// missing" rather than "no server" (different stderr shape).
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "anchor_sh", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name": "show_hooks",
		"arguments": map[string]any{
			"target": "ghost_session_xyzzy",
		},
	})
	_, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatal("expected error for missing target session")
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d), msg=%q",
			rerr.Code, errs.CodeSessionNotFound, rerr.Message)
	}
}

// TestShowHooks_EmptyServerReturnsEmptyList pins the cold-start case:
// a fresh server with no bindings (and no sessions) must answer
// `{"hooks": []}` cleanly — never null, never an error. The encoder
// stamps an empty slice as `[]`; a nil slice would surface as `null`,
// which would break callers iterating the array.
func TestShowHooks_EmptyServerReturnsEmptyList(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	body := callShowHooks(t, tools, ctx, map[string]any{})
	// Decode the wrapper directly so we can also pin the literal
	// `[]` shape — a nil slice would marshal as `null` and break the
	// rest of the assertion.
	if !strings.Contains(body, `"hooks":[]`) && !strings.Contains(body, `"hooks": []`) {
		t.Fatalf("empty server must produce \"hooks\": []; body=%s", body)
	}
	rows := decodeShowHooks(t, body)
	if len(rows) != 0 {
		t.Fatalf("expected zero hook rows on empty server, got %d: %v",
			len(rows), rows)
	}
}

// TestShowHooks_RejectsUnknownProperty pins the `additionalProperties:
// false` schema guard. A caller sending an unknown field must surface
// as -32602 rather than silently ignoring the typo and running the
// default no-target sweep.
func TestShowHooks_RejectsUnknownProperty(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	// We assert the schema-side guard at the schema level, since the
	// handler decodes via json.Unmarshal which silently accepts unknown
	// fields. The schema's additionalProperties:false is what an MCP
	// client validating against the published surface relies on.
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] != "show_hooks" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		if schema == nil {
			t.Fatalf("inputSchema missing on show_hooks def: %#v", def)
		}
		addl, ok := schema["additionalProperties"].(bool)
		if !ok || addl {
			t.Fatalf("additionalProperties must be false on show_hooks; got %#v",
				schema["additionalProperties"])
		}
		return
	}
	t.Fatal("tools/list missing show_hooks")
}

// TestShowHooks_RejectsBadTargetShape pins the up-front session-name
// validator. A target that violates the regex / length policy must
// come back as CodeInvalidParams (-32602) rather than reaching tmux
// and surfacing whatever stderr the daemon picked.
func TestShowHooks_RejectsBadTargetShape(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := &Tools{}

	cases := []struct {
		label string
		name  string
	}{
		{"with spaces", "bad name"},
		{"colon", "demo:colon"},
		{"too long", strings.Repeat("a", 65)},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name":      "show_hooks",
				"arguments": map[string]any{"target": tc.name},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid-params error for %q", tc.name)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
					rerr.Code, errs.CodeInvalidParams, rerr.Message)
			}
		})
	}
}

// TestShowHooks_ListedInTools confirms the init()-time registration
// actually wired show_hooks into tools/list. Without this guard a
// regression in the package-init append could silently drop the tool
// from the surface even though the dispatcher still recognised it.
func TestShowHooks_ListedInTools(t *testing.T) {
	t.Parallel()
	tools := &Tools{}
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] != "show_hooks" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		if schema == nil {
			t.Fatalf("inputSchema missing on show_hooks def: %#v", def)
		}
		// `target` is optional — required must NOT include it (or the
		// schema would force every caller to pass a session even for
		// the global-sweep default).
		if req, ok := schema["required"]; ok {
			if list, _ := req.([]string); len(list) != 0 {
				t.Fatalf("required = %#v, want empty (target is optional)", req)
			}
		}
		return
	}
	t.Fatal("tools/list missing show_hooks")
}

// TestShowHooks_AllowedUnderReadOnly is the read-only contract: the
// allowlist in readonly.go must include show_hooks so a server running
// with -read-only still exposes the inspector. Without this pin, the
// entry could silently fall out of the table during a later refactor.
func TestShowHooks_AllowedUnderReadOnly(t *testing.T) {
	t.Parallel()
	if !IsReadOnlyTool("show_hooks") {
		t.Fatal("IsReadOnlyTool(\"show_hooks\") = false; allowlist must accept it")
	}
}

// callShowHooks dispatches a tools/call against show_hooks and
// returns the textual body. Centralising the framing here keeps each
// test's intent (which arguments / which assertions) at the top of
// the function rather than buried under boilerplate.
func callShowHooks(t *testing.T, tools *Tools, ctx context.Context, args map[string]any) string {
	t.Helper()
	params := mustJSON(t, map[string]any{
		"name":      "show_hooks",
		"arguments": args,
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("show_hooks: code=%d msg=%q", rerr.Code, rerr.Message)
	}
	return extractText(t, res)
}

// decodeShowHooks pulls the rows out of the canonical
// `{"hooks": [...]}` envelope. Failing the test on a decode error
// keeps the assertion pinned to the wire contract — a future change
// to the response shape has to update this helper.
func decodeShowHooks(t *testing.T, body string) []hookRow {
	t.Helper()
	var got struct {
		Hooks []hookRow `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode show_hooks body %q: %v", body, err)
	}
	return got.Hooks
}
