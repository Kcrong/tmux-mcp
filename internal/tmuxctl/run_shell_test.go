package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// newCtlWithAnchor wires a fresh controller and creates a stub
// "anchor" session on it so the tmux server is up and running. tmux
// run-shell needs a live server to talk to even when -t is omitted
// — without an anchor every test would just exercise the "no server"
// stderr path and miss the actual run-shell semantics.
func newCtlWithAnchor(t *testing.T) (*Controller, context.Context) {
	t.Helper()
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	if err := c.CreateSession(ctx, SessionSpec{Name: "rs_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession(anchor): %v", err)
	}
	return c, ctx
}

// TestRunShell_HappyPathReturnsStdout pins the load-bearing contract:
// `RunShell(ctx, "printf hello", "", "", false)` actually returns the
// captured stdout from /bin/sh. tmux's own run-shell does not pipe
// stdout back to the client (it sends to view-mode), so this test is
// the regression guard that the temp-file capture wrapper survives.
func TestRunShell_HappyPathReturnsStdout(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c, ctx := newCtlWithAnchor(t)

	out, err := c.RunShell(ctx, "printf hello", "", "", false)
	if err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	if out != "hello" {
		t.Fatalf("RunShell stdout = %q, want %q", out, "hello")
	}
}

// TestRunShell_StartDirChdirsBeforeExec pins the `-c <start-dir>` flag
// end-to-end: when startDir is supplied, /bin/sh runs with that
// directory as its cwd, so `pwd` returns it verbatim. Without the
// flag tmux would inherit the controller's own cwd, which is rarely
// what the operator wants.
func TestRunShell_StartDirChdirsBeforeExec(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c, ctx := newCtlWithAnchor(t)

	dir := t.TempDir()
	out, err := c.RunShell(ctx, "pwd", dir, "", false)
	if err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	// pwd appends a single trailing newline; trim it so the assertion
	// stays focused on the cwd rather than the shell's framing.
	got := strings.TrimRight(out, "\n")
	if got != dir {
		t.Fatalf("RunShell pwd = %q, want %q", got, dir)
	}
}

// TestRunShell_BackgroundReturnsEmptyQuickly pins the `-b` contract:
// when background=true tmux runs the command detached, returns
// immediately, and the method returns ("", nil) regardless of what
// the underlying command writes. We deliberately use a sleep that
// would dominate a synchronous wait so a regression where -b is
// dropped would visibly hang this test past the (short) ctx
// deadline.
func TestRunShell_BackgroundReturnsEmptyQuickly(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c, _ := newCtlWithAnchor(t)
	// Use a tighter ctx than the anchor's so a regression that drops
	// -b would visibly trip the deadline rather than slipping through
	// inside the anchor's 30-second budget.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	start := time.Now()
	// A 30-second sleep would obviously trip the 5-second ctx if -b
	// were dropped; under -b tmux fires off the child and returns
	// almost immediately.
	out, err := c.RunShell(ctx, "sleep 30", "", "", true)
	if err != nil {
		t.Fatalf("RunShell(background): %v", err)
	}
	if out != "" {
		t.Fatalf("RunShell(background) stdout = %q, want empty", out)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("RunShell(background) blocked %s; -b should return promptly", d)
	}
}

// TestRunShell_MissingTargetWrapsSentinel pins the typed-error contract
// for an unknown target. tmux's own run-shell silently ignores an
// invalid -t (the command runs anyway against tmux's current/global
// context), which is exactly the surprising behaviour we paper over
// with an up-front has-session probe. The probe's stderr ("can't find
// session: <name>") is recognised by run()'s isSessionMissingMsg, so
// the typed errs.ErrSessionNotFound wrapping happens automatically.
func TestRunShell_MissingTargetWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c, ctx := newCtlWithAnchor(t)

	_, err := c.RunShell(ctx, "printf hello", "", "ghost_session_nonexistent_xyzzy", false)
	if err == nil {
		t.Fatal("expected error for missing target session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestRunShell_CommandWithSpacesSurvivesIntact pins the boundary
// contract: a command containing spaces (and quoted text) must reach
// /bin/sh verbatim, not as multiple argv entries that the shell would
// re-interpret. The temp-file wrapper rebuilds the command via brace
// grouping, so a regression where the wrapper splits on whitespace
// would visibly drop everything after "echo".
func TestRunShell_CommandWithSpacesSurvivesIntact(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c, ctx := newCtlWithAnchor(t)

	out, err := c.RunShell(ctx, "echo hello world", "", "", false)
	if err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	got := strings.TrimRight(out, "\n")
	if got != "hello world" {
		t.Fatalf("RunShell stdout = %q, want %q", got, "hello world")
	}
}

// TestRunShell_RejectsEmptyCommand locks the up-front guard. Without
// it tmux would happily exec /bin/sh on an empty argv (a no-op that
// silently succeeds), masking the caller's mistake. Runs without an
// anchor session because the guard fires before any tmux call.
func TestRunShell_RejectsEmptyCommand(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	_, err := c.RunShell(ctx, "", "", "", false)
	if err == nil {
		t.Fatal("expected error for empty command")
	}
	if !strings.Contains(err.Error(), "command required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRunShell_ShellExitNonZeroIsNotSessionSentinel pins the documented
// distinction: when the wrapped /bin/sh command exits non-zero the
// error is a legitimate "your command failed" surface, NOT
// errs.ErrSessionNotFound. The JSON-RPC layer relies on this to map
// the failure to CodeInternal instead of CodeSessionNotFound, so a
// regression where every shell error funneled into the session
// sentinel would silently break "did the build pass?" use cases.
func TestRunShell_ShellExitNonZeroIsNotSessionSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c, ctx := newCtlWithAnchor(t)

	_, err := c.RunShell(ctx, "exit 7", "", "", false)
	if err == nil {
		t.Fatal("expected error from shell exit 7")
	}
	if errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("shell exit 7 wrapped as ErrSessionNotFound: %v", err)
	}
}
