package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_SetHook_BindAndUnset is the load-bearing happy path:
// dispatch tools/call set_hook, decode the canonical
// `{"set": true, "unset": false, "global": false, "name": "<hook>"}`
// ack on bind, then dispatch the unset round-trip and verify the ack
// reflects the unset mode flag. The deeper "did the hook actually land
// in tmux's options table" invariant lives in the tmuxctl-side
// SetHook tests where the controller can poke at `show-options -wH`
// directly; this test is responsible for the JSON-RPC envelope and
// the canonical ack shape.
func TestHandle_SetHook_BindAndUnset(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "hk", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create: %s", rerr.Message)
	}

	bindParams := mustJSON(t, map[string]any{
		"name": "set_hook",
		"arguments": map[string]any{
			"name":    "pane-died",
			"command": `display-message "ping"`,
			"target":  "hk",
		},
	})
	res, rerr := tools.Handle(ctx, "tools/call", bindParams)
	if rerr != nil {
		t.Fatalf("set_hook bind: %s", rerr.Message)
	}
	body := extractText(t, res)
	var bindAck struct {
		Set    bool   `json:"set"`
		Unset  bool   `json:"unset"`
		Global bool   `json:"global"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal([]byte(body), &bindAck); err != nil {
		t.Fatalf("decode set_hook bind ack: %v\nbody=%s", err, body)
	}
	if !bindAck.Set || bindAck.Unset || bindAck.Global || bindAck.Name != "pane-died" {
		t.Fatalf("unexpected bind ack: %+v (body=%s)", bindAck, body)
	}

	unsetParams := mustJSON(t, map[string]any{
		"name": "set_hook",
		"arguments": map[string]any{
			"name":   "pane-died",
			"target": "hk",
			"unset":  true,
		},
	})
	res, rerr = tools.Handle(ctx, "tools/call", unsetParams)
	if rerr != nil {
		t.Fatalf("set_hook unset: %s", rerr.Message)
	}
	body = extractText(t, res)
	var unsetAck struct {
		Set    bool   `json:"set"`
		Unset  bool   `json:"unset"`
		Global bool   `json:"global"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal([]byte(body), &unsetAck); err != nil {
		t.Fatalf("decode set_hook unset ack: %v\nbody=%s", err, body)
	}
	if !unsetAck.Set || !unsetAck.Unset || unsetAck.Global || unsetAck.Name != "pane-died" {
		t.Fatalf("unexpected unset ack: %+v (body=%s)", unsetAck, body)
	}
	// A second unset against the now-cleared hook must succeed
	// (idempotent): tmux's `set-hook -u` is content with the no-op,
	// and the wrapper preserves that contract so deployment scripts
	// can re-run their teardown unconditionally.
	if _, rerr := tools.Handle(ctx, "tools/call", unsetParams); rerr != nil {
		t.Fatalf("set_hook unset (idempotent re-run): %s", rerr.Message)
	}
}

// TestHandle_SetHook_Global pins the -g (server-wide) path through the
// dispatcher: bind a global hook, verify the controller-level
// SetHook call accepted the "no target" form, and the unset round-trip
// returns success.
func TestHandle_SetHook_Global(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	// Anchor the daemon with a real session so the global bind exercises
	// "server up, hook installed against global table".
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "anchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	bindParams := mustJSON(t, map[string]any{
		"name": "set_hook",
		"arguments": map[string]any{
			"name":    "client-attached",
			"command": `display-message "global"`,
			"global":  true,
		},
	})
	res, rerr := tools.Handle(ctx, "tools/call", bindParams)
	if rerr != nil {
		t.Fatalf("set_hook -g bind: %s", rerr.Message)
	}
	body := extractText(t, res)
	var ack struct {
		Set    bool `json:"set"`
		Global bool `json:"global"`
	}
	if err := json.Unmarshal([]byte(body), &ack); err != nil {
		t.Fatalf("decode set_hook bind ack: %v\nbody=%s", err, body)
	}
	if !ack.Set || !ack.Global {
		t.Fatalf("unexpected ack: %+v (body=%s)", ack, body)
	}

	unsetParams := mustJSON(t, map[string]any{
		"name": "set_hook",
		"arguments": map[string]any{
			"name":   "client-attached",
			"global": true,
			"unset":  true,
		},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", unsetParams); rerr != nil {
		t.Fatalf("set_hook -g unset: %s", rerr.Message)
	}
}

