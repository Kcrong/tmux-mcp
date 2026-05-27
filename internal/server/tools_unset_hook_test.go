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

// setHookForTest installs a fresh hook on the controller's tmux daemon
// by shelling out to `tmux -S <socket> set-hook ...` directly. The
// unset_hook tool ships before its sister `set_hook` tool surface, so
// we cannot drive the bind through the public dispatcher; reaching for
// the tmux CLI keeps the test self-contained without coupling to
// either a not-yet-merged set_hook tool or a test-only Controller
// export.
//
// args is the suffix after `set-hook` (e.g. `[]string{"-t", "uh",
// "pane-died", `display-message "x"`}`). The bound action is a
// `display-message` so a hook that ever fires only logs a noop.
func setHookForTest(t *testing.T, tools *Tools, ctx context.Context, args ...string) {
	t.Helper()
	full := append([]string{"-S", tools.Ctl.Socket(), "set-hook"}, args...)
	cmd := exec.CommandContext(ctx, "tmux", full...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("set-hook %v: %v: %s", args, err, string(out))
	}
}

// hookListedSession reports whether a per-session hook is currently
// registered against the resolved tmux session by walking the daemon's
// `show-options` output. Same name+`[` discriminator as the
// controller-side probes, accessed via the controller's run() pipe.
func hookListedSession(t *testing.T, tools *Tools, ctx context.Context, target, name string) bool {
	t.Helper()
	needle := name + "["
	for _, args := range [][]string{
		{"show-options", "-t", target, "-wH"},
		{"show-options", "-t", target, "-H"},
	} {
		full := append([]string{"-S", tools.Ctl.Socket()}, args...)
		out, err := exec.CommandContext(ctx, "tmux", full...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v: %s", args, err, string(out))
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), needle) {
				return true
			}
		}
	}
	return false
}

// TestHandle_UnsetHook_HappyPath drives the dispatcher end-to-end:
// install a fresh per-session hook, ask unset_hook to clear it via
// tools/call, observe it disappears from `show-options`. Pins both
// the dispatcher case wiring and the controller's `-t TARGET` argv
// shape — a regression where the boundary dropped either flag would
// surface as the post-unset probe still finding the hook.
func TestHandle_UnsetHook_HappyPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "uh_hp", "command": "/bin/sh",
	})

	const target = "uh_hp"
	const hook = "pane-died"

	setHookForTest(t, tools, ctx, "-t", target, hook, `display-message "ping"`)
	if !hookListedSession(t, tools, ctx, target, hook) {
		t.Fatalf("pre-condition: %s/%s not present after set-hook", target, hook)
	}

	res := callTool(t, tools, ctx, "unset_hook", map[string]any{
		"hook":   hook,
		"target": target,
	})
	body := extractText(t, res)
	var ack struct {
		Unset  bool   `json:"unset"`
		Global bool   `json:"global"`
		Window bool   `json:"window"`
		Hook   string `json:"hook"`
	}
	if err := json.Unmarshal([]byte(body), &ack); err != nil {
		t.Fatalf("decode unset_hook response: %v\nbody=%s", err, body)
	}
	if !ack.Unset || ack.Global || ack.Window || ack.Hook != hook {
		t.Fatalf("unexpected ack: %+v (body=%s)", ack, body)
	}
	if hookListedSession(t, tools, ctx, target, hook) {
		t.Fatalf("post-condition: %s/%s still present after unset_hook", target, hook)
	}
}

