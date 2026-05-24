package tmuxctl

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// raw drives a tmux command against the controller's private socket
// for setup the public API does not expose (e.g. set-window-option,
// signaling the foreground PID directly). Tests that need to flip
// remain-on-exit on or push the pane into pane_dead=1 use this; it
// stays out of the production surface so the controller's vocabulary
// does not grow just to support tests.
func raw(t *testing.T, c *Controller, args ...string) string {
	t.Helper()
	full := append([]string{"-S", c.Socket()}, args...)
	cmd := exec.Command(c.bin, full...) //nolint:gosec // c.bin is validated; socket is private
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("tmux %s: %v\nstderr=%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

// TestRespawnPane_RestartsAfterNaturalExit drives the happy path: a
// pane whose foreground process exited (with remain-on-exit on so the
// pane survives) is brought back via RespawnPane without -k. The
// pid changes and pane_dead flips back to 0 — that is the difference
// between "respawn worked" and "tmux silently no-op'd".
func TestRespawnPane_RestartsAfterNaturalExit(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "rpa", Command: "sleep 30", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Pin the pane in place after the foreground process exits;
	// otherwise the only-pane session would tear down with it.
	raw(t, c, "set-window-option", "-t", "rpa:0", "remain-on-exit", "on")

	// Ctrl-C the sleep. Pane stays alive because of the option;
	// pane_dead flips to 1 once the shell-less child exits.
	raw(t, c, "send-keys", "-t", "rpa:0.0", "C-c")

	// Wait for pane_dead=1.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out := raw(t, c, "display-message", "-p", "-t", "rpa:0.0", "#{pane_dead}")
		if strings.TrimSpace(out) == "1" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := strings.TrimSpace(raw(t, c, "display-message", "-p", "-t", "rpa:0.0", "#{pane_dead}")); got != "1" {
		t.Fatalf("pane never reached pane_dead=1; got %q", got)
	}

	pidBefore := strings.TrimSpace(raw(t, c, "display-message", "-p", "-t", "rpa:0.0", "#{pane_pid}"))

	if err := c.RespawnPane(ctx, "rpa", "0", "0", "sleep 60", false); err != nil {
		t.Fatalf("RespawnPane: %v", err)
	}

	if got := strings.TrimSpace(raw(t, c, "display-message", "-p", "-t", "rpa:0.0", "#{pane_dead}")); got != "0" {
		t.Fatalf("pane_dead post-respawn = %q, want 0", got)
	}
	pidAfter := strings.TrimSpace(raw(t, c, "display-message", "-p", "-t", "rpa:0.0", "#{pane_pid}"))
	if pidAfter == "" || pidAfter == pidBefore {
		t.Fatalf("pane_pid post-respawn = %q (before %q), want a fresh pid", pidAfter, pidBefore)
	}
}

// TestRespawnPane_ActiveWithoutKillWrapsSentinel pins the typed
// errs.ErrPaneActive contract: tmux refuses respawn-pane on a busy
// pane unless -k is passed, and the controller surfaces this as the
// typed sentinel so the JSON-RPC layer can return CodePaneActive.
// Clients branch on the code and retry with kill=true.
func TestRespawnPane_ActiveWithoutKillWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "rpb", Command: "sleep 60", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.RespawnPane(ctx, "rpb", "0", "0", "echo replaced", false)
	if err == nil {
		t.Fatal("expected error respawning active pane without kill=true")
	}
	if !errors.Is(err, errs.ErrPaneActive) {
		t.Fatalf("error %v does not wrap errs.ErrPaneActive", err)
	}

	// Sanity: kill=true succeeds on the same pane.
	if err := c.RespawnPane(ctx, "rpb", "0", "0", "sleep 30", true); err != nil {
		t.Fatalf("RespawnPane(kill=true): %v", err)
	}
}

// TestRespawnPane_MissingSessionWrapsSentinel pins the typed sentinel
// so the JSON-RPC layer can map "session/window/pane not found" to
// CodeSessionNotFound. Mirrors the BreakPane / SwapPane / ResizePane
// contract every other pane-scoped method upholds.
func TestRespawnPane_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we hit "server up, target missing"
	// rather than "no server" (different stderr shape).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.RespawnPane(ctx, "ghost_session_xyzzy", "0", "0", "echo hi", false)
	if err == nil {
		t.Fatal("expected error for missing target")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestRespawnPane_RejectsEmptyComponents pins the up-front contract:
// each of session / window / pane is required. tmux would otherwise
// resolve an empty target field to whatever it considers current.
func TestRespawnPane_RejectsEmptyComponents(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	cases := []struct {
		name             string
		session, win, pn string
		wantSubstr       string
	}{
		{"empty session", "", "0", "0", "session required"},
		{"empty window", "rp", "", "0", "window required"},
		{"empty pane", "rp", "0", "", "pane required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := c.RespawnPane(ctx, tc.session, tc.win, tc.pn, "", false)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error = %v, want to contain %q", err, tc.wantSubstr)
			}
		})
	}
}
