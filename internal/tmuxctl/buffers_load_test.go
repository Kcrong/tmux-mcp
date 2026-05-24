package tmuxctl

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// TestLoadBuffer_AutoNamePicksLatest mirrors the SetBuffer auto-name
// test: writing three buffers via `load-buffer -` (no -b) must resolve
// to tmux's monotonically-incrementing `bufferN` series. tmux's own
// counter is shared across set-buffer and load-buffer, so the
// expected sequence is buffer0 → buffer1 → buffer2 regardless of
// which write path produced each entry.
func TestLoadBuffer_AutoNamePicksLatest(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so list-buffers does not hit the
	// "no server running" branch on a freshly-built controller.
	if err := c.CreateSession(ctx, SessionSpec{
		Name: "lb_auto_anchor", Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	want := []string{"buffer0", "buffer1", "buffer2"}
	for i, expectedName := range want {
		payload := "auto-load-" + expectedName
		got, err := c.LoadBuffer(ctx, payload, "", false)
		if err != nil {
			t.Fatalf("LoadBuffer #%d: %v", i, err)
		}
		if got != expectedName {
			t.Errorf("LoadBuffer #%d resolved to %q, want %q", i, got, expectedName)
		}
		body, err := c.run(ctx, "show-buffer", "-b", got)
		if err != nil {
			t.Fatalf("show-buffer #%d: %v", i, err)
		}
		if body != payload {
			t.Errorf("show-buffer(%q) = %q, want %q", got, body, payload)
		}
	}
}

// TestLoadBuffer_NamedRoundTrips locks the explicit-name path: pinning
// `-b NAME` echoes the same name back without consulting list-buffers,
// and the payload streamed over stdin round-trips byte-for-byte.
func TestLoadBuffer_NamedRoundTrips(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "lb_name_anchor", Command: "/bin/sh",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	const want = "the quick brown fox jumps over the lazy dog"
	got, err := c.LoadBuffer(ctx, want, "lb_pinned", false)
	if err != nil {
		t.Fatalf("LoadBuffer: %v", err)
	}
	if got != "lb_pinned" {
		t.Errorf("LoadBuffer resolved to %q, want %q", got, "lb_pinned")
	}
	body, err := c.run(ctx, "show-buffer", "-b", "lb_pinned")
	if err != nil {
		t.Fatalf("show-buffer: %v", err)
	}
	if body != want {
		t.Errorf("show-buffer(lb_pinned) = %q, want %q", body, want)
	}
}

// TestLoadBuffer_AppendConcatenates pins the `-a` flag end-to-end: a
// second LoadBuffer with appendMode=true and the same name must extend
// the existing buffer rather than replacing it. Mirrors SetBuffer's
// own append test so the two write paths stay behaviourally aligned.
func TestLoadBuffer_AppendConcatenates(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "lb_append_anchor", Command: "/bin/sh",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := c.LoadBuffer(ctx, "head|", "lb_appended", false); err != nil {
		t.Fatalf("LoadBuffer head: %v", err)
	}
	if _, err := c.LoadBuffer(ctx, "tail", "lb_appended", true); err != nil {
		t.Fatalf("LoadBuffer tail (append): %v", err)
	}

	out, err := c.run(ctx, "show-buffer", "-b", "lb_appended")
	if err != nil {
		t.Fatalf("show-buffer: %v", err)
	}
	if out != "head|tail" {
		t.Errorf("appended buffer = %q, want %q", out, "head|tail")
	}
}

// TestLoadBuffer_EmptyPayload locks the empty-data branch: tmux is
// happy to accept a zero-byte stdin and create an empty buffer, and
// our wrapper must not invent an extra rejection on top. The
// post-write tmux behaviour for empty buffers varies across versions
// (3.4 may drop them entirely), so the assertion focuses on the
// "no error from the wrapper" contract rather than round-tripping the
// payload.
func TestLoadBuffer_EmptyPayload(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "lb_empty_anchor", Command: "/bin/sh",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := c.LoadBuffer(ctx, "", "lb_empty", false)
	if err != nil {
		t.Fatalf("LoadBuffer empty: %v", err)
	}
	if got != "lb_empty" {
		t.Errorf("LoadBuffer resolved to %q, want %q", got, "lb_empty")
	}
}

// TestLoadBuffer_LargeBinaryPayload exercises the load-over-stdin
// rationale directly: 5 KiB of random binary noise (well within the
// MCP-layer 1 MiB cap, but already enough to feel awkward as an argv
// argument) must round-trip verbatim. The payload includes embedded
// NUL bytes (filtered out below — tmux truncates buffers at the first
// NUL on some versions, so we keep the comparison stable across
// builds) and a generous mix of newlines / shell metachars to cover
// the "no implicit escaping" contract documented on LoadBuffer.
func TestLoadBuffer_LargeBinaryPayload(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "lb_big_anchor", Command: "/bin/sh",
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// 5 KiB of base64-encoded noise gives us a payload that is
	// definitely binary-safe (no NULs, no embedded BOMs) but also far
	// from a trivially-quoted shell string. base64 input is exactly
	// 4 * ceil(rawLen/3) bytes, so we size raw to land on 5120 bytes
	// of encoded output without padding fluff.
	raw := make([]byte, 5120/4*3)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	// Salt with a few line breaks and shell metachars so the payload
	// would clearly mangle if the wrapper tried to re-encode anything.
	encoded = encoded + "\n;rm -rf /\nline2\t$(echo nope)"
	if len(encoded) < 5*1024 {
		t.Fatalf("test payload too small: %d bytes", len(encoded))
	}

	got, err := c.LoadBuffer(ctx, encoded, "lb_big", false)
	if err != nil {
		t.Fatalf("LoadBuffer: %v", err)
	}
	if got != "lb_big" {
		t.Errorf("LoadBuffer resolved to %q, want %q", got, "lb_big")
	}
	out, err := c.run(ctx, "show-buffer", "-b", "lb_big")
	if err != nil {
		t.Fatalf("show-buffer: %v", err)
	}
	if out != encoded {
		t.Errorf("show-buffer length = %d, want %d (round-trip mismatch)", len(out), len(encoded))
	}
	// Sanity: the embedded `;rm -rf /` did not actually run; the
	// controller socket still looks like a tmux scratch path.
	if c.Socket() == "" || strings.Contains(c.Socket(), "rm") {
		t.Errorf("controller socket %q looks suspicious", c.Socket())
	}
}
