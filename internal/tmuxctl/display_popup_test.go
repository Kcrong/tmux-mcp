package tmuxctl

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// fakePopupTmux writes a tiny shell script that masquerades as `tmux`
// for the DisplayPopup suite. The script:
//
//   - Honours `-V` so [resolveTmuxBin] / [checkTmuxVersion] still pass
//     during construction (we hard-code "tmux 3.4" — well above the 3.0
//     floor the production checker enforces).
//   - When invoked with the production `-S <socket>` prefix and a
//     `display-popup` verb, appends the *full argv* (one token per
//     line, with a NUL between invocations) to the path argFile so
//     the test can assert on the exact flags the controller emitted.
//   - When stderrMsg is non-empty, also writes that to stderr and
//     exits with status 1 so the controller exercises its
//     error-mapping paths (ErrSessionNotFound wrapping, etc.).
//   - Returns 0 for everything else (kill-server, has-session probes,
//     the version check) so [Controller.Shutdown] does not log noise.
//
// The argFile is flushed every invocation so the test can read the
// recorded argv synchronously after DisplayPopup returns.
func fakePopupTmux(t *testing.T, argFile, stderrMsg string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake tmux script needs unix-like shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux")
	// The shell script itself is straightforward: dispatch on the
	// first non-flag argument. The `-V` short-circuit comes first
	// so the controller's startup version check passes without
	// touching the recorder file. Quoting: we use single-quoted
	// here-string fragments and inject argFile / stderrMsg via
	// shell variable interpolation outside the quotes so a path
	// containing spaces still works (TempDir() rarely produces
	// such, but the test harness should not care).
	//
	// `printf '%s\n'` with "$@" prints one argv element per line,
	// followed by a NUL marker so multi-call assertions can split
	// reliably. The trailing `>>` keeps the file additive; we
	// truncate on first use from the test side via os.WriteFile.
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"-V\" ]; then echo \"tmux 3.4\"; exit 0; fi\n" +
		"# Skip server-flag prefix: -S <sock> [-f <conf>] then the verb.\n" +
		"shift 2 # drop -S <sock>\n" +
		"if [ \"$1\" = \"-f\" ]; then shift 2; fi\n" +
		"verb=\"$1\"\n" +
		"case \"$verb\" in\n" +
		"  display-popup)\n" +
		"    {\n" +
		"      printf '%s\\n' \"$@\"\n" +
		"      printf '\\0'\n" +
		"    } >> \"" + argFile + "\"\n" +
		"    ;;\n" +
		"esac\n" +
		"if [ -n \"" + stderrMsg + "\" ] && [ \"$verb\" = \"display-popup\" ]; then\n" +
		"  printf '%s\\n' \"" + stderrMsg + "\" 1>&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"exit 0\n"
	// Write to a sibling path and rename atomically into place so a
	// concurrent test process that exec()s right as another goroutine
	// finishes WriteFile cannot race on ETXTBSY ("text file busy"). The
	// race fires when the kernel briefly sees a file open-for-write at
	// the same inode the executor wants to fork+exec; rename(2) bypasses
	// that window because the destination path is freshly published with
	// no writer ever owning that inode.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename fake tmux into place: %v", err)
	}
	return path
}

