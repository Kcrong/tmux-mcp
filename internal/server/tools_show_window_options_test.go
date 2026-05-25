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

// setWindowOptionRaw drives a raw `tmux set-window-option` against the
// controller backing the test's tools instance. show_window_options
// itself is read-only — there is no symmetric write tool yet (the
// set_window_option PR is still outstanding) — so each test seeds tmux
// state directly. We exec a one-shot tmux invocation against the same
// socket the controller uses (Controller.Socket() is the public escape
// hatch): the controller's own run() is private to the tmuxctl package,
// so going through `tmux -S <sock>` from the test is the cleanest way
// to drive tmux state without leaking a write surface into production
// code prematurely. skipIfNoTmux upstream of every caller guarantees
// `tmux` resolves on PATH.
func setWindowOptionRaw(t *testing.T, tools *Tools, ctx context.Context, target, key, value string) {
	t.Helper()
	socket := tools.Ctl.Socket()
	args := []string{"-S", socket}
	if cfg := tools.Ctl.ConfigPath(); cfg != "" {
		// Mirror the controller's argv assembly: -f, when configured,
		// must precede the subcommand verb. Without this, a deployment
		// that pins a custom tmux.conf (e.g. via -tmux-config-path)
		// would seed state under the controller's defaults but read it
		// back under the test runner's tmux defaults, producing
		// confusing flakes.
		args = append(args, "-f", cfg)
	}
	args = append(args, "set-window-option", "-t", target, key, value)
	cmd := exec.CommandContext(ctx, "tmux", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("set-window-option %s %s=%s: %v (output=%q)", target, key, value, err, string(out))
	}
}

// TestHandle_ShowWindowOptions_GlobalDefaults drives the happy path for
// the `-g` view through the JSON-RPC dispatcher: anchor a real session,
// call show_window_options with global=true, then assert the response
// envelope decodes cleanly and at least one well-known global key is
// present. mode-keys is the long-standing default we pin against.
func TestHandle_ShowWindowOptions_GlobalDefaults(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	const name = "swo_global_h"
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": name, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": name},
			}))
	})

	body := extractText(t, callTool(t, tools, ctx, "show_window_options", map[string]any{
		"target": name + ":0",
		"global": true,
	}))
	var got struct {
		Options []map[string]any `json:"options"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode show_window_options body %q: %v", body, err)
	}
	if len(got.Options) == 0 {
		t.Fatalf("expected non-empty global window options, body=%s", body)
	}
	found := false
	for _, e := range got.Options {
		if n, _ := e["name"].(string); n == "mode-keys" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected mode-keys among global window options, body=%s", body)
	}
}

// TestHandle_ShowWindowOptions_ByName_AfterSet pins the load-bearing
// happy path for the per-window read: seed a synchronize-panes override
// via raw set-window-option, then ShowWindowOptions(target, name,
// false) returns exactly one entry with name="synchronize-panes" and
// value="on". This mirrors the agent introspection use case the brief
// calls out: an LLM checking which per-window flag the live window has
// flipped.
func TestHandle_ShowWindowOptions_ByName_AfterSet(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	const name = "swo_byname_h"
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": name, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": name},
			}))
	})
	setWindowOptionRaw(t, tools, ctx, name+":0", "synchronize-panes", "on")

	body := extractText(t, callTool(t, tools, ctx, "show_window_options", map[string]any{
		"target": name + ":0",
		"name":   "synchronize-panes",
	}))
	var got struct {
		Options []map[string]any `json:"options"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode show_window_options body %q: %v", body, err)
	}
	if len(got.Options) != 1 {
		t.Fatalf("by-name query should return exactly one entry, got %d (%s)", len(got.Options), body)
	}
	if n, _ := got.Options[0]["name"].(string); n != "synchronize-panes" {
		t.Fatalf("entry name = %q, want synchronize-panes", n)
	}
	if v, _ := got.Options[0]["value"].(string); v != "on" {
		t.Fatalf("synchronize-panes = %q, want %q", v, "on")
	}
}

