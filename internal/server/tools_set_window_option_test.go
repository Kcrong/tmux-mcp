package server

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// tmuxRunSetWindowOption shells out to the tmux binary on PATH against
// the supplied socket and returns stdout. Used by the
// set_window_option suite to inspect tmux's view of the window
// options without depending on the show_options MCP tool, which has
// its own scope-validation suite — keeping the assertions focused on
// the write-side surface we just wired in.
//
// Failure aborts the test with a stderr-bearing message so a flaky
// tmux build does not turn into a head-scratching assertion miss.
func tmuxRunSetWindowOption(t *testing.T, socket string, args ...string) string {
	t.Helper()
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatalf("tmux not on PATH: %v", err)
	}
	full := append([]string{"-S", socket}, args...)
	cmd := exec.Command(bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		t.Fatalf("tmux %v: %v (stderr=%q)", args, runErr, stderr.String())
	}
	return stdout.String()
}

// setWindowOptionTestSetup spins up a fresh *Tools with an anchor
// session (so the tmux server is up — set-window-option needs a real
// target window) and returns a pre-bound `call` helper plus the
// deadline context. The anchor session is named per-test via the
// `session` argument so parallel tests don't collide on the tmux
// session table.
//
// Each caller must invoke t.Parallel() itself — t.Helper() inside a
// helper does not propagate t.Parallel, and the user-facing
// concurrency contract is "one tmux server per top-level test".
func setWindowOptionTestSetup(t *testing.T, session string) (
	tools *Tools,
	call func(name string, args any) any,
	ctx context.Context,
) {
	t.Helper()
	skipIfNoTmux(t)
	tools = newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	call = func(name string, args any) any {
		t.Helper()
		params := mustJSON(t, map[string]any{"name": name, "arguments": args})
		res, rerr := tools.Handle(ctx, "tools/call", params)
		if rerr != nil {
			t.Fatalf("%s: %s", name, rerr.Message)
		}
		return res
	}

	call("session_create", map[string]any{
		"name": session, "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": session},
			}))
	})
	return tools, call, ctx
}

