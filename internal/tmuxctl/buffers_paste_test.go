package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// pastePrepareSession spins up a controller-anchored session with a
// long-running shell and waits for the prompt so the upcoming
// paste-buffer call has somewhere to land. The shell is configured
// with a stripped-down PS1 to make the prompt-detection loop
// deterministic across distros that ship colourful default prompts.
//
// Each caller must invoke t.Parallel() itself — t.Helper does not
// propagate the parallel flag, and the package-level concurrency
// contract is "one tmux server per top-level test".
func pastePrepareSession(t *testing.T, name string) (
	c *Controller,
	target string,
	ctx context.Context,
) {
	t.Helper()
	skipIfNoTmux(t)
	c = newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name:    name,
		Command: "/bin/sh",
		Width:   80,
		Height:  24,
		Env:     map[string]string{"PS1": "$ "},
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	target = name + ":0.0"
	return c, target, ctx
}

// waitForCapture polls capture-pane against target until the pane's
// visible region contains marker, or the deadline expires. The test
// asserts on capture output rather than on tmux's exit code from
// paste-buffer because paste-buffer's success is observable as
// "the bytes hit the pty"; an exit code of 0 by itself does not
// prove the buffer was non-empty or that tmux routed it to the
// right pane.
func waitForCapture(t *testing.T, c *Controller, ctx context.Context, target, marker string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, err := c.run(ctx, "capture-pane", "-p", "-t", target)
		if err != nil {
			t.Fatalf("capture-pane %s: %v", target, err)
		}
		last = out
		if strings.Contains(out, marker) {
			return out
		}
		time.Sleep(75 * time.Millisecond)
	}
	t.Fatalf("marker %q never appeared in %s; last capture:\n%s", marker, target, last)
	return last
}

// TestPasteBuffer_NamedDeliversBuffer drives the explicit-name happy
// path: stash a sentinel under a known buffer name, paste it into the
// session's active pane, and confirm the bytes show up in the visible
// region. We deliberately do not pass deleteAfter — that branch has
// its own focused test below — so this case pins the "bytes land in
// the pane" behaviour in isolation.
func TestPasteBuffer_NamedDeliversBuffer(t *testing.T) {
	t.Parallel()
	c, target, ctx := pastePrepareSession(t, "pb_named")

	const sentinel = "PASTE_BUFFER_NAMED_MARK"
	if _, err := c.run(ctx, "set-buffer", "-b", "named_paste", sentinel); err != nil {
		t.Fatalf("set-buffer named_paste: %v", err)
	}

	if err := c.PasteBuffer(ctx, target, "named_paste", false, false); err != nil {
		t.Fatalf("PasteBuffer: %v", err)
	}
	waitForCapture(t, c, ctx, target, sentinel)

	// Buffer must still exist (deleteAfter=false) so a follow-up paste
	// could hit the same name. list-buffers is the cheapest probe.
	listing, err := c.run(ctx, "list-buffers", "-F", "#{buffer_name}")
	if err != nil {
		t.Fatalf("list-buffers: %v", err)
	}
	if !strings.Contains(listing, "named_paste") {
		t.Fatalf("buffer named_paste was unexpectedly gone after paste; listing=%q", listing)
	}
}

// TestPasteBuffer_DefaultPicksMostRecent locks the empty-name path:
// when the caller does not pin `name`, tmux pastes the
// most-recently-added buffer (the bare `paste-buffer` CLI default).
// Two buffers are seeded so the assertion catches a regression that
// silently switched to "first" or "alphabetical" ordering.
func TestPasteBuffer_DefaultPicksMostRecent(t *testing.T) {
	t.Parallel()
	c, target, ctx := pastePrepareSession(t, "pb_default")

	// Seed two buffers; the second one becomes "most recent" and is
	// what an empty-name paste must deliver.
	if _, err := c.run(ctx, "set-buffer", "PASTE_BUFFER_OLDEST_MARK"); err != nil {
		t.Fatalf("set-buffer oldest: %v", err)
	}
	if _, err := c.run(ctx, "set-buffer", "PASTE_BUFFER_NEWEST_MARK"); err != nil {
		t.Fatalf("set-buffer newest: %v", err)
	}

	if err := c.PasteBuffer(ctx, target, "", false, false); err != nil {
		t.Fatalf("PasteBuffer (default): %v", err)
	}
	body := waitForCapture(t, c, ctx, target, "PASTE_BUFFER_NEWEST_MARK")
	if strings.Contains(body, "PASTE_BUFFER_OLDEST_MARK") {
		t.Fatalf("default paste delivered the wrong buffer; body=\n%s", body)
	}
}

// TestPasteBuffer_DeleteAfterRemovesBuffer pins the `-d` round-trip:
// after a paste with deleteAfter=true the buffer is gone from
// list-buffers, matching tmux's documented "delete the buffer after
// pasting" semantic. This is the load-bearing flag for an agent that
// stages a one-shot snippet and does not want it to leak into
// subsequent list_buffers responses.
func TestPasteBuffer_DeleteAfterRemovesBuffer(t *testing.T) {
	t.Parallel()
	c, target, ctx := pastePrepareSession(t, "pb_delete")

	const sentinel = "PASTE_BUFFER_DELETE_MARK"
	if _, err := c.run(ctx, "set-buffer", "-b", "ephemeral", sentinel); err != nil {
		t.Fatalf("set-buffer ephemeral: %v", err)
	}
	// Sanity: the buffer is present before we paste.
	pre, preErr := c.run(ctx, "list-buffers", "-F", "#{buffer_name}")
	if preErr != nil {
		t.Fatalf("list-buffers pre: %v", preErr)
	}
	if !strings.Contains(pre, "ephemeral") {
		t.Fatalf("buffer ephemeral missing before paste; listing=%q", pre)
	}

	if err := c.PasteBuffer(ctx, target, "ephemeral", true, false); err != nil {
		t.Fatalf("PasteBuffer (deleteAfter): %v", err)
	}
	waitForCapture(t, c, ctx, target, sentinel)

	post, err := c.run(ctx, "list-buffers", "-F", "#{buffer_name}")
	if err != nil {
		t.Fatalf("list-buffers post: %v", err)
	}
	if strings.Contains(post, "ephemeral") {
		t.Fatalf("deleteAfter=true did not drop the buffer; listing=%q", post)
	}
}

// TestPasteBuffer_MissingWrapsSentinel pins the typed-error contract
// for "no such buffer": pasting from a name tmux has never seen must
// surface a wrapped errs.ErrSessionNotFound so the JSON-RPC layer can
// emit CodeSessionNotFound (-32000) regardless of which exact phrase
// the local tmux version emitted.
func TestPasteBuffer_MissingWrapsSentinel(t *testing.T) {
	t.Parallel()
	c, target, ctx := pastePrepareSession(t, "pb_missing")

	err := c.PasteBuffer(ctx, target, "ghost_paste_buffer_nonexistent", false, false)
	if err == nil {
		t.Fatal("expected error pasting from a missing buffer")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestPasteBuffer_RejectsEmptyTarget locks the up-front guard. tmux
// would otherwise resolve "" against the current target, which is
// rarely what an MCP caller actually wants — we reject the malformed
// call before any tmux command is dispatched.
func TestPasteBuffer_RejectsEmptyTarget(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.PasteBuffer(ctx, "", "any", false, false)
	if err == nil {
		t.Fatal("expected error for empty target")
	}
	if !strings.Contains(err.Error(), "target required") {
		t.Fatalf("unexpected error: %v", err)
	}
}