// TestHandle_UnsetHook_GlobalRoundTrip exercises the `-g`
// (server-wide) clear path through the dispatcher: install a global
// hook, ask unset_hook with global=true, the bind disappears. A
// regression where the boundary dropped `-g` would either no-op
// (against an empty per-session table) or wipe the wrong session, both
// of which surface as the post-unset probe still finding the global
// entry.
func TestHandle_UnsetHook_GlobalRoundTrip(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	// Anchor the daemon with a real session so the global bind exercises
	// "server up, hook installed against global table".
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "anchor", "command": "/bin/sh",
	})

	const hook = "client-attached"
	setHookForTest(t, tools, ctx, "-g", hook, `display-message "global"`)

	res := callTool(t, tools, ctx, "unset_hook", map[string]any{
		"hook":   hook,
		"global": true,
	})
	body := extractText(t, res)
	var ack struct {
		Unset  bool   `json:"unset"`
		Global bool   `json:"global"`
		Window bool   `json:"window"`
		Hook   string `json:"hook"`
	}
	if err := json.Unmarshal([]byte(body), &ack); err != nil {
		t.Fatalf("decode unset_hook -g response: %v\nbody=%s", err, body)
	}
	if !ack.Unset || !ack.Global || ack.Window || ack.Hook != hook {
		t.Fatalf("unexpected ack: %+v (body=%s)", ack, body)
	}

	// Probe the global namespace via the controller socket.
	out, err := exec.CommandContext(ctx, "tmux", "-S", tools.Ctl.Socket(), "show-options", "-gH").CombinedOutput()
	if err != nil {
		t.Fatalf("show-options -gH: %v: %s", err, string(out))
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), hook+"[") {
			t.Fatalf("post-condition: %s still present in global options after unset_hook -g", hook)
		}
	}
}

// TestHandle_UnsetHook_WindowRoundTrip pins the `-w` (window-scoped)
// clear path through the dispatcher: install a window-scoped hook on a
// session, ask unset_hook with window=true and the same target, the
// bind disappears. Mirrors the controller-level WindowRoundTrip test
// at the JSON-RPC envelope layer so a regression that swapped `-w`
// for `-g` (or dropped the flag entirely) trips both layers.
func TestHandle_UnsetHook_WindowRoundTrip(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "uh_w", "command": "/bin/sh",
	})

	const target = "uh_w"
	const hook = "pane-died"
	setHookForTest(t, tools, ctx, "-w", "-t", target, hook, `display-message "win"`)

	res := callTool(t, tools, ctx, "unset_hook", map[string]any{
		"hook":   hook,
		"target": target,
		"window": true,
	})
	body := extractText(t, res)
	var ack struct {
		Unset  bool   `json:"unset"`
		Global bool   `json:"global"`
		Window bool   `json:"window"`
		Hook   string `json:"hook"`
	}
	if err := json.Unmarshal([]byte(body), &ack); err != nil {
		t.Fatalf("decode unset_hook -w response: %v\nbody=%s", err, body)
	}
	if !ack.Unset || ack.Global || !ack.Window || ack.Hook != hook {
		t.Fatalf("unexpected ack: %+v (body=%s)", ack, body)
	}

	// `-w -t SESSION` lands in the window-options table; probe via
	// `show-options -t SESSION -wH` exactly like the controller-side
	// probe does so the test stays consistent across layers.
	out, err := exec.CommandContext(ctx, "tmux", "-S", tools.Ctl.Socket(),
		"show-options", "-t", target, "-wH").CombinedOutput()
	if err != nil {
		t.Fatalf("show-options -t %s -wH: %v: %s", target, err, string(out))
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), hook+"[") {
			t.Fatalf("post-condition: %s still present on window after unset_hook -w", hook)
		}
	}
}