// TestHandle_SetHook_RejectsMissingName pins the required-field path:
// omitting `name` must come back as CodeInvalidParams rather than
// falling through to tmux with an empty hook name (which would surface
// a less helpful "too few arguments" stderr).
func TestHandle_SetHook_RejectsMissingName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "set_hook",
		"arguments": map[string]any{"command": `display-message "x"`, "target": "any"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing name")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q", rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
}

// TestHandle_SetHook_RejectsBadName locks the regex check on `name`.
// A stray semicolon / whitespace / shell metachar must not slip
// through to the tmux argv.
func TestHandle_SetHook_RejectsBadName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	for _, bad := range []string{
		"pane-died;rm -rf /",
		"pane died",
		"pane.died",
		`pane"died`,
	} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "set_hook",
				"arguments": map[string]any{
					"name":    bad,
					"command": `display-message "x"`,
					"target":  "any",
				},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params error for bad name %q", bad)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_SetHook_RejectsMissingCommandOnBind locks the bind-path
// guard. A non-unset call without a command must come back as
// CodeInvalidParams — without the up-front check tmux would emit a
// less helpful "too few arguments" stderr the caller would have to
// substring-match.
func TestHandle_SetHook_RejectsMissingCommandOnBind(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "set_hook",
		"arguments": map[string]any{
			"name":   "pane-died",
			"target": "any",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing command on bind path")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q", rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
}

// TestHandle_SetHook_RejectsControlCharsInCommand pins the
// command-body shape policy. NUL and other ASCII control bytes have
// no place in a tmux command line and admitting them risks an
// injected escape sequence taking effect when tmux later renders the
// hook in show-options output.
func TestHandle_SetHook_RejectsControlCharsInCommand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	for _, bad := range []string{
		"display-message \x00\"x\"",
		"display-message \x07x",
		"display-message \x1bx", // ESC
	} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "set_hook",
				"arguments": map[string]any{
					"name":    "pane-died",
					"command": bad,
					"target":  "any",
				},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params error for command with control chars %q", bad)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_SetHook_RejectsMissingTargetWhenNotGlobal locks the
// per-session bind-path target check. Without the up-front guard tmux
// would resolve "" to whatever session it considered current,
// silently mis-routing the hook against a stale target.
func TestHandle_SetHook_RejectsMissingTargetWhenNotGlobal(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "set_hook",
		"arguments": map[string]any{
			"name":    "pane-died",
			"command": `display-message "x"`,
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing target when not global")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q", rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
}

// TestHandle_SetHook_MissingSessionMapsCode pins the wire contract
// that set_hook against an unknown target session surfaces
// CodeSessionNotFound (-32000), mirroring clear_history /
// session_describe / pane_kill.
func TestHandle_SetHook_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, target
	// missing" rather than "no server" (different stderr shape).
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "anchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	params := mustJSON(t, map[string]any{
		"name": "set_hook",
		"arguments": map[string]any{
			"name":    "pane-died",
			"command": `display-message "x"`,
			"target":  "ghost_session_xyzzy",
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

// TestHandle_SetHook_ListedInTools confirms the init()-time
// registration actually wired set_hook into tools/list. Without this
// guard a regression in the package-init append could silently drop
// the tool from the surface even though the dispatcher still
// recognised it.
func TestHandle_SetHook_ListedInTools(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] == "set_hook" {
			return
		}
	}
	t.Fatal("tools/list missing set_hook")
}

// TestSetHook_NotInReadOnlyAllowlist pins the policy: set_hook
// installs / removes tmux hooks (long-lived behaviour changes the
// daemon will run on every event) and is therefore mutating, so a
// `-read-only` operator must not be able to invoke it. Adding the
// tool to the allowlist would silently let a read-only agent rewire
// the daemon's behaviour.
func TestSetHook_NotInReadOnlyAllowlist(t *testing.T) {
	t.Parallel()
	if IsReadOnlyTool("set_hook") {
		t.Fatal("set_hook must NOT be in the read-only allowlist (it mutates tmux hook bindings)")
	}
}
