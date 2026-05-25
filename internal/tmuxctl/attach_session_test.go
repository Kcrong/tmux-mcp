package tmuxctl

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestAttachSession_RejectsEmptyTarget pins the up-front guard: without
// a target_session the controller refuses the call rather than letting
// tmux fall back to "the most recently used session", which has no
// stable interpretation on the headless servers tmux-mcp owns. The
// boundary already enforces this, but the controller defends here too
// for tests / direct call sites that bypass the JSON-RPC layer.
func TestAttachSession_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.AttachSession(ctx, AttachSessionOpts{DetachOthers: true})
	if err == nil {
		t.Fatal("expected error for empty target_session, got nil")
	}
	// Pinning on the substring rather than the full string keeps the
	// test resilient to a future copyedit; the semantic guarantee is
	// "the message names target_session as the missing input".
	if got := err.Error(); !contains(got, "target_session required") {
		t.Fatalf("error %q does not mention target_session required", got)
	}
}

// TestAttachSession_NoDetachReturnsTTYSentinel pins the load-bearing
// headless contract: a no-detach attach call refuses up front with the
// typed ErrAttachRequiresTTY sentinel so the JSON-RPC layer can map it
// onto CodeInvalidParams. Without this, callers would get a confusing
// internal error pointing at tmux when in fact the failure is "you
// asked for an interactive attach from a context that has no TTY".
func TestAttachSession_NoDetachReturnsTTYSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "as_tty", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.AttachSession(ctx, AttachSessionOpts{TargetSession: "as_tty"})
	if err == nil {
		t.Fatal("expected ErrAttachRequiresTTY for a no-detach attach, got nil")
	}
	if !errors.Is(err, ErrAttachRequiresTTY) {
		t.Fatalf("error %v does not wrap ErrAttachRequiresTTY", err)
	}
	if !IsAttachRequiresTTYErr(err) {
		t.Fatalf("IsAttachRequiresTTYErr(%v) = false, want true", err)
	}
}

// TestAttachSession_DetachOthersHeadlessNoOp drives the meaningful
// headless interpretation of `-d`: with the target session present and
// no clients attached (the common case for tmux-mcp's own daemons),
// AttachSession with DetachOthers=true must succeed as a clean no-op.
// Internally this funnels through DetachClient, which already swallows
// the "no current client" stderr — so the contract is "calling this on
// an empty roster is fine".
func TestAttachSession_DetachOthersHeadlessNoOp(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "as_detach", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.AttachSession(ctx, AttachSessionOpts{
		TargetSession: "as_detach",
		DetachOthers:  true,
	}); err != nil {
		t.Fatalf("AttachSession (DetachOthers=true): %v", err)
	}
}

// TestAttachSession_DetachOthersIncludingSelfHeadlessNoOp covers the
// `-D` shape symmetrically. tmux 3.4 and earlier reject `-D` itself,
// but our headless interpretation routes through DetachClient (which
// uses `-s` not `-D`) so the call must succeed regardless of the
// operator's tmux version.
func TestAttachSession_DetachOthersIncludingSelfHeadlessNoOp(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "as_detach_d", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.AttachSession(ctx, AttachSessionOpts{
		TargetSession:             "as_detach_d",
		DetachOthersIncludingSelf: true,
	}); err != nil {
		t.Fatalf("AttachSession (DetachOthersIncludingSelf=true): %v", err)
	}
}

// TestAttachSession_MissingSessionWrapsSentinel pins the typed-error
// contract for a non-existent target: the controller pre-flights
// has-session, which already maps "can't find session: <name>" onto
// errs.ErrSessionNotFound through run()'s isSessionMissingMsg detector.
// The dispatcher relies on that wrapping to surface a clean -32000 to
// the JSON-RPC client.
func TestAttachSession_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise the "server up,
	// session missing" branch (a brand-new socket reports the
	// different "no server running" stderr).
	if err := c.CreateSession(ctx, SessionSpec{Name: "as_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.AttachSession(ctx, AttachSessionOpts{
		TargetSession: "ghost_session_xyzzy",
		DetachOthers:  true,
	})
	if err == nil {
		t.Fatal("expected error for missing session, got nil")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestAttachSession_ForwardCompatFlagsAreInert exercises the
// forward-compat fields (ReadOnly, WorkingDirectory,
// SkipEnvironmentUpdate, Flags, NoEnvironmentApply) under the headless
// detach path. They are accepted for shape but currently ignored on
// the wire — the test asserts that they don't change the contract:
// passing them alongside DetachOthers=true must still succeed as a
// clean no-op.
func TestAttachSession_ForwardCompatFlagsAreInert(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "as_fwd", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.AttachSession(ctx, AttachSessionOpts{
		TargetSession:         "as_fwd",
		DetachOthers:          true,
		ReadOnly:              true,
		WorkingDirectory:      "/tmp",
		SkipEnvironmentUpdate: true,
		Flags:                 "active-pane,read-only",
		NoEnvironmentApply:    true,
	}); err != nil {
		t.Fatalf("AttachSession (with all forward-compat flags): %v", err)
	}
}

// contains is a thin wrapper around strings.Contains used by this
// suite so the import surface stays small. We deliberately avoid
// importing strings at the package level for a single substring check
// and instead colocate the helper here — it makes a future refactor
// (e.g. switching to a typed sentinel match) a one-line change.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
