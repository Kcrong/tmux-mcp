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

// runTmux drives a raw tmux command against the controller's private
// socket. respawn_pane's integration tests need to set
// `remain-on-exit on` and force the pane's foreground process to exit
// without the controller surface exposing those primitives — going
// straight at tmux keeps the test independent of what the controller
// API happens to wrap today.
func runTmux(t *testing.T, socket string, args ...string) string {
	t.Helper()
	full := append([]string{"-S", socket}, args...)
	cmd := exec.Command("tmux", full...) //nolint:gosec // socket is a private path produced by tmuxctl.New
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("tmux %s: %v\nstderr=%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

// waitPaneDead polls `display-message #{pane_dead}` until it flips to
// 1 or the deadline is hit. Used after sending the foreground process
// a signal: the pane stays alive (because remain-on-exit is on) but
// its child has exited, which is exactly the "natural exit" branch we
// want respawn_pane to recover.
func waitPaneDead(t *testing.T, socket, target string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out := runTmux(t, socket, "display-message", "-p", "-t", target, "#{pane_dead}")
		if strings.TrimSpace(out) == "1" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pane %s never reached pane_dead=1 within %s", target, timeout)
}

// TestHandle_RespawnPane_AfterNaturalExit drives the happy path: a
// session whose foreground command has exited (with remain-on-exit on
// so the pane survives) is brought back via respawn_pane without the
// caller setting kill=true. The response carries `respawned:true` and
// the pane's pane_dead flips back to 0 with a fresh pid.
func TestHandle_RespawnPane_AfterNaturalExit(t *testing.T) {
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

	// `sleep 30` keeps the pane busy long enough that we can flip
	// remain-on-exit on before the foreground process exits — without
	// it the only-pane session would tear down the moment the child
	// died, taking the window with it.
	call("session_create", map[string]any{
		"name": "rpa", "command": "sleep 30", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "rpa"}}))
	})

	socket := tools.Ctl.Socket()

	// Pin the pane in place after its child exits so respawn_pane has
	// something to recover. Without this option the pane (and its
	// solitary window) would vanish when sleep dies.
	runTmux(t, socket, "set-window-option", "-t", "rpa:0", "remain-on-exit", "on")

	// Kill the foreground sleep with Ctrl-C. The pane stays alive
	// because of the option above; pane_dead flips to 1 once the
	// child finishes.
	runTmux(t, socket, "send-keys", "-t", "rpa:0.0", "C-c")
	waitPaneDead(t, socket, "rpa:0.0", 5*time.Second)

	// Capture the original pid so we can prove the respawn really
	// started a new child.
	pidBefore := strings.TrimSpace(runTmux(t, socket,
		"display-message", "-p", "-t", "rpa:0.0", "#{pane_pid}"))

	body := extractText(t, call("respawn_pane", map[string]any{
		"session": "rpa",
		"window":  "0",
		"pane":    "0",
		"command": "sleep 60",
	}))
	var resp struct {
		Respawned bool `json:"respawned"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode respawn_pane: %v\nbody=%s", err, body)
	}
	if !resp.Respawned {
		t.Fatalf("respawn_pane: respawned=false, body=%s", body)
	}

	// pane_dead must flip back to 0 and the pid must change — that
	// is the difference between "respawn worked" and "tmux silently
	// returned without restarting".
	deadAfter := strings.TrimSpace(runTmux(t, socket,
		"display-message", "-p", "-t", "rpa:0.0", "#{pane_dead}"))
	if deadAfter != "0" {
		t.Fatalf("pane_dead after respawn = %q, want 0", deadAfter)
	}
	pidAfter := strings.TrimSpace(runTmux(t, socket,
		"display-message", "-p", "-t", "rpa:0.0", "#{pane_pid}"))
	if pidAfter == "" || pidAfter == pidBefore {
		t.Fatalf("pane_pid after respawn = %q (before=%q), want a fresh pid",
			pidAfter, pidBefore)
	}
}

// TestHandle_RespawnPane_ActiveWithoutKillReturnsCode locks the
// CodePaneActive (-32005) wire contract: the pane is still running its
// original command, so tmux refuses respawn-pane unless the caller
// passed kill=true. Clients branch on the code to retry.
func TestHandle_RespawnPane_ActiveWithoutKillReturnsCode(t *testing.T) {
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

	// `sleep 60` keeps the pane busy past the test deadline so the
	// "still active" branch is the one we hit.
	call("session_create", map[string]any{
		"name": "rpb", "command": "sleep 60", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "rpb"}}))
	})

	params := mustJSON(t, map[string]any{
		"name": "respawn_pane",
		"arguments": map[string]any{
			"session": "rpb",
			"window":  "0",
			"pane":    "0",
			"command": "echo replaced",
		},
	})
	_, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr == nil {
		t.Fatal("expected error when respawning an active pane without kill=true")
	}
	if rerr.Code != errs.CodePaneActive {
		t.Fatalf("code = %d, want CodePaneActive (%d), msg=%q",
			rerr.Code, errs.CodePaneActive, rerr.Message)
	}

	// Sanity: with kill=true the same call now succeeds. This pins
	// the "client retries with kill=true" recovery path the typed
	// code is meant to enable.
	body := extractText(t, call("respawn_pane", map[string]any{
		"session": "rpb",
		"window":  "0",
		"pane":    "0",
		"command": "sleep 30",
		"kill":    true,
	}))
	var resp struct {
		Respawned bool `json:"respawned"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode respawn_pane: %v\nbody=%s", err, body)
	}
	if !resp.Respawned {
		t.Fatalf("respawn_pane with kill=true: respawned=false, body=%s", body)
	}
}

