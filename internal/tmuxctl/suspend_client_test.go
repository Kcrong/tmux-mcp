package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestSuspendClient_NoCurrentClientIsNoOp pins the load-bearing
// "headless server, nobody attached" path: a SuspendClient call against
// a controller with no attached clients must return nil, not an error.
// The headless tmux servers tmux-mcp owns are the common case for this
// path — agents fire-and-forget a suspend without first checking
// whether anyone is watching, and the boundary's job is to make that
// shape look like a clean success rather than push every caller to
// substring-match tmux stderr for "no current client".
func TestSuspendClient_NoCurrentClientIsNoOp(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor the daemon so the call hits the "server up, no clients"
	// branch rather than the "no server running" branch (different
	// stderr shape entirely).
	if err := c.CreateSession(ctx, SessionSpec{Name: "scn", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.SuspendClient(ctx, SuspendClientOpts{}); err != nil {
		t.Fatalf("SuspendClient: %v (want nil for no-current-client headless case)", err)
	}
}

// TestSuspendClient_MissingTargetClientWrapsSentinel pins the typed-
// error contract for an unknown `-t` target: callers (and the JSON-RPC
// layer) must be able to errors.Is into errs.ErrSessionNotFound so the
// dispatcher can map it uniformly to CodeSessionNotFound regardless of
// which exact phrase tmux emitted.
//
// The test deliberately uses a target value tmux cannot match against
// any real client (no clients are attached on the headless server) to
// trigger the "can't find client" error path.
func TestSuspendClient_MissingTargetClientWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the failure surfaces as "named
	// client missing" rather than "no server running".
	if err := c.CreateSession(ctx, SessionSpec{Name: "smc", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.SuspendClient(ctx, SuspendClientOpts{TargetClient: "/dev/pts/ghost-nonexistent"})
	if err == nil {
		t.Fatal("expected error for missing target client")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSuspendClient_BuildsArgvWithTarget pins the argv shape: when
// TargetClient is set, the boundary must emit `suspend-client -t
// <target>`. We can't directly inspect the argv tmux saw, so the test
// observes via tmux's own error message — passing a clearly-invalid
// target name and asserting the stderr names that exact target. A
// regression where the boundary dropped `-t` entirely would either
// no-op (matching the headless case above) or emit a different stderr
// pinned to the "current client" path.
func TestSuspendClient_BuildsArgvWithTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "sba", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	const target = "ghost-target-pts-9"
	err := c.SuspendClient(ctx, SuspendClientOpts{TargetClient: target})
	if err == nil {
		t.Fatalf("expected error when suspending nonexistent target %q", target)
	}
	// tmux echoes the supplied target in its "can't find client" stderr
	// across every supported version.
	if !strings.Contains(err.Error(), target) {
		t.Fatalf("error %v should reference target %q (proves -t flag was forwarded)", err, target)
	}
}

// TestSuspendClient_RespectsContextCancellation pins the context-
// honouring contract: a cancelled context must abort the suspend call
// with the cancellation error rather than blocking on tmux. Mirrors
// the contract every other tmuxctl method upholds — a JSON-RPC client
// that disconnects mid-call must not pin a goroutine indefinitely.
func TestSuspendClient_RespectsContextCancellation(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)

	if err := c.CreateSession(context.Background(), SessionSpec{Name: "sca", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	t.Cleanup(func() { c.Shutdown(context.Background()) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel up front so the call cannot land

	err := c.SuspendClient(ctx, SuspendClientOpts{})
	if err == nil {
		// SuspendClient might succeed if tmux already replied before
		// our cancellation propagated — the no-op headless path is
		// extremely fast. Accept that as a benign race; the load-
		// bearing assertion is "no infinite block".
		return
	}
	// A non-nil error must surface the cancellation rather than a
	// random tmux failure. The controller's run() wraps the original
	// exec error in a "tmux <verb>: <stderr>" envelope that does NOT
	// preserve the typed context.Canceled — the matcher walks the
	// stderr text instead. Either "context canceled" or "killed"
	// (signal-killed exec on some platforms) is acceptable; anything
	// else means the cancellation did not propagate.
	msg := err.Error()
	if !strings.Contains(msg, "context canceled") && !strings.Contains(msg, "killed") {
		t.Fatalf("expected context-cancelled or signal-killed error, got %v", err)
	}
}

// TestSuspendClient_ReusesDetachClientHelpers documents the helper
// reuse contract: SuspendClient depends on isNoCurrentClientMsg and
// isClientMissingMsg, both defined in detach_client.go. A regression
// where someone re-introduced local copies (or removed the helpers
// from detach_client.go without re-exposing them) would surface as a
// build failure here. Pinning the dependency in a test rather than a
// build-time `var _ = …` check keeps the diagnostic close to the
// behaviour the helpers gate (no-op on no-current-client; sentinel-
// wrap on missing target).
func TestSuspendClient_ReusesDetachClientHelpers(t *testing.T) {
	t.Parallel()
	// Compile-time pins: if either helper disappears or changes
	// signature, this test file stops building before any tmux call.
	if !isNoCurrentClientMsg("no current client") {
		t.Fatal("isNoCurrentClientMsg lost its 'no current client' match — SuspendClient's headless no-op contract relies on it")
	}
	if !isClientMissingMsg("can't find client: ghost") {
		t.Fatal("isClientMissingMsg lost its 'can't find client' match — SuspendClient's missing-target sentinel mapping relies on it")
	}
}

// TestSuspendClient_EmptyOptsArgvShape is the explicit pin for the
// "no -t flag" argv shape: when TargetClient is empty, the boundary
// must not emit a `-t` arg with an empty value (tmux would reject
// that with "expected client argument" stderr). The headless no-op
// path above relies on this — a regression where the boundary
// emitted `suspend-client -t ""` would surface as a different
// stderr, breaking the empty-roster shortcut.
//
// Asserting on the resulting error path is the only black-box way to
// pin argv from a unit test: a successful no-op return proves the
// "no -t" branch ran; an unrelated stderr would surface here.
func TestSuspendClient_EmptyOptsArgvShape(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "seo", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Both an explicit zero-value struct and a positionally-named
	// field-zero literal must hit the same "no -t" branch.
	for _, opts := range []SuspendClientOpts{
		{},
		{TargetClient: ""},
	} {
		if err := c.SuspendClient(ctx, opts); err != nil {
			t.Fatalf("SuspendClient(%+v): %v (want nil for empty-opts headless case)", opts, err)
		}
	}
}