// TestHandle_UnsetHook_RejectsMissingHook pins the required-field
// path: omitting `hook` must come back as CodeInvalidParams rather
// than falling through to tmux with an empty hook name (which would
// surface a less helpful "too few arguments" stderr).
func TestHandle_UnsetHook_RejectsMissingHook(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "unset_hook",
		"arguments": map[string]any{"target": "any"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing hook")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q", rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
}

// TestHandle_UnsetHook_RejectsBadHookName locks the regex check on
// `hook`. A stray semicolon / whitespace / shell metachar / uppercase
// must not slip through to the tmux argv. tmux's documented hook
// names are lowercase-snake or hyphenated, so the boundary refuses
// anything outside that conservative shape.
func TestHandle_UnsetHook_RejectsBadHookName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	for _, bad := range []string{
		"pane-died;rm -rf /",
		"pane died",
		"pane.died",
		`pane"died`,
		"Pane-Died",  // uppercase rejected by ^[a-z]...
		"-pane-died", // leading dash rejected
		"1pane-died", // leading digit rejected
		"_pane-died", // leading underscore rejected
	} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "unset_hook",
				"arguments": map[string]any{
					"hook":   bad,
					"target": "any",
				},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params error for bad hook %q", bad)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_UnsetHook_RejectsBothGlobalAndWindow pins the mutual-
// exclusion guard: `global=true` and `window=true` together do not
// have a single well-defined meaning, and tmux's behaviour across
// versions is undefined. The boundary refuses the shape so callers
// get a clean -32602 instead of a confusing successful-but-wrong
// outcome.
func TestHandle_UnsetHook_RejectsBothGlobalAndWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "unset_hook",
		"arguments": map[string]any{
			"hook":   "pane-died",
			"global": true,
			"window": true,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for global=true + window=true")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q", rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
}

// TestHandle_UnsetHook_RejectsMissingTargetWhenSession locks the
// per-session clear-path target check: `global=false`, `window=false`,
// no `target` must come back as CodeInvalidParams. Without the
// up-front guard tmux would resolve "" to whatever session it
// considered current, silently mis-routing the clear against a stale
// target.
func TestHandle_UnsetHook_RejectsMissingTargetWhenSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "unset_hook",
		"arguments": map[string]any{
			"hook": "pane-died",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing target on per-session clear")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q", rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
}

// TestHandle_UnsetHook_MissingSessionMapsCode pins the wire contract
// that unset_hook against an unknown target session surfaces
// CodeSessionNotFound (-32000), mirroring clear_history /
// session_describe / pane_kill.
func TestHandle_UnsetHook_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, target
	// missing" rather than "no server" (which surfaces the same code
	// via the headless-folding path but is a different stderr shape).
	callTool(t, tools, ctx, "session_create", map[string]any{
		"name": "anchor", "command": "/bin/sh",
	})

	params := mustJSON(t, map[string]any{
		"name": "unset_hook",
		"arguments": map[string]any{
			"hook":   "pane-died",
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

// TestHandle_UnsetHook_ListedInTools confirms the init()-time
// registration actually wired unset_hook into tools/list. Without
// this guard a regression in the package-init append could silently
// drop the tool from the surface even though the dispatcher still
// recognised it.
func TestHandle_UnsetHook_ListedInTools(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] == "unset_hook" {
			return
		}
	}
	t.Fatal("tools/list missing unset_hook")
}

// TestHandle_UnsetHook_AdditionalPropertiesLocked enforces the
// additionalProperties:false contract on the schema — a typo like
// "hook_name" instead of "hook" must surface through the schema
// rather than being silently swallowed at decode time.
func TestHandle_UnsetHook_AdditionalPropertiesLocked(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] != "unset_hook" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		got, ok := schema["additionalProperties"].(bool)
		if !ok || got {
			t.Fatalf("unset_hook schema additionalProperties = %v, want false", schema["additionalProperties"])
		}
		props, _ := schema["properties"].(map[string]any)
		for _, want := range []string{"hook", "target", "global", "window"} {
			if _, ok := props[want]; !ok {
				t.Fatalf("unset_hook schema missing property %q", want)
			}
		}
		required, _ := schema["required"].([]string)
		if len(required) != 1 || required[0] != "hook" {
			t.Fatalf("unset_hook schema required = %v, want [\"hook\"]", required)
		}
		return
	}
	t.Fatal("tools/list missing unset_hook")
}

// TestUnsetHook_NotInReadOnlyAllowlist pins the policy: unset_hook
// removes a tmux hook (long-lived behaviour change) and is therefore
// mutating, so a `-read-only` operator must not be able to invoke it.
// Adding the tool to the allowlist would silently let a read-only
// agent rewire the daemon's behaviour by clearing the bindings their
// supervisor depends on.
func TestUnsetHook_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("unset_hook") {
		t.Fatal("unset_hook must NOT be in the read-only allowlist (it removes tmux hook bindings)")
	}
}