// TestHandle_ShowWindowOptions_EmptyResult pins the contract that a
// fresh window with no per-window overrides surfaces as
// `{"options": []}` — not an error. Without this guard, callers
// iterating the result would crash on a missing field.
func TestHandle_ShowWindowOptions_EmptyResult(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	const name = "swo_empty_h"
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": name, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": name},
			}))
	})

	body := extractText(t, callTool(t, tools, ctx, "show_window_options", map[string]any{
		"target": name + ":0",
	}))
	var got struct {
		Options []map[string]any `json:"options"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode show_window_options body %q: %v", body, err)
	}
	if len(got.Options) != 0 {
		t.Fatalf("fresh window should have no per-window overrides, got %v", got.Options)
	}
}

// TestHandle_ShowWindowOptions_UnknownPropertyRejected pins the
// `additionalProperties: false` contract: a client that misnames a
// field gets a fast schema-shaped rejection rather than a silent
// no-op. We do this through the JSON Schema's invariant (json.Unmarshal
// of strict struct + DisallowUnknownFields would also work, but the
// existing tools rely on the schema layer at the protocol boundary —
// this test pins the schema is correctly declared so a future caller
// reading tools/list sees the documented constraint).
func TestHandle_ShowWindowOptions_UnknownPropertyRejected(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "show_window_options" {
			schema, _ := def["inputSchema"].(map[string]any)
			got, ok := schema["additionalProperties"].(bool)
			if !ok || got {
				t.Fatalf("show_window_options schema additionalProperties = %v, want false", schema["additionalProperties"])
			}
			return
		}
	}
	t.Fatalf("tools/list missing show_window_options")
}

// TestHandle_ShowWindowOptions_MissingSessionMapsCode pins the wire
// contract for a target whose session does not exist: the JSON-RPC
// error code must be CodeSessionNotFound (-32000) regardless of which
// exact phrase tmux emits ("no such window" on tmux 3.4). MCP clients
// branch on the typed code — substring-matching the message would
// drift across tmux versions.
func TestHandle_ShowWindowOptions_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor so the dispatcher hits "server up, target missing" rather
	// than "no server running" — the latter is a different branch.
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "swo_anchor", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "swo_anchor"},
			}))
	})

	params := mustJSON(t, map[string]any{
		"name": "show_window_options",
		"arguments": map[string]any{
			"target": "definitely_does_not_exist_xyzzy:0",
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

// TestHandle_ShowWindowOptions_RejectsBadTarget guards the regex/length
// policy on the optional `target` argument. A malformed value must be
// refused with CodeInvalidParams up front so tmux is never asked to
// resolve a session reference with shell metachars.
func TestHandle_ShowWindowOptions_RejectsBadTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "show_window_options",
		"arguments": map[string]any{"target": "bad name:0"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad target")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ShowWindowOptions_RejectsBadWindow guards the regex/length
// policy on the window half of the target. session is well-formed but
// the window component carries a shell metachar — the boundary must
// reject it before tmux is consulted.
func TestHandle_ShowWindowOptions_RejectsBadWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "show_window_options",
		"arguments": map[string]any{"target": "demo:bad win"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for bad window")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_ShowWindowOptions_ListedInTools confirms the init()-time
// registration actually wired show_window_options into tools/list. The
// schema check in the same loop guards the additionalProperties=false
// contract too so a future contributor flipping it sees the regression
// here.
func TestHandle_ShowWindowOptions_ListedInTools(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] == "show_window_options" {
			schema, _ := def["inputSchema"].(map[string]any)
			props, _ := schema["properties"].(map[string]any)
			for _, want := range []string{"target", "name", "global"} {
				if _, ok := props[want]; !ok {
					t.Errorf("schema missing %q property", want)
				}
			}
			return
		}
	}
	t.Fatal("tools/list missing show_window_options")
}

// TestHandle_ShowWindowOptions_IsReadOnlyAllowed pins the inverse of
// the read-only-rejects-mutators contract: show_window_options is a
// pure inspection of tmux state, so it MUST be on the read-only
// allowlist. Without this pin, a future contributor moving the entry
// out of readonly.go's table would silently break -read-only
// deployments.
func TestHandle_ShowWindowOptions_IsReadOnlyAllowed(t *testing.T) {
	t.Parallel()
	if !IsReadOnlyTool("show_window_options") {
		t.Fatal("IsReadOnlyTool(\"show_window_options\") = false, want true (must be inspection-allowed)")
	}
}

// TestHandle_ShowWindowOptions_AcceptsNullArguments guards the
// "raw is empty" branch — every field is optional, so a tools/call
// with `arguments: {}` (or no arguments at all) must dispatch cleanly.
// Anchoring with a session keeps the response non-empty so we can
// also probe that the tmux server is reachable on this branch.
func TestHandle_ShowWindowOptions_AcceptsNullArguments(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "swo_null", "command": "/bin/sh",
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "swo_null"},
			}))
	})

	params := mustJSON(t, map[string]any{"name": "show_window_options"})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("show_window_options: %s", rerr.Message)
	}
	body := extractText(t, res)
	if !strings.Contains(body, `"options"`) {
		t.Fatalf("expected response to contain options key, got %s", body)
	}
}
