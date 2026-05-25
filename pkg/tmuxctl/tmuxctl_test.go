package tmuxctl_test

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/pkg/tmuxctl"
)

// skipIfNoTmux mirrors the internal package's helper so the public
// smoke test can run on CI hosts that have tmux while still being
// portable to developer machines that do not.
func skipIfNoTmux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("tmux tests require unix-like OS")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
}

// TestPublicSurface_CreateAndList exercises the full public API:
// constructor, SessionSpec, CreateSession, ListSessions, SendKeys,
// WaitForStable, KillSession, Shutdown. If this test compiles and
// passes, downstream consumers can rely on the same surface.
func TestPublicSurface_CreateAndList(t *testing.T) {
	skipIfNoTmux(t)

	c, err := tmuxctl.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Shutdown(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, tmuxctl.SessionSpec{
		Name:    "public-smoke",
		Command: "/bin/sh",
		Width:   80,
		Height:  24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	names, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(names) != 1 || names[0] != "public-smoke" {
		t.Fatalf("unexpected sessions: %v", names)
	}

	if err := c.SendKeys(ctx, "public-smoke",
		[]string{"echo PUBLIC_OK_42", "Enter"}, false); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	body, err := c.WaitForStable(ctx, "public-smoke",
		300*time.Millisecond, 100*time.Millisecond, 5*time.Second)
	if err != nil {
		t.Fatalf("WaitForStable: %v", err)
	}
	if !strings.Contains(body, "PUBLIC_OK_42") {
		t.Fatalf("captured body did not contain sentinel:\n%s", body)
	}

	if err := c.KillSession(ctx, "public-smoke"); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
}

// TestCaptureModeConstants makes sure the re-exported constants compare
// equal to themselves under the alias and can be used as Capture's mode
// argument. This is the kind of thing a consumer is likely to do, so we
// pin the behaviour explicitly.
func TestCaptureModeConstants(t *testing.T) {
	if tmuxctl.CaptureVisible == tmuxctl.CaptureScrollback {
		t.Fatal("CaptureVisible and CaptureScrollback must differ")
	}
	m := tmuxctl.CaptureVisible
	if m != tmuxctl.CaptureVisible {
		t.Fatalf("CaptureMode round-trip failed: got %v", m)
	}
}
