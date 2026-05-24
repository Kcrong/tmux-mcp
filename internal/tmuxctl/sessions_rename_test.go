package tmuxctl

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestRenameSession_HappyPath drives the controller end-to-end: create a
// session, rename it, then assert the old name has vanished from the
// listing while the new one is present. Mirrors the structure of
// TestSessionLifecycle so the rename path stays observable from the
// same vantage point operators inspect when debugging session state.
func TestRenameSession_HappyPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const oldName = "rename_old"
	const newName = "rename_new"
	if err := c.CreateSession(ctx, SessionSpec{Name: oldName, Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.RenameSession(ctx, oldName, newName); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}

	names, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	hasOld := false
	hasNew := false
	for _, n := range names {
		switch n {
		case oldName:
			hasOld = true
		case newName:
			hasNew = true
		}
	}
	if hasOld {
		t.Fatalf("old name %q still listed after rename: %v", oldName, names)
	}
	if !hasNew {
		t.Fatalf("new name %q missing after rename: %v", newName, names)
	}

	// describe should now succeed against the new name and fail against
	// the old one — the canonical check that follow-up tools see the
	// rename.
	if _, derr := c.DescribeSession(ctx, newName); derr != nil {
		t.Fatalf("DescribeSession(%q) after rename: %v", newName, derr)
	}
	if _, derr := c.DescribeSession(ctx, oldName); derr == nil {
		t.Fatalf("DescribeSession(%q) should fail after rename", oldName)
	} else if !errors.Is(derr, errs.ErrSessionNotFound) {
		t.Fatalf("DescribeSession(%q) error %v does not wrap ErrSessionNotFound", oldName, derr)
	}
}

// TestRenameSession_UnknownOldWrapsSentinel pins the contract relied on
// by the JSON-RPC layer: an unknown source session surfaces as a
// wrapped errs.ErrSessionNotFound so the dispatcher can map it to
// CodeSessionNotFound.
func TestRenameSession_UnknownOldWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so we exercise the
	// "server up, named session missing" branch (a fresh controller
	// has no socket and produces a different error message).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession anchor: %v", err)
	}

	err := c.RenameSession(ctx, "definitely_does_not_exist_xyzzy", "newname")
	if err == nil {
		t.Fatal("expected error renaming missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestRenameSession_DuplicateNewWrapsSentinel covers the collision path:
// renaming a session to a name that is already in use must wrap
// errs.ErrSessionExists so the dispatcher can emit CodeSessionExists.
func TestRenameSession_DuplicateNewWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "dup_a", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession dup_a: %v", err)
	}
	if err := c.CreateSession(ctx, SessionSpec{Name: "dup_b", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession dup_b: %v", err)
	}

	err := c.RenameSession(ctx, "dup_a", "dup_b")
	if err == nil {
		t.Fatal("expected error renaming to an existing session name")
	}
	if !errors.Is(err, errs.ErrSessionExists) {
		t.Fatalf("error %v does not wrap errs.ErrSessionExists", err)
	}
	// Should not also wrap ErrSessionNotFound — that would map the
	// failure to CodeSessionNotFound (-32000) on the wire instead of the
	// dedicated CodeSessionExists (-32004).
	if errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v should not wrap ErrSessionNotFound (sentinels collide)", err)
	}
}

// TestRenameSession_EmptyOldNameRejected guards the cheap input check the
// method performs before shelling out to tmux.
func TestRenameSession_EmptyOldNameRejected(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.RenameSession(ctx, "", "any"); err == nil {
		t.Fatal("expected error for empty old session name")
	}
}

// TestRenameSession_EmptyNewNameRejected guards the same up-front check
// for the destination name. tmux itself rejects an empty new name with
// "invalid session:", but defending here keeps the controller usable
// from tests that bypass the boundary validator.
func TestRenameSession_EmptyNewNameRejected(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.RenameSession(ctx, "any", ""); err == nil {
		t.Fatal("expected error for empty new session name")
	}
}
