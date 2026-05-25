package tmuxctl

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestChooseClient_HeadlessReturnsSentinel pins the load-bearing
// "no clients attached" path: the headless tmux servers tmux-mcp owns
// have nothing watching them, so `choose-client` cannot do anything
// useful and the controller must refuse the call up front with a
// wrapped errs.ErrSessionNotFound. Without this contract the JSON-RPC
// layer would surface a bogus success — the chooser tmux silently
// queued would never appear.
func TestChooseClient_HeadlessReturnsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the controller's tmux server is
	// definitely up — the empty list-clients path we want to exercise
	// is "server up, no clients attached", not "no server at all".
	if err := c.CreateSession(ctx, SessionSpec{Name: "cc_headless", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.ChooseClient(ctx, "", "", "", "", "", "", false, false, false)
	if err == nil {
		t.Fatal("expected error for headless server")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestChooseClient_MissingTargetWrapsSentinel pins the typed-error
// contract for an unknown `-t` target. tmux phrases the failure as
// "can't find pane" / "can't find window" depending on which half of
// the target it tried to resolve; the controller must translate either
// shape into errs.ErrSessionNotFound so the JSON-RPC layer can map the
// failure to CodeSessionNotFound regardless of the tmux build on PATH.
func TestChooseClient_MissingTargetWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we hit the "server up, target
	// missing" branch rather than the entirely-different "no server
	// running" stderr shape.
	if err := c.CreateSession(ctx, SessionSpec{Name: "cc_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.ChooseClient(ctx, "%99999", "", "", "", "", "", false, false, false)
	if err == nil {
		t.Fatal("expected error for missing target pane")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestChooseClient_HappyPath drives the full success path: a session
// with a real client attached must accept `choose-client` cleanly and
// return without error. The flag-forwarding paths (every optional
// argument set to a representative non-empty value) are exercised in
// the same call so a regression in argv assembly (e.g. swapping `-F`
// and `-f`) tickles a tmux usage error rather than going unnoticed.
//
// Linux-only because the helper that fakes a tmux client uses
// /dev/ptmx + ioctl directly. Darwin runners skip cleanly via the
// runtime check rather than failing on a missing syscall.
func TestChooseClient_HappyPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	if runtime.GOOS != "linux" {
		t.Skip("happy path requires the Linux pty helper")
	}
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	const name = "cc_happy"
	if err := c.CreateSession(ctx, SessionSpec{
		Name: name, Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Spawn a fake client on a private pty so list-clients reports a
	// non-empty set. attachFakeClient parks until the client appears
	// to tmux and registers a t.Cleanup that tears it down.
	attachFakeClient(t, c, name)

	// Every optional argument is passed so the argv builder is
	// exercised end-to-end. The values are deliberately mundane
	// (matching tmux defaults) so no failure path is taken inside
	// tmux's own parsing.
	err := c.ChooseClient(ctx,
		name+":0.0", // target
		"#{client_tty}",
		"1",          // filter accepts a tmux conditional
		"N",          // key-format
		"name",       // sort-order
		`display ""`, // template (no-op tmux command)
		true, true, true,
	)
	if err != nil {
		t.Fatalf("ChooseClient: %v", err)
	}
}
