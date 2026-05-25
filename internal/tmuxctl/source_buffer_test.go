package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// skipIfNoSourceBuffer skips the test when the tmux on PATH does not
// implement the `source-buffer` command. tmux added source-buffer in
// 3.5; older releases (the 3.4 still shipping in Ubuntu 24.04 / Debian
// stable as of 2026) emit "unknown command: source-buffer" instead.
// Probing via list-commands keeps the gate independent of any future
// rename of the command — the helper is asking "does my tmux know about
// this verb?" rather than "is the version >= 3.5?".
//
// Callers must invoke skipIfNoTmux first; this helper assumes a tmux
// binary is on PATH and only sniffs whether it carries the verb.
func skipIfNoSourceBuffer(t *testing.T, c *Controller) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := c.run(ctx, "list-commands")
	if err != nil {
		t.Fatalf("list-commands probe: %v", err)
	}
	if !strings.Contains(out, "source-buffer") {
		t.Skipf("tmux on PATH does not implement source-buffer (added in 3.5)")
	}
}

// TestSourceBuffer_NamedAppliesCommands drives the explicit-name happy
// path end-to-end: stash a tmux command line in a named paste buffer,
// run SourceBuffer with that name, and confirm the option the body sets
// (status-keys → vi) actually lands in tmux's options table. This is
// the load-bearing case for the "stage config without touching disk"
// agent workflow.
func TestSourceBuffer_NamedAppliesCommands(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the tmux server is definitely up;
	// buffers (and the source-buffer verb itself) require a running
	// daemon.
	if err := c.CreateSession(ctx, SessionSpec{
		Name: "src_named", Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	skipIfNoSourceBuffer(t, c)

	// Pre-flight: status-keys defaults to "emacs" on a fresh server, so
	// any later "vi" reading proves source-buffer ran the body.
	if _, err := c.SetBuffer(ctx, "set -g status-keys vi", "cfg_named", false); err != nil {
		t.Fatalf("SetBuffer: %v", err)
	}
	if err := c.SourceBuffer(ctx, "cfg_named"); err != nil {
		t.Fatalf("SourceBuffer: %v", err)
	}
	options, err := c.ShowOptions(ctx, OptionScopeServer, "", "", false)
	if err != nil {
		t.Fatalf("ShowOptions: %v", err)
	}
	if got := options["status-keys"]; got != "vi" {
		t.Errorf("status-keys = %q, want %q (source-buffer body did not apply)", got, "vi")
	}
}

// TestSourceBuffer_DefaultPicksMostRecent pins the empty-name path:
// passing "" must pick the most-recently-added buffer (mirroring tmux's
// CLI default). We seed two buffers and confirm the second one's body
// is the one tmux executes.
func TestSourceBuffer_DefaultPicksMostRecent(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{
		Name: "src_default", Command: "/bin/sh", Width: 80, Height: 24,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	skipIfNoSourceBuffer(t, c)

	// Seed an older buffer with a body that would pick a different
	// value, then a newer buffer with the value we expect to win.
	if _, err := c.SetBuffer(ctx, "set -g status-keys emacs", "older", false); err != nil {
		t.Fatalf("SetBuffer older: %v", err)
	}
	if _, err := c.SetBuffer(ctx, "set -g status-keys vi", "newer", false); err != nil {
		t.Fatalf("SetBuffer newer: %v", err)
	}
	if err := c.SourceBuffer(ctx, ""); err != nil {
		t.Fatalf("SourceBuffer: %v", err)
	}
	options, err := c.ShowOptions(ctx, OptionScopeServer, "", "", false)
	if err != nil {
		t.Fatalf("ShowOptions: %v", err)
	}
	if got := options["status-keys"]; got != "vi" {
		t.Errorf("status-keys = %q, want %q (default did not pick the most-recent buffer)", got, "vi")
	}
}

// TestSourceBuffer_MissingWrapsSentinel pins the typed-error contract
// for "named buffer does not exist": callers (and the JSON-RPC layer)
// must be able to errors.Is into errs.ErrSessionNotFound regardless of
// the exact phrase tmux emitted ("no buffer X" / "unknown buffer X").
func TestSourceBuffer_MissingWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a session so we exercise "server up, buffer missing"
	// rather than "no server" (different stderr shape).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor_src_miss", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	skipIfNoSourceBuffer(t, c)

	err := c.SourceBuffer(ctx, "ghost_buffer_nonexistent")
	if err == nil {
		t.Fatal("expected error sourcing missing buffer")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSourceBuffer_HeadlessWrapsSentinel pins the headless-server case:
// a fresh controller whose tmux daemon has not yet started returns the
// same "buffer not found" sentinel as a missing named buffer. There
// are no buffers without a server, so conflating the two is faithful
// to the wire contract clients use ("buffer is not here").
func TestSourceBuffer_HeadlessWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// No CreateSession beforehand — the controller's socket file does
	// not exist yet and tmux emits "error connecting" / "no server
	// running" / "No such file or directory" depending on the platform.
	// We can't probe for source-buffer support without a server, so
	// only run this assertion on tmux versions known to ship it. We
	// proxy the version check by spinning up a throwaway server first,
	// probing, then tearing it down so the headless assertion runs
	// against the real "no server" stderr.
	probe := newCtl(t)
	if err := probe.CreateSession(ctx, SessionSpec{Name: "probe_src_headless", Command: "/bin/sh"}); err != nil {
		t.Fatalf("probe CreateSession: %v", err)
	}
	skipIfNoSourceBuffer(t, probe)

	err := c.SourceBuffer(ctx, "")
	if err == nil {
		t.Fatal("expected error sourcing buffer with no server")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSourceBuffer_MalformedBodySurfacesPlain pins the parse-error
// contract: when the buffer body contains text tmux's command parser
// rejects (e.g. "not-a-tmux-command"), the failure surfaces as a plain
// non-sentinel error. Those are user-input mistakes against the command
// parser, not a missing-buffer case, so conflating them with
// ErrSessionNotFound would corrupt the wire contract for the
// "the named thing does not exist" code.
func TestSourceBuffer_MalformedBodySurfacesPlain(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "src_bad", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	skipIfNoSourceBuffer(t, c)

	if _, err := c.SetBuffer(ctx, "not-a-tmux-command", "bad_cfg", false); err != nil {
		t.Fatalf("SetBuffer: %v", err)
	}
	err := c.SourceBuffer(ctx, "bad_cfg")
	if err == nil {
		t.Fatal("expected error from parse failure")
	}
	if errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("parse error must NOT wrap ErrSessionNotFound (got %v)", err)
	}
	// Sanity: the failure mentions tmux's complaint about the unknown
	// verb so a future regression that swallowed stderr would surface
	// here.
	if !strings.Contains(strings.ToLower(err.Error()), "unknown") &&
		!strings.Contains(strings.ToLower(err.Error()), "not-a-tmux-command") {
		t.Errorf("error %q does not name the offending input", err)
	}
}
