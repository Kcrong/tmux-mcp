package tmuxctl

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestRespawnWindow_RestartsWithKill drives the happy path with
// kill=true: a window whose foreground command is still running can be
// forcibly respawned, and tmux flips it back to a fresh process. We
// pin the contract by capturing pane_pid before and after — a different
// pid is the difference between "respawn worked" and "tmux silently
// no-op'd". Using kill=true keeps the test independent of `remain-on-
// exit` quirks across tmux versions; the natural-exit branch is
// covered separately for respawn_pane and is not load-bearing here.
func TestRespawnWindow_RestartsWithKill(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "rwa", Command: "sleep 60", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	pidBefore := strings.TrimSpace(raw(t, c,
		"display-message", "-p", "-t", "rwa:0", "#{pane_pid}"))
	if pidBefore == "" {
		t.Fatalf("pre-respawn pane_pid empty")
	}

	if err := c.RespawnWindow(ctx, "rwa", "0", "sleep 30", "", true); err != nil {
		t.Fatalf("RespawnWindow: %v", err)
	}

	pidAfter := strings.TrimSpace(raw(t, c,
		"display-message", "-p", "-t", "rwa:0", "#{pane_pid}"))
	if pidAfter == "" || pidAfter == pidBefore {
		t.Fatalf("pane_pid post-respawn = %q (before=%q), want a fresh pid",
			pidAfter, pidBefore)
	}
}

// TestRespawnWindow_ActiveWithoutKillWrapsSentinel pins the typed
// errs.ErrPaneActive contract for the window-scope respawn: tmux refuses
// respawn-window on a busy window unless -k is passed, and the
// controller surfaces this as the same typed sentinel respawn_pane
// uses so the JSON-RPC layer can return CodePaneActive. Reusing the
// sentinel — instead of minting a new "window active" one — keeps the
// recovery contract uniform: clients branch on the single code and
// retry with kill=true regardless of whether the original call was
// pane- or window-scoped.
func TestRespawnWindow_ActiveWithoutKillWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "rwb", Command: "sleep 60", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.RespawnWindow(ctx, "rwb", "0", "echo replaced", "", false)
	if err == nil {
		t.Fatal("expected error respawning active window without kill=true")
	}
	if !errors.Is(err, errs.ErrPaneActive) {
		t.Fatalf("error %v does not wrap errs.ErrPaneActive", err)
	}

	// Sanity: kill=true succeeds on the same window. Pins the recovery
	// path the typed code is meant to enable.
	if err := c.RespawnWindow(ctx, "rwb", "0", "sleep 30", "", true); err != nil {
		t.Fatalf("RespawnWindow(kill=true): %v", err)
	}
}

// TestRespawnWindow_MissingSessionWrapsSentinel pins the typed
// errs.ErrSessionNotFound sentinel so the JSON-RPC layer maps
// "session/window not found" to CodeSessionNotFound. Mirrors
// SwapWindow / SelectWindow / RenameWindow — every window-scoped tmuxctl
// method must uphold the same contract.
func TestRespawnWindow_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we hit "server up, target missing"
	// rather than "no server" (different stderr shape across tmux
	// versions).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.RespawnWindow(ctx, "ghost_session_xyzzy", "0", "echo hi", "", false)
	if err == nil {
		t.Fatal("expected error for missing target")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestRespawnWindow_CommandReplaceRoundTrips proves that a non-empty
// `command` actually replaces what the window runs after the respawn.
// We start the window with `sleep 60`, force a respawn with `echo
// hello-respawn-window` (and kill=true so the busy-pane branch does not
// trip), then assert the captured pane shows the sentinel. Without this
// pin, a regression where the controller silently dropped the command
// argv (e.g. swapping argv order so tmux saw the command as a flag
// value) would only surface as "the wrong process is running" — visible
// to a human, invisible to CI.
func TestRespawnWindow_CommandReplaceRoundTrips(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "rwc", Command: "sleep 60", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Pin the pane after the new short-lived process exits; otherwise
	// the only-pane window would tear down, taking the session with it
	// and racing the capture below.
	const sentinel = "hello-respawn-window-42"
	// Keep the new command alive long enough to read the buffer reliably.
	// A bare `echo` exits immediately and on tmux 3.4 with default
	// settings the pane's own "Pane is dead" overlay races the agent's
	// capture, so chain a sleep to hold the pty open.
	cmd := "sh -c 'echo " + sentinel + "; sleep 30'"
	if err := c.RespawnWindow(ctx, "rwc", "0", cmd, "", true); err != nil {
		t.Fatalf("RespawnWindow: %v", err)
	}

	// Wait for the echo to land in the pane buffer. Polling is
	// deliberate — tmux's pty has its own scheduling and capturing
	// "right now" can race the child's first write on slow runners.
	deadline := time.Now().Add(5 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		body = raw(t, c, "capture-pane", "-p", "-t", "rwc:0")
		if strings.Contains(body, sentinel) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(body, sentinel) {
		t.Fatalf("pane did not show sentinel %q after respawn; body=%q",
			sentinel, body)
	}
}

// TestRespawnWindow_CwdAppliesStartDirectory pins the -c plumbing: a
// non-empty cwd must reach tmux as the new starting directory. We
// respawn the window with `pwd` as the command and a temporary
// directory as cwd, then assert the captured pane prints that path.
// Without this pin, a regression where the controller dropped or
// reordered -c (e.g. emitting it after -t, which tmux's argv parser
// would tolerate but other test harnesses might not) would slip through.
func TestRespawnWindow_CwdAppliesStartDirectory(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "rwd", Command: "sleep 60", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	dir := t.TempDir()
	// Symlink resolution differs across runners (macOS prefixes /private,
	// some Linux runners use /tmp directly). Resolve the real path once
	// so the assertion below matches whatever pwd prints.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	// Keep the pane alive by chaining a sleep after pwd; otherwise tmux
	// 3.4 collapses the only-pane window the moment pwd exits and the
	// pane's own "Pane is dead" overlay races the capture below.
	if err := c.RespawnWindow(ctx, "rwd", "0", "sh -c 'pwd; sleep 30'", resolved, true); err != nil {
		t.Fatalf("RespawnWindow: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		body = raw(t, c, "capture-pane", "-p", "-t", "rwd:0")
		if strings.Contains(body, resolved) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(body, resolved) {
		t.Fatalf("pane did not show cwd %q after respawn; body=%q",
			resolved, body)
	}
}

// TestRespawnWindow_RejectsEmptyComponents pins the up-front contract:
// each of session / window is required. tmux would otherwise resolve an
// empty target field to whatever window it considers current — never
// what the caller intended. The pane-scope respawn already pins the
// same contract; mirroring it here keeps the two tools' guards in
// sync.
func TestRespawnWindow_RejectsEmptyComponents(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	cases := []struct {
		name         string
		session, win string
		wantSubstr   string
	}{
		{"empty session", "", "0", "session required"},
		{"empty window", "rw", "", "window required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := c.RespawnWindow(ctx, tc.session, tc.win, "", "", false)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error = %v, want to contain %q", err, tc.wantSubstr)
			}
		})
	}
}
