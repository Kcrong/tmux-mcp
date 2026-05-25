package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestHandle_RespawnWindow_RestartsWithKill drives the happy path: a
// window whose foreground command is busy is forcibly respawned with
// kill=true, and the boundary returns the documented `{"respawned":
// true}` body. We pin pane_pid before/after through tmux directly so a
// regression where the handler returned success without actually
// restarting the window would fail loudly.
func TestHandle_RespawnWindow_RestartsWithKill(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	call := func(name string, args any) any {
		t.Helper()
		params := mustJSON(t, map[string]any{"name": name, "arguments": args})
		res, rerr := tools.Handle(ctx, "tools/call", params)
		if rerr != nil {
			t.Fatalf("%s: %s", name, rerr.Message)
		}
		return res
	}

	// `sleep 60` keeps the window busy past the test deadline; kill=true
	// is the documented recovery path so we exercise it here rather than
	// staging a remain-on-exit corpse (covered by respawn_pane already).
	call("session_create", map[string]any{
		"name": "rwa", "command": "sleep 60", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "rwa"}}))
	})

	socket := tools.Ctl.Socket()
	pidBefore := strings.TrimSpace(runTmux(t, socket,
		"display-message", "-p", "-t", "rwa:0", "#{pane_pid}"))
	if pidBefore == "" {
		t.Fatal("pre-respawn pane_pid empty")
	}

	body := extractText(t, call("respawn_window", map[string]any{
		"session": "rwa",
		"window":  "0",
		"command": "sleep 30",
		"kill":    true,
	}))
	var resp struct {
		Respawned bool `json:"respawned"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode respawn_window: %v\nbody=%s", err, body)
	}
	if !resp.Respawned {
		t.Fatalf("respawn_window: respawned=false, body=%s", body)
	}

	pidAfter := strings.TrimSpace(runTmux(t, socket,
		"display-message", "-p", "-t", "rwa:0", "#{pane_pid}"))
	if pidAfter == "" || pidAfter == pidBefore {
		t.Fatalf("pane_pid after respawn = %q (before=%q), want a fresh pid",
			pidAfter, pidBefore)
	}
}

// TestHandle_RespawnWindow_ActiveWithoutKillReturnsCode locks the
// CodePaneActive (-32005) wire contract: the window is still running
// its original command, so tmux refuses respawn-window unless the
// caller passed kill=true. The code is deliberately the same one
// respawn_pane uses — clients branch on a single typed code and retry
// with kill=true regardless of whether the call was pane- or window-
// scoped, instead of having to learn a second sentinel.
func TestHandle_RespawnWindow_ActiveWithoutKillReturnsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	call := func(name string, args any) any {
		t.Helper()
		params := mustJSON(t, map[string]any{"name": name, "arguments": args})
		res, rerr := tools.Handle(ctx, "tools/call", params)
		if rerr != nil {
			t.Fatalf("%s: %s", name, rerr.Message)
		}
		return res
	}

	call("session_create", map[string]any{
		"name": "rwb", "command": "sleep 60", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "rwb"}}))
	})

	params := mustJSON(t, map[string]any{
		"name": "respawn_window",
		"arguments": map[string]any{
			"session": "rwb",
			"window":  "0",
			"command": "echo replaced",
		},
	})
	_, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatal("expected error when respawning an active window without kill=true")
	}
	if rerr.Code != errs.CodePaneActive {
		t.Fatalf("code = %d, want CodePaneActive (%d), msg=%q",
			rerr.Code, errs.CodePaneActive, rerr.Message)
	}

	// Sanity: with kill=true the same call now succeeds. Pins the
	// "client retries with kill=true" recovery path the typed code is
	// meant to enable.
	body := extractText(t, call("respawn_window", map[string]any{
		"session": "rwb",
		"window":  "0",
		"command": "sleep 30",
		"kill":    true,
	}))
	var resp struct {
		Respawned bool `json:"respawned"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode respawn_window: %v\nbody=%s", err, body)
	}
	if !resp.Respawned {
		t.Fatalf("respawn_window with kill=true: respawned=false, body=%s", body)
	}
}