// readPopupArgv reads the most recently recorded display-popup argv
// from the file fakePopupTmux appends to. Returns the slice of argv
// tokens (without the trailing NUL marker) — empty when the file does
// not exist yet, which means DisplayPopup never reached the recorder.
func readPopupArgv(t *testing.T, argFile string) []string {
	t.Helper()
	f, err := os.Open(argFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("open argv file: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	scanner := bufio.NewScanner(f)
	var argv []string
	for scanner.Scan() {
		line := scanner.Text()
		// The fake script writes a literal NUL byte after each
		// invocation's argv block. Split lines stop on '\n' so
		// any NUL ends up at the start of the next line; trim
		// it here so the assertions see clean tokens.
		line = strings.TrimLeft(line, "\x00")
		if line == "" {
			continue
		}
		argv = append(argv, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan argv file: %v", err)
	}
	return argv
}

// newPopupCtl builds a Controller wired to the fake tmux script in
// fakePopupTmux. The argFile is created (truncated) up front so the
// test can read it back even if DisplayPopup never wrote to it.
//
// The construction is wrapped in a small retry because Linux can return
// ETXTBSY ("text file busy") when one goroutine is mid-fork() with a
// write-open file descriptor inherited from another goroutine, and
// another goroutine exec()s a different freshly-written script. Go's
// runtime fans goroutines out across CPUs, so under -parallel the race
// fires intermittently. The retry is bounded to a few attempts with a
// short backoff — each iteration creates a fresh script in a fresh
// TempDir, so the retry itself does not introduce shared state.
func newPopupCtl(t *testing.T, stderrMsg string) (*Controller, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake tmux script needs unix-like shell")
	}
	argFile := filepath.Join(t.TempDir(), "argv.log")
	if err := os.WriteFile(argFile, nil, 0o644); err != nil {
		t.Fatalf("seed argv file: %v", err)
	}
	var (
		c   *Controller
		err error
	)
	for attempt := 0; attempt < 5; attempt++ {
		bin := fakePopupTmux(t, argFile, stderrMsg)
		c, err = NewWithSocket("", WithBinary(bin))
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "text file busy") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("NewWithSocket(fake): %v", err)
	}
	t.Cleanup(func() { c.Shutdown(context.Background()) })
	return c, argFile
}

// TestDisplayPopup_AssemblesArgvInStableOrder is the load-bearing
// happy path: every supported flag is set, and the constructed argv
// must match the documented stable ordering — booleans first
// (-B / -C / -E / -r), then keyed flags in the order
// (-T / -S / -b / -d / -e / -h / -w / -x / -y / -t), and finally
// the positional shell-command. A regression that re-ordered the
// loop body would surface here as a token-by-token mismatch.
func TestDisplayPopup_AssemblesArgvInStableOrder(t *testing.T) {
	t.Parallel()
	c, argFile := newPopupCtl(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.DisplayPopup(ctx, DisplayPopupOptions{
		NoBorder:        true,
		CloseOnExit:     true,
		CloseOnZeroExit: true,
		Centered:        true,
		Title:           "review",
		BorderStyle:     "fg=red",
		BorderLines:     "rounded",
		StartDirectory:  "/tmp",
		Env:             map[string]string{"FOO": "1", "BAR": "two"},
		Height:          "80%",
		Width:           "60%",
		X:               "10",
		Y:               "5",
		Target:          "demo:0.1",
		ShellCommand:    "echo hi",
	}); err != nil {
		t.Fatalf("DisplayPopup: %v", err)
	}

	got := readPopupArgv(t, argFile)
	want := []string{
		"display-popup",
		"-B", "-C", "-E", "-r",
		"-T", "review",
		"-S", "fg=red",
		"-b", "rounded",
		"-d", "/tmp",
		"-e", "BAR=two",
		"-e", "FOO=1",
		"-h", "80%",
		"-w", "60%",
		"-x", "10",
		"-y", "5",
		"-t", "demo:0.1",
		"echo hi",
	}
	if !equalSlice(got, want) {
		t.Fatalf("argv mismatch\n got: %v\nwant: %v", got, want)
	}
}

// TestDisplayPopup_OmitsAbsentFlags pins the inverse: an empty
// DisplayPopupOptions must produce a bare `display-popup` argv. tmux
// resolves every default itself (centred, half-the-terminal, default
// border) so the controller's job is just to forward what the caller
// supplied — anything more would silently override tmux's own
// defaulting.
func TestDisplayPopup_OmitsAbsentFlags(t *testing.T) {
	t.Parallel()
	c, argFile := newPopupCtl(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.DisplayPopup(ctx, DisplayPopupOptions{}); err != nil {
		t.Fatalf("DisplayPopup: %v", err)
	}

	got := readPopupArgv(t, argFile)
	want := []string{"display-popup"}
	if !equalSlice(got, want) {
		t.Fatalf("argv mismatch\n got: %v\nwant: %v", got, want)
	}
}

// TestDisplayPopup_EnvOrderDeterministic is a focused regression test
// for the sorted env emission. Without the explicit sort.Strings step,
// Go's map iteration would shuffle the -e flags between calls and the
// argv assertion in TestDisplayPopup_AssemblesArgvInStableOrder would
// flake on every test run.
func TestDisplayPopup_EnvOrderDeterministic(t *testing.T) {
	t.Parallel()
	c, argFile := newPopupCtl(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.DisplayPopup(ctx, DisplayPopupOptions{
		Env: map[string]string{"Z": "last", "A": "first", "M": "middle"},
	}); err != nil {
		t.Fatalf("DisplayPopup: %v", err)
	}

	got := readPopupArgv(t, argFile)
	want := []string{
		"display-popup",
		"-e", "A=first",
		"-e", "M=middle",
		"-e", "Z=last",
	}
	if !equalSlice(got, want) {
		t.Fatalf("argv mismatch\n got: %v\nwant: %v", got, want)
	}
}

// TestDisplayPopup_WrapsCantFindWindow pins the contract the JSON-RPC
// layer relies on: a "can't find window" stderr from tmux must be
// translated into a wrapped errs.ErrSessionNotFound so the dispatcher
// can map it to CodeSessionNotFound. run() already covers the
// "can't find session" phrasing; this guard is for the window/pane
// branches the controller wraps explicitly.
func TestDisplayPopup_WrapsCantFindWindow(t *testing.T) {
	t.Parallel()
	c, _ := newPopupCtl(t, "can't find window: bogus")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.DisplayPopup(ctx, DisplayPopupOptions{Target: "bogus:99"})
	if err == nil {
		t.Fatal("expected error for unknown window target")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestDisplayPopup_WrapsCantFindPane pins the same contract for the
// pane-half branch: tmux's "can't find pane" surfaces as the same
// typed sentinel so callers do not have to substring-match the
// version-specific stderr.
func TestDisplayPopup_WrapsCantFindPane(t *testing.T) {
	t.Parallel()
	c, _ := newPopupCtl(t, "can't find pane: bogus")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.DisplayPopup(ctx, DisplayPopupOptions{Target: "demo:0.99"})
	if err == nil {
		t.Fatal("expected error for unknown pane target")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestDisplayPopup_PassesThroughOtherErrors guards the inverse: a
// random tmux failure (e.g. "no current client", or an unknown flag
// on an older tmux) must not be mis-categorised as a missing-target
// error. The dispatcher relies on this so CodeInternal stays
// reserved for "something else went wrong".
func TestDisplayPopup_PassesThroughOtherErrors(t *testing.T) {
	t.Parallel()
	c, _ := newPopupCtl(t, "no current client")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.DisplayPopup(ctx, DisplayPopupOptions{})
	if err == nil {
		t.Fatal("expected error for headless server")
	}
	if errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v unexpectedly wraps ErrSessionNotFound", err)
	}
	if !strings.Contains(err.Error(), "no current client") {
		t.Fatalf("error %v does not preserve tmux stderr", err)
	}
}

// equalSlice is a tiny string-slice equality helper. The tests want
// position-by-position equality (the argv ordering is the contract),
// not set-equality, so we hand-roll it instead of pulling in
// reflect.DeepEqual — the failure messages are the same length and
// the loop body documents what "equal" means here.
func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
