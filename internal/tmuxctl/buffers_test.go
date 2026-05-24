package tmuxctl

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestSetBuffer_AutoNamePicksLatest exercises the auto-naming path
// end-to-end: write three buffers without -b and confirm the
// controller returns the most-recently-minted bufferN each time.
// tmux's own counter increments monotonically, so the resolved name
// must track that progression — buffer0 → buffer1 → buffer2.
func TestSetBuffer_AutoNamePicksLatest(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	// Anchor the tmux server with a real session so list-buffers
	// later in the test doesn't hit the "no server running" branch.
	if err := c.CreateSession(ctx, SessionSpec{
		Name: "sb_anchor", Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	want := []string{"buffer0", "buffer1", "buffer2"}
	for i, expectedName := range want {
		payload := "auto-" + expectedName
		got, err := c.SetBuffer(ctx, payload, "", false)
		if err != nil {
			t.Fatalf("SetBuffer #%d: %v", i, err)
		}
		if got != expectedName {
			t.Errorf("SetBuffer #%d resolved to %q, want %q", i, got, expectedName)
		}
	}
}

// TestSetBuffer_NamedReturnsSameName confirms the explicit-name path:
// pinning `-b NAME` simply echoes the same name back without consulting
// list-buffers (the most common path; the auto-name lookup only runs
// when the caller did not pin a name).
func TestSetBuffer_NamedReturnsSameName(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "sb_name_anchor", Command: "/bin/sh",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := c.SetBuffer(ctx, "the-payload", "pinned", false)
	if err != nil {
		t.Fatalf("SetBuffer: %v", err)
	}
	if got != "pinned" {
		t.Errorf("SetBuffer resolved to %q, want %q", got, "pinned")
	}
}

// TestSetBuffer_AppendConcatenates pins the `-a` flag end-to-end: a
// second SetBuffer with appendMode=true and the same name extends the
// existing buffer rather than replacing it. We verify the contents via
// tmux's own show-buffer (run() with the real tmux binary) so the
// test stays independent of the higher-level read tools that arrive
// in feat/buffer-tools (#98).
func TestSetBuffer_AppendConcatenates(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "sb_append_anchor", Command: "/bin/sh",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := c.SetBuffer(ctx, "head|", "appended", false); err != nil {
		t.Fatalf("SetBuffer head: %v", err)
	}
	if _, err := c.SetBuffer(ctx, "tail", "appended", true); err != nil {
		t.Fatalf("SetBuffer tail (append): %v", err)
	}

	out, err := c.run(ctx, "show-buffer", "-b", "appended")
	if err != nil {
		t.Fatalf("show-buffer: %v", err)
	}
	if out != "head|tail" {
		t.Errorf("appended buffer = %q, want %q", out, "head|tail")
	}
}

// TestSetBuffer_AppendCreatesMissing pins the documented behaviour
// that appending to a name tmux has never seen before silently
// creates the buffer — no error, the payload lands as a fresh
// buffer.
func TestSetBuffer_AppendCreatesMissing(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "sb_create_anchor", Command: "/bin/sh",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := c.SetBuffer(ctx, "first-write", "born_via_append", true)
	if err != nil {
		t.Fatalf("SetBuffer: %v", err)
	}
	if got != "born_via_append" {
		t.Errorf("resolved name = %q, want %q", got, "born_via_append")
	}
	out, err := c.run(ctx, "show-buffer", "-b", "born_via_append")
	if err != nil {
		t.Fatalf("show-buffer: %v", err)
	}
	if out != "first-write" {
		t.Errorf("show-buffer = %q, want %q", out, "first-write")
	}
}

// TestSetBuffer_BinaryPayload locks that we forward arbitrary bytes
// to tmux verbatim — including newlines and shell metachars — so an
// agent stashing structured text doesn't have to escape anything.
func TestSetBuffer_BinaryPayload(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "sb_bin_anchor", Command: "/bin/sh",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	const want = "line1\nline2\t$(echo nope)\n;rm -rf /"
	if _, err := c.SetBuffer(ctx, want, "binary_payload", false); err != nil {
		t.Fatalf("SetBuffer: %v", err)
	}
	out, err := c.run(ctx, "show-buffer", "-b", "binary_payload")
	if err != nil {
		t.Fatalf("show-buffer: %v", err)
	}
	if out != want {
		t.Errorf("show-buffer = %q, want %q", out, want)
	}
	// Sanity: rm did not actually run; the working directory still
	// contains a shred of state we expect (the tmux socket).
	if c.Socket() == "" || strings.Contains(c.Socket(), "rm") {
		t.Errorf("controller socket %q looks suspicious", c.Socket())
	}
}