// TestHandle_RespawnWindow_CommandReplaceRoundTrips proves the optional
// `command` field actually replaces what the window runs after the
// respawn. We start the window with `sleep 60`, force a respawn with a
// sentinel echo command (kill=true so the busy branch does not trip),
// then capture the pane and assert the sentinel landed. Without this
// pin a regression where the handler dropped the command (e.g.
// forgetting to plumb args.Command to the controller) would only show
// up as "the wrong process is running", invisible to CI.
func TestHandle_RespawnWindow_CommandReplaceRoundTrips(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	call := func(name string, args any) any {
		t.Helper()
		params := mustJSON(t, map[string]any{"name": name, "arguments": args})
		res, rerr := tools.Handle(ctx, "tools/call", params)
		if rerr != nil {
			t.Fatalf("%s: %s", name, rerr.Message)
		}
		return res
	}

	call("session_create", map[string]any{
		"name": "rwc", "command": "sleep 60", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "rwc"}}))
	})

	socket := tools.Ctl.Socket()

	const sentinel = "hello-respawn-window-server-42"
	// Chain a sleep after the echo so the pty stays open long enough for
	// capture-pane to see the buffer. A bare `echo` exits immediately
	// and tmux 3.4's "Pane is dead" overlay races the agent's capture.
	body := extractText(t, call("respawn_window", map[string]any{
		"session": "rwc",
		"window":  "0",
		"command": "sh -c 'echo " + sentinel + "; sleep 30'",
		"kill":    true,
	}))
	var resp struct {
		Respawned bool `json:"respawned"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode respawn_window: %v\nbody=%s", err, body)
	}
	if !resp.Respawned {
		t.Fatalf("respawn_window: respawned=false, body=%s", body)
	}

	// Poll the pane for the sentinel — tmux's pty has its own scheduling
	// and capturing immediately after the respawn races the child's
	// first write on slow runners.
	deadline := time.Now().Add(5 * time.Second)
	var pane string
	for time.Now().Before(deadline) {
		pane = runTmux(t, socket, "capture-pane", "-p", "-t", "rwc:0")
		if strings.Contains(pane, sentinel) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(pane, sentinel) {
		t.Fatalf("pane did not show sentinel %q after respawn; body=%q",
			sentinel, pane)
	}
}

// TestHandle_RespawnWindow_CwdAppliesStartDirectory pins the -c
// plumbing through the JSON-RPC boundary. We respawn the window with
// `pwd` and a temporary directory as cwd, then assert the captured
// pane prints that path. A regression where the handler validated cwd
// but never forwarded it to the controller would slip through every
// other test.
func TestHandle_RespawnWindow_CwdAppliesStartDirectory(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	call := func(name string, args any) any {
		t.Helper()
		params := mustJSON(t, map[string]any{"name": name, "arguments": args})
		res, rerr := tools.Handle(ctx, "tools/call", params)
		if rerr != nil {
			t.Fatalf("%s: %s", name, rerr.Message)
		}
		return res
	}

	call("session_create", map[string]any{
		"name": "rwd", "command": "sleep 60", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "rwd"}}))
	})

	dir := t.TempDir()
	// Symlink resolution differs across runners (macOS prefixes /private,
	// some Linux runners use /tmp directly). Resolve the real path once
	// so the assertion below matches whatever pwd prints.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	socket := tools.Ctl.Socket()

	// Chain a sleep after pwd so the pty stays open long enough for
	// capture-pane to see the buffer; tmux 3.4's "Pane is dead" overlay
	// otherwise wipes the pwd output the moment the child exits.
	body := extractText(t, call("respawn_window", map[string]any{
		"session": "rwd",
		"window":  "0",
		"command": "sh -c 'pwd; sleep 30'",
		"cwd":     resolved,
		"kill":    true,
	}))
	var resp struct {
		Respawned bool `json:"respawned"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode respawn_window: %v\nbody=%s", err, body)
	}
	if !resp.Respawned {
		t.Fatalf("respawn_window: respawned=false, body=%s", body)
	}

	deadline := time.Now().Add(5 * time.Second)
	var pane string
	for time.Now().Before(deadline) {
		pane = runTmux(t, socket, "capture-pane", "-p", "-t", "rwd:0")
		if strings.Contains(pane, resolved) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(pane, resolved) {
		t.Fatalf("pane did not show cwd %q after respawn; body=%q",
			resolved, pane)
	}
}

