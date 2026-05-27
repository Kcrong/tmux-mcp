package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestSaveBuffer_DefaultDumpsMostRecent confirms SaveBuffer with an
// empty name resolves to `tmux save-buffer -` (no -b), which dumps
// the most-recently-added buffer to stdout. Mirrors the
// ShowBuffer default contract — agents that just called set-buffer
// can rely on this without round-tripping through list-buffers.
func TestSaveBuffer_DefaultDumpsMostRecent(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "save_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := c.run(ctx, "set-buffer", "first"); err != nil {
		t.Fatalf("set-buffer first: %v", err)
	}
	if _, err := c.run(ctx, "set-buffer", "second"); err != nil {
		t.Fatalf("set-buffer second: %v", err)
	}

	body, err := c.SaveBuffer(ctx, "")
	if err != nil {
		t.Fatalf("SaveBuffer: %v", err)
	}
	// tmux returns the buffer body verbatim — no trailing newline,
	// matching the show-buffer contract.
	if body != "second" {
		t.Errorf("SaveBuffer default = %q, want %q", body, "second")
	}
}

// TestSaveBuffer_NamedReturnsSpecific exercises the -b path: pin a
// buffer to a known name, dump it by name, and confirm the contents
// round-trip exactly. A decoy buffer is added so the
// "default → most-recent" path would pick a different one — proves
// -b is honoured.
func TestSaveBuffer_NamedReturnsSpecific(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "save_named_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	const want = "the quick brown fox"
	if _, err := c.run(ctx, "set-buffer", "-b", "named_save", want); err != nil {
		t.Fatalf("set-buffer named_save: %v", err)
	}
	if _, err := c.run(ctx, "set-buffer", "decoy"); err != nil {
		t.Fatalf("set-buffer decoy: %v", err)
	}

	got, err := c.SaveBuffer(ctx, "named_save")
	if err != nil {
		t.Fatalf("SaveBuffer(named_save): %v", err)
	}
	if got != want {
		t.Errorf("SaveBuffer(named_save) = %q, want %q", got, want)
	}
}

// TestSaveBuffer_MissingWrapsSentinel pins the typed-error contract
// for "no such buffer": callers (and the JSON-RPC layer) must be able
// to errors.Is into errs.ErrSessionNotFound regardless of which exact
// phrase tmux emitted ("no buffer X" vs the older "unknown buffer"
// message). Mirrors the ShowBuffer guarantee so a client switching
// between the two read paths sees a stable wire code.
func TestSaveBuffer_MissingWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a session so we exercise "server up, buffer missing"
	// rather than "no server" (different stderr shape).
	if err := c.CreateSession(ctx, SessionSpec{Name: "save_missing_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	_, err := c.SaveBuffer(ctx, "ghost_save_buffer_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing buffer")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSaveBuffer_LargePayloadSurvivesPipe pins that bodies that
// straddle a single pipe-read round-trip intact through
// `tmux save-buffer -`. exec's stdout reader drains the underlying
// pipe via [bytes.Buffer], which auto-grows, but the test still
// proves we don't regress to a half-read on a multi-page payload.
//
// The payload is loaded via `load-buffer -b NAME -` so the seed
// step does not push a multi-KiB blob through tmux's argv (which
// hits "argument list too long" well before the pipe-read path
// matters). Using load-buffer is also closer to how a real agent
// would seed a chunk this size — set-buffer is convenient for
// short snippets but breaks on anything that approaches the
// kernel's argv limit.
func TestSaveBuffer_LargePayloadSurvivesPipe(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "save_big_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// 256 KiB is comfortably past Linux's default 64 KiB pipe buffer
	// and macOS's 16 KiB default, so a faulty single-read
	// implementation on the save-buffer path would show up as a
	// truncated body rather than a flake.
	const payloadSize = 256 * 1024
	payload := strings.Repeat("abcdefgh", payloadSize/8)

	if err := loadBufferStdin(t, c, "big_save", payload); err != nil {
		t.Fatalf("load-buffer seed: %v", err)
	}

	got, err := c.SaveBuffer(ctx, "big_save")
	if err != nil {
		t.Fatalf("SaveBuffer(big_save): %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("SaveBuffer(big_save) returned %d bytes, want %d", len(got), len(payload))
	}
	if got != payload {
		t.Fatal("SaveBuffer(big_save) payload differs from input")
	}
}

// loadBufferStdin seeds a tmux paste buffer by piping `data` into
// `tmux load-buffer -b NAME -`. The controller's surface intentionally
// does not expose load-buffer (production tooling has no need), so
// this is a test-only fixture for cases where the payload is too
// large for set-buffer's argv-bound interface.
func loadBufferStdin(t *testing.T, c *Controller, name, data string) error {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), c.bin, "-S", c.socket,
		"load-buffer", "-b", name, "-")
	cmd.Stdin = strings.NewReader(data)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("load-buffer %q: %w (stderr=%s)", name, err, stderr.String())
	}
	return nil
}
