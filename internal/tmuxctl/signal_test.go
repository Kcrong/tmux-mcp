package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestSendSignal_TerminatesSleepingProcess is the integration test:
// drive a session that runs `sleep 100`, fire SIGTERM through the
// public API, and confirm the session ends within a short deadline
// (the sleep would otherwise pin tmux for 100s).
//
// We also keep an anchor session running so the tmux server itself
// does not exit when the target session vanishes — that race makes
// HasSession unreliable ("no server" vs "server exited unexpectedly"
// depending on timing).
func TestSendSignal_TerminatesSleepingProcess(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "anchor", Command: "/bin/sh", Width: 80, Height: 20,
	}); err != nil {
		t.Fatalf("CreateSession anchor: %v", err)
	}
	if err := c.CreateSession(ctx, SessionSpec{
		Name: "sig", Command: "sleep 100", Width: 80, Height: 20,
	}); err != nil {
		t.Fatalf("CreateSession sig: %v", err)
	}

	if err := c.SendSignal(ctx, "sig", "TERM"); err != nil {
		t.Fatalf("SendSignal: %v", err)
	}

	// SIGTERM on the pane PID should bubble up through tmux: the pane
	// closes when its child exits, and the session — being single-pane
	// and remain-on-exit=off by default — vanishes with it. Poll the
	// session list until "sig" is gone or we hit the deadline.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		has, herr := c.HasSession(ctx, "sig")
		if herr != nil {
			t.Fatalf("HasSession: %v", herr)
		}
		if !has {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("session sig still alive 5s after SIGTERM")
}

// TestSendSignal_RejectsUnknownSignal locks down the whitelist —
// anything outside SignalNames() must be refused without any tmux
// calls being attempted.
func TestSendSignal_RejectsUnknownSignal(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "rs", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	err := c.SendSignal(ctx, "rs", "STOP")
	if err == nil {
		t.Fatal("expected error for non-whitelisted signal")
	}
	if !strings.Contains(err.Error(), "whitelist") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSendSignal_MissingSessionWrapsSentinel pins the contract that
// asking to signal a session tmux doesn't know about surfaces the
// typed errs.ErrSessionNotFound — needed by the JSON-RPC layer to map
// this to CodeSessionNotFound.
func TestSendSignal_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	// Anchor the tmux server with a real session so display-message
	// hits the "server is up but the named session does not exist"
	// branch rather than the "no server" branch.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	err := c.SendSignal(ctx, "ghost_session_nonexistent", "TERM")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSendSignal_RejectsEmptySession guards the up-front check —
// tmux would otherwise resolve "" to whatever it considers current.
func TestSendSignal_RejectsEmptySession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	err := c.SendSignal(ctx, "", "TERM")
	if err == nil {
		t.Fatal("expected error for empty session")
	}
	if !strings.Contains(err.Error(), "session required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSignalNames_StableOrder pins the public ordering of the
// whitelist. Tool schemas and error messages depend on this slice
// being deterministic across invocations and Go versions.
func TestSignalNames_StableOrder(t *testing.T) {
	t.Parallel()
	got := SignalNames()
	want := []string{"TERM", "HUP", "INT", "QUIT", "USR1", "USR2", "KILL"}
	if len(got) != len(want) {
		t.Fatalf("len(SignalNames) = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("SignalNames[%d] = %q, want %q", i, got[i], w)
		}
	}
}
