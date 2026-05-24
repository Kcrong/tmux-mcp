package tmuxctl

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

func skipIfNoTmux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("tmux tests require unix-like OS")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
}

func newCtl(t *testing.T) *Controller {
	t.Helper()
	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Shutdown(context.Background()) })
	return c
}

func TestSessionLifecycle(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "alpha", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	names, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(names) != 1 || names[0] != "alpha" {
		t.Fatalf("unexpected sessions: %v", names)
	}
	has, err := c.HasSession(ctx, "alpha")
	if err != nil || !has {
		t.Fatalf("HasSession: has=%v err=%v", has, err)
	}
	if err := c.KillSession(ctx, "alpha"); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
}

func TestSendKeysAndCapture(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{
		Name:    "echo",
		Command: "/bin/sh",
		Width:   80, Height: 20,
		Env: map[string]string{"PS1": "$ "},
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Drive the shell: print a sentinel string.
	if err := c.SendKeys(ctx, "echo", []string{"echo TMUX_MCP_HELLO_42", "Enter"}, false); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	body, err := c.WaitForStable(ctx, "echo", 300*time.Millisecond, 100*time.Millisecond, 5*time.Second)
	if err != nil {
		t.Fatalf("WaitForStable: %v", err)
	}
	if !strings.Contains(body, "TMUX_MCP_HELLO_42") {
		t.Fatalf("captured body did not contain sentinel:\n%s", body)
	}
}

func TestWaitForText(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "wait", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := c.SendKeys(ctx, "wait", []string{"printf 'READY-%s\\n' 99", "Enter"}, false); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	m, err := c.WaitForText(ctx, "wait", `READY-\d+`, 50*time.Millisecond, 5*time.Second)
	if err != nil {
		t.Fatalf("WaitForText: %v", err)
	}
	if !strings.HasPrefix(m.Match, "READY-") {
		t.Fatalf("match = %q", m.Match)
	}
}

func TestWaitForText_TimesOut(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "to", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	_, err := c.WaitForText(ctx, "to", `IMPOSSIBLE_PATTERN_XYZZY`, 50*time.Millisecond, 400*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "not found within") {
		t.Fatalf("unexpected error: %v", err)
	}
	// Must wrap the typed timeout sentinel so the JSON-RPC layer can map
	// it to CodeTimeout (-32002).
	if !errors.Is(err, errs.ErrTimeout) {
		t.Fatalf("error %v does not wrap errs.ErrTimeout", err)
	}
}

// TestKillSession_UnknownWrapsSentinel proves tmuxctl surfaces a missing
// session via the typed sentinel — relied on by the dispatcher to emit
// CodeSessionNotFound on the wire.
func TestKillSession_UnknownWrapsSentinel(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Create-then-kill so the tmux server is definitely up; then ask it
	// to kill a name that doesn't exist.
	if err := c.CreateSession(ctx, SessionSpec{Name: "real", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	err := c.KillSession(ctx, "ghost_session_nonexistent")
	if err == nil {
		t.Fatal("expected error killing missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestWaitForStable_TimesOutWrapsSentinel confirms the WaitForStable
// timeout path also wraps the typed sentinel.
func TestWaitForStable_TimesOutWrapsSentinel(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "ws", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Constant churn: keep printing the date in a tight loop so the pane
	// is never quiet for the requested window.
	if err := c.SendKeys(ctx, "ws", []string{"while :; do date +%N; done", "Enter"}, false); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	// quiet > timeout guarantees the deadline trips first.
	_, err := c.WaitForStable(ctx, "ws", 1*time.Second, 50*time.Millisecond, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected wait_for_stable timeout error")
	}
	if !errors.Is(err, errs.ErrTimeout) {
		t.Fatalf("error %v does not wrap errs.ErrTimeout", err)
	}
}

func TestResize(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.CreateSession(ctx, SessionSpec{Name: "rs", Command: "/bin/sh", Width: 80, Height: 24}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := c.Resize(ctx, "rs", 100, 30); err != nil {
		t.Fatalf("Resize: %v", err)
	}
}

func TestListSessions_EmptyOnFreshController(t *testing.T) {
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	names, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("expected no sessions, got %v", names)
	}
}

// TestNewWithSocket_HonoursExplicitPath verifies the controller's socket
// matches the caller-supplied path verbatim (no MkdirTemp shadow).
func TestNewWithSocket_HonoursExplicitPath(t *testing.T) {
	skipIfNoTmux(t)
	dir := t.TempDir()
	want := filepath.Join(dir, "tmux.sock")
	c, err := NewWithSocket(want)
	if err != nil {
		t.Fatalf("NewWithSocket(%q): %v", want, err)
	}
	t.Cleanup(func() { c.Shutdown(context.Background()) })
	if got := c.Socket(); got != want {
		t.Fatalf("socket = %q, want %q", got, want)
	}
	if c.ownsDir {
		t.Fatal("ownsDir must be false for caller-supplied paths")
	}
}

// TestNewWithSocket_ParentSurvivesShutdown asserts that Shutdown on a
// caller-supplied socket leaves the parent directory in place — only
// the socket file (if any) is removed. Operators of systemd / container
// deployments rely on this so that a restart does not race against a
// vanishing /run/tmux-mcp directory.
func TestNewWithSocket_ParentSurvivesShutdown(t *testing.T) {
	skipIfNoTmux(t)
	dir := t.TempDir()
	socket := filepath.Join(dir, "tmux.sock")
	c, err := NewWithSocket(socket)
	if err != nil {
		t.Fatalf("NewWithSocket: %v", err)
	}
	c.Shutdown(context.Background())
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("parent dir %q removed by Shutdown: %v", dir, err)
	}
}

func TestNewWithSocket_RejectsRelativePath(t *testing.T) {
	skipIfNoTmux(t)
	_, err := NewWithSocket("relative/sock")
	if err == nil {
		t.Fatal("expected error for relative socket path")
	}
	if !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewWithSocket_RejectsMissingParent(t *testing.T) {
	skipIfNoTmux(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist", "sock")
	_, err := NewWithSocket(missing)
	if err == nil {
		t.Fatal("expected error for missing parent dir")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewWithSocket_RejectsParentNotDirectory(t *testing.T) {
	skipIfNoTmux(t)
	dir := t.TempDir()
	// A regular file in place of the parent directory.
	notDir := filepath.Join(dir, "blocker")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	_, err := NewWithSocket(filepath.Join(notDir, "sock"))
	if err == nil {
		t.Fatal("expected error when parent path is a file")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestNew_OwnsScratchDir confirms the default New() path still uses an
// MkdirTemp-backed directory and cleans it up on Shutdown.
func TestNew_OwnsScratchDir(t *testing.T) {
	skipIfNoTmux(t)
	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !c.ownsDir {
		t.Fatal("ownsDir must be true for the default constructor")
	}
	dir := filepath.Dir(c.socket)
	c.Shutdown(context.Background())
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("scratch dir %q should be removed, stat err = %v", dir, err)
	}
}

// TestWithBinary_HonoursExplicitPath verifies the binary override
// actually flows through to the controller's `bin` field, so every
// downstream `tmux …` command exec's the operator-supplied path
// instead of `exec.LookPath("tmux")`. The fake-tmux script's `-V`
// output is the sentinel we assert on: if WithBinary did not take
// effect, NewWithSocket would either fall back to the real tmux on
// PATH (and report a different version) or fail with the LookPath
// error.
func TestWithBinary_HonoursExplicitPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake tmux script needs unix-like shell")
	}
	bin := fakeTmux(t, "tmux 3.4")
	c, err := NewWithSocket("", WithBinary(bin))
	if err != nil {
		t.Fatalf("NewWithSocket(WithBinary(%q)): %v", bin, err)
	}
	t.Cleanup(func() { c.Shutdown(context.Background()) })
	if c.bin != bin {
		t.Fatalf("controller.bin = %q, want %q", c.bin, bin)
	}
}

// TestWithBinary_EmptyFallsBackToPath pins the documented escape
// hatch: passing WithBinary("") must behave as if the option were
// never applied, so callers can forward a possibly-empty CLI flag
// without an extra branch. We just confirm the controller still
// constructs successfully when tmux is available on PATH.
func TestWithBinary_EmptyFallsBackToPath(t *testing.T) {
	skipIfNoTmux(t)
	c, err := NewWithSocket("", WithBinary(""))
	if err != nil {
		t.Fatalf("NewWithSocket(WithBinary(\"\")): %v", err)
	}
	t.Cleanup(func() { c.Shutdown(context.Background()) })
	if c.bin == "" {
		t.Fatal("controller.bin must be populated when override is empty")
	}
}

// TestWithBinary_RejectsRelativePath mirrors the -socket validation
// contract: a relative path is refused at construction time so the
// operator immediately sees the mistake instead of an obscure exec
// failure once the working directory shifts.
func TestWithBinary_RejectsRelativePath(t *testing.T) {
	_, err := NewWithSocket("", WithBinary("relative/tmux"))
	if err == nil {
		t.Fatal("expected error for relative tmux binary path")
	}
	if !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestWithBinary_RejectsMissingFile confirms a non-existent path
// surfaces as a clean "tmux binary %q not executable: ..." error
// rather than getting swallowed and re-emerging downstream as a
// confusing "fork/exec" failure.
func TestWithBinary_RejectsMissingFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "no-such-tmux")
	_, err := NewWithSocket("", WithBinary(missing))
	if err == nil {
		t.Fatal("expected error for missing tmux binary")
	}
	if !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("expected 'not executable' phrase, got: %v", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("expected error to quote the offending path %q, got: %v",
			missing, err)
	}
}

// TestWithBinary_RejectsDirectory makes sure a directory at the
// supplied path is rejected up front rather than producing a confusing
// permission error at exec time.
func TestWithBinary_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	_, err := NewWithSocket("", WithBinary(dir))
	if err == nil {
		t.Fatal("expected error when tmux binary path is a directory")
	}
	if !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("expected 'not executable' phrase, got: %v", err)
	}
}

// TestWithBinary_RejectsNonExecutableFile pins the executable-bit
// check: a regular file with no executable bits set must be refused.
// Without this, exec'ing it later would surface as "permission denied"
// from the kernel — much further from the operator's mistake.
func TestWithBinary_RejectsNonExecutableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits don't map cleanly on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	_, err := NewWithSocket("", WithBinary(path))
	if err == nil {
		t.Fatal("expected error for non-executable tmux binary")
	}
	if !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("expected 'not executable' phrase, got: %v", err)
	}
}

// TestProbeVersionWithBinary_UsesOverride confirms the `-probe` /
// `-dry-run` paths exec the operator-supplied binary verbatim instead
// of looking tmux up on PATH. We point ProbeVersionWithBinary at a
// fake-tmux script that prints a sentinel "tmux next-9.9" banner: if
// the override were ignored the returned version would not match.
func TestProbeVersionWithBinary_UsesOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake tmux script needs unix-like shell")
	}
	bin := fakeTmux(t, "tmux next-9.9")
	v, err := ProbeVersionWithBinary(context.Background(), bin)
	if err != nil {
		t.Fatalf("ProbeVersionWithBinary(%q): %v", bin, err)
	}
	if !strings.Contains(v, "9.9") {
		t.Fatalf("ProbeVersionWithBinary returned %q; expected sentinel 9.9", v)
	}
}

// TestProbeVersionWithBinary_RejectsRelative pins the validation
// contract on the probe path: a relative override is refused before
// any subprocess is spawned, so a bogus -tmux-bin value can never
// hang the liveness check on a misconfigured PATH.
func TestProbeVersionWithBinary_RejectsRelative(t *testing.T) {
	_, err := ProbeVersionWithBinary(context.Background(), "relative/tmux")
	if err == nil {
		t.Fatal("expected error for relative tmux binary path")
	}
	if !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestProbeVersionWithBinary_EmptyFallsBackToPath confirms passing an
// empty override is byte-equivalent to the legacy ProbeVersion path
// (i.e. tmux looked up on $PATH). This is what main.go relies on when
// -tmux-bin is not set.
func TestProbeVersionWithBinary_EmptyFallsBackToPath(t *testing.T) {
	skipIfNoTmux(t)
	got, err := ProbeVersionWithBinary(context.Background(), "")
	if err != nil {
		t.Fatalf("ProbeVersionWithBinary(\"\"): %v", err)
	}
	want, err := ProbeVersion(context.Background())
	if err != nil {
		t.Fatalf("ProbeVersion: %v", err)
	}
	if got != want {
		t.Fatalf("ProbeVersionWithBinary(\"\") = %q, want %q (= ProbeVersion)", got, want)
	}
}