// TestHandle_RespawnWindow_RejectsMissingFields pins CodeInvalidParams
// for the two required components. tmux would otherwise resolve an
// empty target to whatever window it considers current — never what the
// caller intended.
func TestHandle_RespawnWindow_RejectsMissingFields(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	cases := []struct {
		name string
		args map[string]any
	}{
		{name: "missing session", args: map[string]any{"window": "0"}},
		{name: "missing window", args: map[string]any{"session": "rw"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name":      "respawn_window",
				"arguments": tc.args,
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatal("expected invalid params error")
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
					rerr.Code, errs.CodeInvalidParams, rerr.Message)
			}
		})
	}
}

// TestHandle_RespawnWindow_RejectsBadFields locks the per-field regex
// and absolute-path guards. Each case smuggles a metachar that tmux's
// argv parser would otherwise treat specially (colon to switch target
// form, dot to split pane index, semicolon to chain shell commands)
// or a relative path that would be resolved against the server's own
// cwd.
func TestHandle_RespawnWindow_RejectsBadFields(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	cases := []struct {
		name string
		args map[string]any
	}{
		{
			name: "bad session",
			args: map[string]any{"session": "rw;rm -rf /", "window": "0"},
		},
		{
			name: "bad window",
			args: map[string]any{"session": "rw", "window": "0:1"},
		},
		{
			name: "relative cwd",
			args: map[string]any{"session": "rw", "window": "0", "cwd": "../etc"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name":      "respawn_window",
				"arguments": tc.args,
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatal("expected invalid params error")
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
					rerr.Code, errs.CodeInvalidParams, rerr.Message)
			}
		})
	}
}

// TestHandle_RespawnWindow_RejectsCommandNewline pins the explicit
// newline guard on the optional `command`. tmux would otherwise
// interpret a newline as a command separator on /bin/sh -c, breaking
// the documented "single command" contract. Mirrors respawn_pane's
// guard so both tools share the same policy.
func TestHandle_RespawnWindow_RejectsCommandNewline(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "respawn_window",
		"arguments": map[string]any{
			"session": "rw",
			"window":  "0",
			"command": "echo hi\nrm -rf /",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for command containing newline")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d), msg=%q",
			rerr.Code, errs.CodeInvalidParams, rerr.Message)
	}
	if !strings.Contains(rerr.Message, "newline") {
		t.Fatalf("expected message to mention newline, got %q", rerr.Message)
	}
}

// TestHandle_RespawnWindow_MissingSessionMapsCode pins the wire
// contract that respawn_window against an unknown session surfaces
// CodeSessionNotFound (-32000), mirroring respawn_pane / swap_window /
// kill_window.
func TestHandle_RespawnWindow_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we hit the "server up, target
	// missing" branch (different stderr from the "no server" case).
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "rwanchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "rwanchor"}}))
	})

	params := mustJSON(t, map[string]any{
		"name": "respawn_window",
		"arguments": map[string]any{
			"session": "ghost_session_xyzzy",
			"window":  "0",
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

// TestHandle_ToolsList_IncludesRespawnWindow makes sure tools/list
// advertises the new tool so MCP clients can discover its schema.
func TestHandle_ToolsList_IncludesRespawnWindow(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "respawn_window" {
			return
		}
	}
	t.Fatalf("tools/list missing respawn_window")
}