// decodeSetWindowOption pulls the {"set": ..., "unset": ..., "name": ...}
// envelope out of the tools/call result so individual test cases stay
// focused on the field that matters.
func decodeSetWindowOption(t *testing.T, result any) (set, unset bool, name string) {
	t.Helper()
	body := extractText(t, result)
	var obj struct {
		Set   bool   `json:"set"`
		Unset bool   `json:"unset"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode set_window_option body %q: %v", body, err)
	}
	return obj.Set, obj.Unset, obj.Name
}

// TestHandle_SetWindowOption_HappyPath_SynchronizePanes drives the
// load-bearing happy path through the dispatcher: setting
// `synchronize-panes` to `on` for an existing session must succeed and
// be visible to a follow-up tmux probe. We probe tmux directly via
// the controller's socket so the test is independent of show_options.
func TestHandle_SetWindowOption_HappyPath_SynchronizePanes(t *testing.T) {
	t.Parallel()
	tools, call, _ := setWindowOptionTestSetup(t, "swo_h")

	set, unset, name := decodeSetWindowOption(t, call("set_window_option", map[string]any{
		"target": "swo_h:0",
		"name":   "synchronize-panes",
		"value":  "on",
	}))
	if !set {
		t.Fatalf("set_window_option set=false, want true")
	}
	if unset {
		t.Fatalf("set_window_option unset=true on a non-unset call")
	}
	if name != "synchronize-panes" {
		t.Errorf("name = %q, want synchronize-panes", name)
	}

	out := tmuxRunSetWindowOption(t, tools.Ctl.Socket(),
		"show-window-options", "-t", "swo_h:0", "synchronize-panes")
	if !strings.Contains(out, "synchronize-panes on") {
		t.Fatalf("show-window-options output %q does not echo `synchronize-panes on`", out)
	}
}

// TestHandle_SetWindowOption_AppendStringList drives the `append`
// boolean against `pane-border-format`, a string-list option whose
// values concatenate when -a is supplied. We seed the option with a
// base value, then append a suffix and assert the resolved value
// contains both halves so the append actually composed instead of
// replacing.
func TestHandle_SetWindowOption_AppendStringList(t *testing.T) {
	t.Parallel()
	tools, call, _ := setWindowOptionTestSetup(t, "swo_app")

	// Seed with a base value (no append).
	call("set_window_option", map[string]any{
		"target": "swo_app:0",
		"name":   "pane-border-format",
		"value":  "BASE",
	})
	// Append with `append: true`.
	set, _, _ := decodeSetWindowOption(t, call("set_window_option", map[string]any{
		"target": "swo_app:0",
		"name":   "pane-border-format",
		"value":  "+EXTRA",
		"append": true,
	}))
	if !set {
		t.Fatalf("set_window_option set=false on append, want true")
	}

	out := tmuxRunSetWindowOption(t, tools.Ctl.Socket(),
		"show-window-options", "-t", "swo_app:0", "pane-border-format")
	if !strings.Contains(out, "BASE") || !strings.Contains(out, "+EXTRA") {
		t.Fatalf("show-window-options output %q does not contain both BASE and +EXTRA after append", out)
	}
}

// TestHandle_SetWindowOption_UnsetClearsOverride pins the `unset`
// boolean: after a set + unset pair, the per-window override must be
// gone. We use synchronize-panes (defaults to "off"); after unset,
// the literal `synchronize-panes on` line — produced by show-window-
// options when the override is present — must no longer appear.
func TestHandle_SetWindowOption_UnsetClearsOverride(t *testing.T) {
	t.Parallel()
	tools, call, _ := setWindowOptionTestSetup(t, "swo_u")

	// Set the override.
	call("set_window_option", map[string]any{
		"target": "swo_u:0",
		"name":   "synchronize-panes",
		"value":  "on",
	})
	// Then unset; `value` may be omitted under unset=true. The
	// dispatcher must accept that combination without complaint.
	set, unset, _ := decodeSetWindowOption(t, call("set_window_option", map[string]any{
		"target": "swo_u:0",
		"name":   "synchronize-panes",
		"unset":  true,
	}))
	if set {
		t.Fatalf("set=true on an unset call, want false")
	}
	if !unset {
		t.Fatalf("unset=false, want true")
	}

	out := tmuxRunSetWindowOption(t, tools.Ctl.Socket(),
		"show-window-options", "-t", "swo_u:0", "synchronize-panes")
	if strings.Contains(out, "synchronize-panes on") {
		t.Fatalf("expected `synchronize-panes on` to be gone after unset, got %q", out)
	}
}

// TestHandle_SetWindowOption_GlobalNoTarget pins the `global` knob
// path: with global=true and no target, tmux modifies the global
// window-options table (the defaults inherited by every window).
// We probe with `show-window-options -g` to confirm the resolved
// global value.
func TestHandle_SetWindowOption_GlobalNoTarget(t *testing.T) {
	t.Parallel()
	tools, call, _ := setWindowOptionTestSetup(t, "swo_g")

	set, _, _ := decodeSetWindowOption(t, call("set_window_option", map[string]any{
		"name":   "synchronize-panes",
		"value":  "on",
		"global": true,
	}))
	if !set {
		t.Fatalf("set_window_option set=false on global, want true")
	}

	out := tmuxRunSetWindowOption(t, tools.Ctl.Socket(),
		"show-window-options", "-g", "synchronize-panes")
	if !strings.Contains(out, "synchronize-panes on") {
		t.Fatalf("show-window-options -g output %q does not echo `synchronize-panes on`", out)
	}
}

// TestHandle_SetWindowOption_MissingSessionMapsCode pins the wire
// contract for "set against a session that does not exist": the
// JSON-RPC error code must be errs.CodeSessionNotFound (-32000) so
// MCP clients can branch on a stable code rather than a tmux stderr
// substring.
func TestHandle_SetWindowOption_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	tools, _, ctx := setWindowOptionTestSetup(t, "swo_anchor")

	params := mustJSON(t, map[string]any{
		"name": "set_window_option",
		"arguments": map[string]any{
			"target": "ghost_xyzzy:0",
			"name":   "synchronize-panes",
			"value":  "on",
		},
	})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatalf("expected error for missing session, got result %#v", res)
	}
	if rerr.Code != errs.CodeSessionNotFound {
		t.Fatalf("code = %d, want CodeSessionNotFound (%d)", rerr.Code, errs.CodeSessionNotFound)
	}
}

// TestHandle_SetWindowOption_RejectsBadName locks the regex check on
// `name` so a stray quote, leading digit, or whitespace cannot slip
// through to the tmux argv. The check runs before any tmux command,
// so the error must carry CodeInvalidParams (-32602).
func TestHandle_SetWindowOption_RejectsBadName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []struct {
		name      string
		fieldName string
	}{
		{"empty", ""},
		{"leading digit", "9synchronize-panes"},
		{"contains space", "bad name"},
		{"contains shell metachar", "syn;chronize"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "set_window_option",
				"arguments": map[string]any{
					"target": "swo:0",
					"name":   tc.fieldName,
					"value":  "on",
				},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params error for name=%q", tc.fieldName)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_SetWindowOption_RejectsOversizedValue enforces the 4 KiB
// value cap. A 4 KiB+1 byte payload must fail with CodeInvalidParams
// before any tmux process is spawned — otherwise tmux happily
// allocates the value and the JSON-RPC writer is left holding the
// bag.
func TestHandle_SetWindowOption_RejectsOversizedValue(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	oversized := strings.Repeat("a", maxSetWindowOptionValueLen+1)
	params := mustJSON(t, map[string]any{
		"name": "set_window_option",
		"arguments": map[string]any{
			"target": "swo:0",
			"name":   "synchronize-panes",
			"value":  oversized,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for oversized value")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "out of range") {
		t.Errorf("error msg = %q, expected to mention `out of range`", rerr.Message)
	}
}

// TestHandle_SetWindowOption_RejectsNULInValue locks the NUL/control
// byte rejection. A stray NUL byte would terminate the Go-side
// argument before tmux saw the rest of the value; a stray control
// character would garble audit / metric surfaces. Either is rejected
// at the boundary with CodeInvalidParams.
func TestHandle_SetWindowOption_RejectsNULInValue(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "set_window_option",
		"arguments": map[string]any{
			"target": "swo:0",
			"name":   "synchronize-panes",
			"value":  "on\x00",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for NUL byte in value")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "NUL") {
		t.Errorf("error msg = %q, expected to mention NUL", rerr.Message)
	}
}

// TestHandle_SetWindowOption_RejectsValueRequiredWhenNotUnset pins
// the "value required unless unset=true" guard the schema cannot
// express. JSON Schema does not have a conditional-required clause
// that depends on another field's value, so the handler enforces
// this in code.
func TestHandle_SetWindowOption_RejectsValueRequiredWhenNotUnset(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	// Note: no `value` field at all, no `unset: true`. The handler
	// must reject this rather than silently send an empty value to
	// tmux.
	params := mustJSON(t, map[string]any{
		"name": "set_window_option",
		"arguments": map[string]any{
			"target": "swo:0",
			"name":   "synchronize-panes",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing value")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "value") {
		t.Errorf("error msg = %q, expected to mention `value`", rerr.Message)
	}
}

// TestHandle_SetWindowOption_RejectsTargetMissingWhenNotGlobal pins
// the "target required unless global=true" guard. Without an explicit
// target, tmux would either pick the "current" window (rarely what
// an agent meant) or fail with a confusing diagnostic.
func TestHandle_SetWindowOption_RejectsTargetMissingWhenNotGlobal(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "set_window_option",
		"arguments": map[string]any{
			"name":  "synchronize-panes",
			"value": "on",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing target without global=true")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_SetWindowOption_ListedInTools makes sure tools/list
// advertises the new tool so MCP clients can discover it via the
// schema endpoint, and also pins the `additionalProperties: false`
// contract so a stray unknown field would be rejected by a
// schema-validating client. (The dispatcher itself does not run
// schema validation against extra fields — Handle's JSON unmarshal
// silently drops unknown keys — so the schema declaration is the
// load-bearing surface for unknown-property rejection.)
func TestHandle_SetWindowOption_ListedInTools(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "set_window_option" {
			schema, ok := def["inputSchema"].(map[string]any)
			if !ok {
				t.Fatalf("inputSchema missing for set_window_option: %v", def)
			}
			if got, ok := schema["additionalProperties"].(bool); !ok || got {
				t.Errorf("set_window_option additionalProperties = %v, want false", schema["additionalProperties"])
			}
			required, _ := schema["required"].([]string)
			if len(required) != 1 || required[0] != "name" {
				t.Errorf("set_window_option required = %v, want [\"name\"]", required)
			}
			props, _ := schema["properties"].(map[string]any)
			for _, want := range []string{"name", "value", "target", "append", "format_expand", "global", "allow_missing", "unset"} {
				if _, ok := props[want]; !ok {
					t.Errorf("set_window_option schema missing property %q", want)
				}
			}
			return
		}
	}
	t.Fatal("tools/list missing set_window_option")
}

// TestHandle_SetWindowOption_NotInReadOnlyAllowlist guards the policy
// that set_window_option is a mutating tool: under -read-only the
// dispatcher must reject it. Pairs with the entry we add to
// RejectsMutators in readonly_test.go — keeping both checks makes
// drift impossible (one would fail before the other if a future
// contributor accidentally added the tool to readOnlyTools).
func TestHandle_SetWindowOption_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("set_window_option") {
		t.Fatal("IsReadOnlyTool(\"set_window_option\") = true; mutating tools must not be inspection-allowed")
	}
}
