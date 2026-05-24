package tmuxctl

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func skipIfNoTmux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("tmux tests require unix-like OS")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
}

func newCtl(t *testing.T) *Controller {
	t.Helper()
	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Shutdown(context.Background()) })
	return c
}

func TestSessionLifecycle(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "alpha", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	names, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(names) != 1 || names[0] != "alpha" {
		t.Fatalf("unexpected sessions: %v", names)
	}
	has, err := c.HasSession(ctx, "alpha")
	if err != nil || !has {
		t.Fatalf("HasSession: has=%v err=%v", has, err)
	}
	if err := c.KillSession(ctx, "alpha"); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
}

func TestSendKeysAndCapture(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{
		Name:    "echo",
		Command: "/bin/sh",
		Width:   80, Height: 20,
		Env: map[string]string{"PS1": "$ "},
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Drive the shell: print a sentinel string.
	if err := c.SendKeys(ctx, "echo", []string{"echo TMUX_MCP_HELLO_42", "Enter"}, false); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	body, err := c.WaitForStable(ctx, "echo", 300*time.Millisecond, 100*time.Millisecond, 5*time.Second)
	if err != nil {
		t.Fatalf("WaitForStable: %v", err)
	}
	if !strings.Contains(body, "TMUX_MCP_HELLO_42") {
		t.Fatalf("captured body did not contain sentinel:\n%s", body)
	}
}

func TestWaitForText(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "wait", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := c.SendKeys(ctx, "wait", []string{"printf 'READY-%s\\n' 99", "Enter"}, false); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	m, err := c.WaitForText(ctx, "wait", `READY-\d+`, 50*time.Millisecond, 5*time.Second)
	if err != nil {
		t.Fatalf("WaitForText: %v", err)
	}
	if !strings.HasPrefix(m.Match, "READY-") {
		t.Fatalf("match = %q", m.Match)
	}
}

func TestWaitForText_TimesOut(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "to", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, err := c.WaitForText(ctx, "to", `IMPOSSIBLE_PATTERN_XYZZY`, 50*time.Millisecond, 400*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "not found within") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResize(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "rs", Command: "/bin/sh", Width: 80, Height: 24}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := c.Resize(ctx, "rs", 100, 30); err != nil {
		t.Fatalf("Resize: %v", err)
	}
}

func TestListSessions_EmptyOnFreshController(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	names, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("expected no sessions, got %v", names)
	}
}
