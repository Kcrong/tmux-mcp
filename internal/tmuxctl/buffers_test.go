package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
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
// test stays independent of the higher-level read tools.
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

// TestListBuffers_EmptyOnFreshController locks the contract that
// `tmux list-buffers` against a controller whose tmux server has not
// even spawned yet returns an empty slice (and a nil error), not a
// fatal "no server running" leak. ListSessions enforces the same
// guarantee so callers iterating ListBuffers can skip a redundant
// "is the server up yet" check.
func TestListBuffers_EmptyOnFreshController(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	bufs, err := c.ListBuffers(ctx)
	if err != nil {
		t.Fatalf("ListBuffers: %v", err)
	}
	if len(bufs) != 0 {
		t.Fatalf("expected no buffers on fresh controller, got %v", bufs)
	}
}

// TestListBuffers_HappyPath drives the populated-server case end to
// end: anchor the tmux server with a session, stash two buffers via
// `tmux set-buffer`, then assert ListBuffers returns the names tmux
// assigned (buffer0/buffer1 + the explicit "pinned" name) with the
// right sizes and a sane created_at.
func TestListBuffers_HappyPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// Anchor with a session so the tmux server is definitely up; buffers
	// live on the server, not on a session, but the server has to exist.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Seed two buffers: one auto-named (becomes "buffer0") and one
	// pinned to a known name ("pinned").
	if _, err := c.run(ctx, "set-buffer", "hello"); err != nil {
		t.Fatalf("set-buffer hello: %v", err)
	}
	beforePinned := time.Now().Unix()
	if _, err := c.run(ctx, "set-buffer", "-b", "pinned", "world"); err != nil {
		t.Fatalf("set-buffer pinned: %v", err)
	}
	afterPinned := time.Now().Unix() + 1

	bufs, err := c.ListBuffers(ctx)
	if err != nil {
		t.Fatalf("ListBuffers: %v", err)
	}
	if len(bufs) != 2 {
		t.Fatalf("expected 2 buffers, got %d (%v)", len(bufs), bufs)
	}

	// Index by name so we don't depend on tmux's listing order, which
	// has changed across versions.
	byName := make(map[string]BufferInfo, len(bufs))
	for _, b := range bufs {
		byName[b.Name] = b
	}
	auto, ok := byName["buffer0"]
	if !ok {
		t.Fatalf("expected auto-named buffer0, got %v", bufs)
	}
	if auto.Size != len("hello") {
		t.Errorf("buffer0 size = %d, want %d", auto.Size, len("hello"))
	}
	pinned, ok := byName["pinned"]
	if !ok {
		t.Fatalf("expected pinned buffer, got %v", bufs)
	}
	if pinned.Size != len("world") {
		t.Errorf("pinned size = %d, want %d", pinned.Size, len("world"))
	}
	pinnedUnix := pinned.CreatedAt.Unix()
	if pinnedUnix < beforePinned || pinnedUnix > afterPinned {
		t.Errorf("pinned created_at unix = %d, want in [%d..%d]", pinnedUnix, beforePinned, afterPinned)
	}
	if pinned.CreatedAt.Location() != time.UTC {
		t.Errorf("pinned created_at = %s, want UTC", pinned.CreatedAt)
	}
}

// TestShowBuffer_DefaultDumpsMostRecent confirms ShowBuffer with an
// empty name resolves to `tmux show-buffer` (no -b), which dumps the
// most-recently-added buffer. Agents that just called set-buffer can
// rely on this — they don't have to round-trip through list-buffers
// to learn the assigned name.
func TestShowBuffer_DefaultDumpsMostRecent(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.run(ctx, "set-buffer", "first"); err != nil {
		t.Fatalf("set-buffer first: %v", err)
	}
	if _, err := c.run(ctx, "set-buffer", "second"); err != nil {
		t.Fatalf("set-buffer second: %v", err)
	}

	body, err := c.ShowBuffer(ctx, "")
	if err != nil {
		t.Fatalf("ShowBuffer: %v", err)
	}
	// tmux returns the buffer body verbatim — no trailing newline.
	if body != "second" {
		t.Errorf("ShowBuffer default = %q, want %q", body, "second")
	}
}

// TestShowBuffer_NamedReturnsSpecific exercises the -b path: pin a
// buffer to a known name, dump it by name, and confirm the contents
// round-trip exactly.
func TestShowBuffer_NamedReturnsSpecific(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	const want = "the quick brown fox"
	if _, err := c.run(ctx, "set-buffer", "-b", "named", want); err != nil {
		t.Fatalf("set-buffer named: %v", err)
	}
	// Add a second buffer so the "default → most-recent" path would
	// pick a different one — proves -b is honoured.
	if _, err := c.run(ctx, "set-buffer", "decoy"); err != nil {
		t.Fatalf("set-buffer decoy: %v", err)
	}

	got, err := c.ShowBuffer(ctx, "named")
	if err != nil {
		t.Fatalf("ShowBuffer(named): %v", err)
	}
	if got != want {
		t.Errorf("ShowBuffer(named) = %q, want %q", got, want)
	}
}

// TestShowBuffer_MissingWrapsSentinel pins the typed-error contract
// for "no such buffer": callers (and the JSON-RPC layer) must be able
// to errors.Is into errs.ErrSessionNotFound regardless of which exact
// phrase tmux emitted ("no buffer X" vs the older "unknown buffer"
// message).
func TestShowBuffer_MissingWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a session so we exercise "server up, buffer missing"
	// rather than "no server" (different stderr shape).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	_, err := c.ShowBuffer(ctx, "ghost_buffer_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing buffer")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}