// TestHandle_RespawnPane_RejectsMissingFields pins CodeInvalidParams
// for the three required components. tmux would otherwise resolve an
// empty target to whatever pane it considers current — never what the
// caller intended.
func TestHandle_RespawnPane_RejectsMissingFields(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	cases := []struct {
		name string
		args map[string]any
	}{
		{name: "missing session", args: map[string]any{"window": "0", "pane": "0"}},
		{name: "missing window", args: map[string]any{"session": "rp", "pane": "0"}},
		{name: "missing pane", args: map[string]any{"session": "rp", "window": "0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name":      "respawn_pane",
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

// TestHandle_RespawnPane_RejectsBadFields locks the per-field regex
// guards. Each case smuggles a metachar that tmux's argv parser would
// otherwise treat specially (colon to switch target form, dot to split
// pane index, semicolon to chain shell commands).
func TestHandle_RespawnPane_RejectsBadFields(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)

	cases := []struct {
		name string
		args map[string]any
	}{
		{
			name: "bad session",
			args: map[string]any{"session": "rp;rm -rf /", "window": "0", "pane": "0"},
		},
		{
			name: "bad window",
			args: map[string]any{"session": "rp", "window": "0:1", "pane": "0"},
		},
		{
			name: "bad pane",
			args: map[string]any{"session": "rp", "window": "0", "pane": "0.1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name":      "respawn_pane",
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

// TestHandle_RespawnPane_RejectsCommandNewline pins the explicit
// newline guard on the optional `command`. tmux would otherwise
// interpret a newline as a command separator on /bin/sh -c, breaking
// the documented "single command" contract.
func TestHandle_RespawnPane_RejectsCommandNewline(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "respawn_pane",
		"arguments": map[string]any{
			"session": "rp",
			"window":  "0",
			"pane":    "0",
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

// TestHandle_RespawnPane_MissingSessionMapsCode pins the wire contract
// that respawn_pane against an unknown session surfaces
// CodeSessionNotFound (-32000), mirroring pane_break / pane_swap /
// pane_kill / pane_resize.
func TestHandle_RespawnPane_MissingSessionMapsCode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we hit the "server up, target
	// missing" branch (different stderr from the "no server" case).
	createParams := mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "rpanchor", "command": "/bin/sh"},
	})
	if _, rerr := tools.Handle(ctx, "tools/call", createParams); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{"name": "session_kill", "arguments": map[string]any{"name": "rpanchor"}}))
	})

	params := mustJSON(t, map[string]any{
		"name": "respawn_pane",
		"arguments": map[string]any{
			"session": "ghost_session_xyzzy",
			"window":  "0",
			"pane":    "0",
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

// TestHandle_ToolsList_IncludesRespawnPane makes sure tools/list
// advertises the new tool so MCP clients can discover its schema.
func TestHandle_ToolsList_IncludesRespawnPane(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %v", rerr)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if name, _ := def["name"].(string); name == "respawn_pane" {
			return
		}
	}
	t.Fatalf("tools/list missing respawn_pane")
}
