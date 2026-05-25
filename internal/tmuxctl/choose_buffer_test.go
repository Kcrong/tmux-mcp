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

// TestChooseBuffer_HappyPath_PutsPaneInBufferMode is the load-bearing
// contract every agent reaching for ChooseBuffer relies on: the call
// puts the target pane into tmux's buffer-mode picker. We verify that
// post-call by reading two `display-message` format variables —
// `#{?pane_in_mode,1,0}` flips to `1` once the pane has entered any
// mode, and `#{pane_mode}` resolves to `buffer-mode` so a regression
// that lands the pane in copy-mode (or any other picker) shows up
// here loud and clear.
//
// We seed the server with a real buffer first so tmux has at least
// one row to render in the chooser. Without a buffer some tmux
// versions still enter the mode but display "no buffers"; including
// the buffer keeps the test robust across versions.
func TestChooseBuffer_HappyPath_PutsPaneInBufferMode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "cb", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.SetBuffer(ctx, "hello world", "", false); err != nil {
		t.Fatalf("SetBuffer: %v", err)
	}

	if err := c.ChooseBuffer(ctx, "cb", "", "", "", "", "", false, false, false); err != nil {
		t.Fatalf("ChooseBuffer: %v", err)
	}

	got, err := c.DisplayMessage(ctx, "#{?pane_in_mode,1,0}", "cb", "", "")
	if err != nil {
		t.Fatalf("DisplayMessage(pane_in_mode): %v", err)
	}
	if got != "1" {
		t.Fatalf("pane_in_mode = %q, want 1 — choose-buffer must enter the pane into a mode", got)
	}
	mode, err := c.DisplayMessage(ctx, "#{pane_mode}", "cb", "", "")
	if err != nil {
		t.Fatalf("DisplayMessage(pane_mode): %v", err)
	}
	if !strings.Contains(mode, "buffer-mode") {
		t.Fatalf("pane_mode = %q, want it to mention buffer-mode", mode)
	}
}

// TestChooseBuffer_ForwardsFlags exercises the argv builder: passing
// every flag at once must succeed end-to-end against a real tmux
// server. We don't introspect the chooser's contents (tmux doesn't
// expose them via display-message), but a clean exit confirms the
// argv shape is acceptable to tmux 3.4 and that none of the optional
// strings gets quoted incorrectly.
func TestChooseBuffer_ForwardsFlags(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "cbf", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.SetBuffer(ctx, "snippet-A", "", false); err != nil {
		t.Fatalf("SetBuffer A: %v", err)
	}
	if _, err := c.SetBuffer(ctx, "snippet-B", "", false); err != nil {
		t.Fatalf("SetBuffer B: %v", err)
	}

	err := c.ChooseBuffer(ctx,
		"cbf",                          // target
		"#{buffer_name}",               // format
		"#{>=:#{buffer_size},1}",       // filter — accept all rows
		"Q",                            // key-format
		"time",                         // sort-order
		"display-message 'picked: %%'", // template
		true,                           // no-preview
		true,                           // zoom
		true,                           // reverse
	)
	if err != nil {
		t.Fatalf("ChooseBuffer with full flag set: %v", err)
	}
	mode, err := c.DisplayMessage(ctx, "#{pane_mode}", "cbf", "", "")
	if err != nil {
		t.Fatalf("DisplayMessage(pane_mode): %v", err)
	}
	if !strings.Contains(mode, "buffer-mode") {
		t.Fatalf("pane_mode = %q, want buffer-mode after full-flag call", mode)
	}
}

// TestChooseBuffer_MissingTargetWrapsSentinel pins the typed-error
// contract for an unknown target: callers (and the JSON-RPC layer)
// must be able to errors.Is into errs.ErrSessionNotFound regardless
// of which exact phrase tmux emitted ("can't find pane",
// "no current target", ...).
func TestChooseBuffer_MissingTargetWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, pane
	// missing" rather than "no server" (different stderr shape, both
	// covered: this test pins the former, the headless test below
	// pins the latter).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	err := c.ChooseBuffer(ctx,
		"ghost_session_nonexistent", // target
		"", "", "", "", "",          // format / filter / keyFormat / sortOrder / template
		false, false, false,
	)
	if err == nil {
		t.Fatal("expected error for missing target pane")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestChooseBuffer_HeadlessWrapsSentinel pins the second flavour of
// "missing surface": when no tmux server is running on the
// controller's socket at all, choose-buffer can't enter any pane
// because there is no pane to enter. tmux phrases this as either
// "no server running on ..." or "error connecting to ..." depending
// on whether the socket file exists; both must wrap
// errs.ErrSessionNotFound so the JSON-RPC layer can map every
// "no target available" surface to CodeSessionNotFound uniformly.
//
// We construct a fresh controller backed by an explicit socket path
// that we never touch, so tmux's first call against it is guaranteed
// to fail with a connection-style error rather than a pane-resolution
// error. The empty target also exercises the "no -t supplied" branch
// of ChooseBuffer's argv builder, which is the surface most likely
// to leak this case to a caller.
func TestChooseBuffer_HeadlessWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	dir := t.TempDir()
	socket := filepath.Join(dir, "headless.sock")
	c, err := NewWithSocket(socket)
	if err != nil {
		t.Fatalf("NewWithSocket: %v", err)
	}
	t.Cleanup(func() { c.Shutdown(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	cbErr := c.ChooseBuffer(ctx,
		"",                 // target — exercise the no-`-t` branch.
		"", "", "", "", "", // format / filter / keyFormat / sortOrder / template
		false, false, false,
	)
	if cbErr == nil {
		t.Fatal("expected error against a headless tmux server")
	}
	if !errors.Is(cbErr, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", cbErr)
	}
}
