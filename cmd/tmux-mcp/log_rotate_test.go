package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestOpenLogOutput_Rotation_DisabledIsByteIdentical pins the contract
// that -log-rotate-size=0 (the documented "disabled" value) keeps the
// legacy "open once, never rotate" behaviour byte-for-byte: the
// returned writer is a plain *os.File rather than a *rotatingFile, so
// existing deployments paired with logrotate(8) see no behavioural
// drift after the flag lands.
func TestOpenLogOutput_Rotation_DisabledIsByteIdentical(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")

	w, closer, err := openLogOutput(path, stderr, stdout, 0, 5)
	if err != nil {
		t.Fatalf("openLogOutput(%q, rotateSize=0): %v", path, err)
	}
	t.Cleanup(func() { _ = closer() })

	// The disabled case must hand back a raw *os.File so no extra
	// per-Write accounting happens on the hot path. Identifying by
	// concrete type (rather than reflecting on interface) is the
	// cheapest pin.
	if _, ok := w.(*os.File); !ok {
		t.Fatalf("rotation disabled: expected *os.File writer, got %T", w)
	}
}

// TestOpenLogOutput_Rotation_EnabledReturnsRotatingFile is the dual
// pin: as soon as -log-rotate-size becomes positive, the returned
// writer must be a *rotatingFile so the size-based rollover kicks in
// on subsequent Writes. This is the load-bearing wire-up test — flip
// it and the rest of the rotation tests below silently regress to
// the disabled path.
func TestOpenLogOutput_Rotation_EnabledReturnsRotatingFile(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")

	w, closer, err := openLogOutput(path, stderr, stdout, 1024, 5)
	if err != nil {
		t.Fatalf("openLogOutput(%q, rotateSize=1024): %v", path, err)
	}
	t.Cleanup(func() { _ = closer() })

	if _, ok := w.(*rotatingFile); !ok {
		t.Fatalf("rotation enabled: expected *rotatingFile writer, got %T", w)
	}
}

// TestRotatingFile_TwoRotationsLeaveTwoArchives is the headline
// behavioural test the spec calls out: write enough bytes through the
// rotator to force two rollovers and assert the live file plus two
// "<path>.<stamp>" archives exist on disk afterwards. The spec's
// retention bound (keep=5) is generous so the GC pass cannot mask a
// rollover bug — a rotator that "rotates" without leaving archives
// would still show one file here even with the GC disabled.
func TestRotatingFile_TwoRotationsLeaveTwoArchives(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")

	// Cap = 100 bytes, three writes of 60 bytes each. The first write
	// (60 bytes) fits inside the cap. The second write (60 bytes)
	// would push the file to 120 > 100, so the rotator renames the
	// live file out of the way (archive #1) and writes the second 60
	// bytes into a fresh file. The third write triggers archive #2.
	const cap = int64(100)
	const chunk = 60
	w, closer, err := openLogOutput(path, stderr, stdout, cap, 5)
	if err != nil {
		t.Fatalf("openLogOutput: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	payload := bytes.Repeat([]byte("a"), chunk)
	for i := range 3 {
		// Two-stamp rotations must produce two distinct archive
		// names. unix-ns is unique-enough on a busy host, but a
		// 1-second sleep would make this test pointlessly slow —
		// instead we rely on the nanosecond clock + the inherent
		// gap between os.Rename + os.OpenFile to space the names
		// apart. A small sleep belt-and-braces it: in practice
		// CI hosts on KVM see clock granularity above 1µs.
		if _, werr := w.Write(payload); werr != nil {
			t.Fatalf("write %d: %v", i, werr)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Close so the archive count is stable when we list the directory.
	if cerr := closer(); cerr != nil {
		t.Fatalf("closer: %v", cerr)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	var live, archives int
	prefix := filepath.Base(path) + "."
	for _, e := range entries {
		switch {
		case e.Name() == filepath.Base(path):
			live++
		case strings.HasPrefix(e.Name(), prefix):
			archives++
		}
	}
	if live != 1 {
		t.Fatalf("expected exactly 1 live file %q, got live=%d entries=%v", path, live, entries)
	}
	if archives != 2 {
		t.Fatalf("expected 2 archive files alongside %q, got archives=%d entries=%v", path, archives, entries)
	}
}

// TestRotatingFile_KeepEnforcement covers the retention bound: with
// -log-rotate-keep=2 and three rotations forced through the rotator,
// only the two newest archives must remain on disk. This pins the GC
// pass that runs at the tail of every rollover.
func TestRotatingFile_KeepEnforcement(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")

	// Cap = 100 bytes. Four writes of 60 bytes each force three
	// rollovers (writes 2, 3, 4 each rotate). With keep=2 the rotator
	// must end up with the live file + two newest archives = 3 files
	// total in the directory.
	const cap = int64(100)
	const chunk = 60
	w, closer, err := openLogOutput(path, stderr, stdout, cap, 2)
	if err != nil {
		t.Fatalf("openLogOutput: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	payload := bytes.Repeat([]byte("b"), chunk)
	for i := range 4 {
		if _, werr := w.Write(payload); werr != nil {
			t.Fatalf("write %d: %v", i, werr)
		}
		// Spread the unix-ns archive stamps so the older ones get
		// dropped first. A few ms is enough to make the mtime sort
		// deterministic on every reasonable filesystem.
		time.Sleep(5 * time.Millisecond)
	}
	if cerr := closer(); cerr != nil {
		t.Fatalf("closer: %v", cerr)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var live, archives int
	prefix := filepath.Base(path) + "."
	for _, e := range entries {
		switch {
		case e.Name() == filepath.Base(path):
			live++
		case strings.HasPrefix(e.Name(), prefix):
			archives++
		}
	}
	if live != 1 {
		t.Fatalf("expected exactly 1 live file, got live=%d entries=%v", live, entries)
	}
	if archives != 2 {
		t.Fatalf("expected keep=2 archives after 3 rotations, got archives=%d entries=%v", archives, entries)
	}
}

// TestRotatingFile_KeepZeroRetainsEverything is the boundary case:
// keep<=0 must skip the GC pass entirely so an operator who wants
// every archive on disk (e.g. for forensic / compliance reasons) can
// pass -log-rotate-keep=0 without losing data. The original file plus
// every rotated archive must remain.
func TestRotatingFile_KeepZeroRetainsEverything(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")

	const cap = int64(50)
	const chunk = 30
	w, closer, err := openLogOutput(path, stderr, stdout, cap, 0)
	if err != nil {
		t.Fatalf("openLogOutput: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	payload := bytes.Repeat([]byte("c"), chunk)
	for i := range 5 {
		if _, werr := w.Write(payload); werr != nil {
			t.Fatalf("write %d: %v", i, werr)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if cerr := closer(); cerr != nil {
		t.Fatalf("closer: %v", cerr)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	prefix := filepath.Base(path) + "."
	var archives int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			archives++
		}
	}
	// 5 writes, 30 bytes each, cap=50 → writes 2..5 each rotate (4
	// rollovers). With keep=0 every archive must remain on disk.
	if archives != 4 {
		t.Fatalf("expected all 4 archives retained when keep=0, got archives=%d entries=%v", archives, entries)
	}
}

// TestRotatingFile_ArchivePermissions confirms that an archived file
// keeps the same private 0600 mode the live log was opened with, so a
// rollover does not silently widen access to slog records that may
// carry hostnames / session names. The new live file must also be
// 0600 — an O_CREATE on a missing path must not honour the umask.
func TestRotatingFile_ArchivePermissions(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")

	w, closer, err := openLogOutput(path, stderr, stdout, 50, 5)
	if err != nil {
		t.Fatalf("openLogOutput: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	// Two writes of 40 bytes force one rotation.
	payload := bytes.Repeat([]byte("d"), 40)
	for range 2 {
		if _, werr := w.Write(payload); werr != nil {
			t.Fatalf("write: %v", werr)
		}
	}
	if cerr := closer(); cerr != nil {
		t.Fatalf("closer: %v", cerr)
	}

	// Live file = original path, archive is the only sibling with
	// "<path>." prefix.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	prefix := filepath.Base(path) + "."
	var archive string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			archive = filepath.Join(dir, e.Name())
			break
		}
	}
	if archive == "" {
		t.Fatalf("expected one archive, got entries=%v", entries)
	}
	for _, p := range []string{path, archive} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %q: %v", p, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("file %q perm=%#o, want 0o600", p, perm)
		}
	}
}

// TestRotatingFile_PreservesContent walks the bytes through the
// rotator and asserts that the concatenation of all archives plus the
// live file equals the concatenation of every payload that was
// Write()n. A rotator that drops bytes during the rename + reopen
// would silently fail this check, so we keep the assertion strict
// (byte-identical, no trimming).
func TestRotatingFile_PreservesContent(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")

	w, closer, err := openLogOutput(path, stderr, stdout, 30, 10)
	if err != nil {
		t.Fatalf("openLogOutput: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	// Distinct payloads so a misaligned rotation (e.g. dropping the
	// first byte after rename) would corrupt the reconstruction.
	payloads := []string{"alpha-1234567890\n", "beta-1234567890\n", "gamma-1234567890\n", "delta-1234567890\n"}
	var want bytes.Buffer
	for _, p := range payloads {
		if _, werr := io.WriteString(w, p); werr != nil {
			t.Fatalf("write %q: %v", p, werr)
		}
		want.WriteString(p)
		// Spread the unix-ns archive stamps so the read-back order
		// matches the write order.
		time.Sleep(2 * time.Millisecond)
	}
	if cerr := closer(); cerr != nil {
		t.Fatalf("closer: %v", cerr)
	}

	// Read every <path> + <path>.* file in the directory in mtime
	// order so the reconstruction is deterministic.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	prefix := filepath.Base(path) + "."
	type stamped struct {
		name  string
		mtime time.Time
	}
	var ours []stamped
	for _, e := range entries {
		if e.Name() != filepath.Base(path) && !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("info %q: %v", e.Name(), err)
		}
		ours = append(ours, stamped{name: e.Name(), mtime: info.ModTime()})
	}
	// Oldest first so concatenation equals the historical write
	// order.
	for i := range ours {
		for j := i + 1; j < len(ours); j++ {
			if ours[j].mtime.Before(ours[i].mtime) {
				ours[i], ours[j] = ours[j], ours[i]
			}
		}
	}
	var got bytes.Buffer
	for _, s := range ours {
		b, err := os.ReadFile(filepath.Join(dir, s.name))
		if err != nil {
			t.Fatalf("read %q: %v", s.name, err)
		}
		got.Write(b)
	}
	if got.String() != want.String() {
		t.Fatalf("reconstruction mismatch:\n  got=%q\n want=%q", got.String(), want.String())
	}
}

// TestRotatingFile_CloseIsIdempotent pins the contract that the
// caller can defer/close more than once without surfacing an error.
// Several callers (slog, the run() defer) might close the writer
// during shutdown and a second call must be a no-op.
func TestRotatingFile_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	w, closer, err := openLogOutput(path, stderr, stdout, 1024, 3)
	if err != nil {
		t.Fatalf("openLogOutput: %v", err)
	}
	if _, werr := io.WriteString(w, "hello\n"); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	if cerr := closer(); cerr != nil {
		t.Fatalf("first close: %v", cerr)
	}
	if cerr := closer(); cerr != nil {
		t.Fatalf("second close not idempotent: %v", cerr)
	}
}

// TestRotatingFile_WriteAfterCloseFails is the safety pin for the
// shutdown path: once Close has run, further writes must return an
// error rather than silently dropping bytes (which would mask
// bookkeeping bugs in callers that race shutdown with a final log).
func TestRotatingFile_WriteAfterCloseFails(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	w, closer, err := openLogOutput(path, stderr, stdout, 1024, 3)
	if err != nil {
		t.Fatalf("openLogOutput: %v", err)
	}
	if cerr := closer(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}
	if _, werr := w.Write([]byte("late\n")); werr == nil {
		t.Fatalf("expected error on Write after Close, got nil")
	}
}

// TestRotatingFile_ConcurrentWritesDoNotPanic checks the local
// sync.Mutex around Write/Close: concurrent writers must not race the
// rollover even though slog already serialises calls. A direct test
// caller (or a future unbuffered logger) must be safe to drive the
// rotator from multiple goroutines.
func TestRotatingFile_ConcurrentWritesDoNotPanic(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	w, closer, err := openLogOutput(path, stderr, stdout, 200, 5)
	if err != nil {
		t.Fatalf("openLogOutput: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	const goroutines = 8
	const writesPer = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func(g int) {
			defer wg.Done()
			payload := []byte(fmt.Sprintf("g%02d-%s\n", g, strings.Repeat("x", 16)))
			for range writesPer {
				if _, werr := w.Write(payload); werr != nil {
					t.Errorf("goroutine %d: write: %v", g, werr)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestRun_LogRotateFlag_EndToEnd is the CLI-surface integration test:
// invoking run() with -log-output + -log-rotate-size + -log-rotate-keep
// must wire the rotator end-to-end and survive a normal startup cycle.
// The 1-byte cap is deliberate: every single log record forces a
// rollover, so the directory ends with multiple archives and exactly
// one live file by the time run() returns.
func TestRun_LogRotateFlag_EndToEnd(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")

	var stdout, stderr bytes.Buffer
	if err := run(
		// JSON format produces one well-formed slog record per
		// dispatch error, and a 1-byte cap means every record forces
		// a rotation. keep=2 caps the directory at the live file +
		// 2 archives so the assertion below is tight.
		[]string{
			"-log-format=json",
			"-log-output=" + logPath,
			"-log-rotate-size=1",
			"-log-rotate-keep=2",
		},
		// Multiple malformed lines so the slog handler emits more
		// than one record and triggers more than one rotation.
		strings.NewReader("not json\nnot json\nnot json\n"),
		&stdout, &stderr,
	); err != nil {
		t.Fatalf("run(-log-rotate-size=1): %v stderr=%q", err, stderr.String())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	prefix := filepath.Base(logPath) + "."
	var live, archives int
	for _, e := range entries {
		switch {
		case e.Name() == filepath.Base(logPath):
			live++
		case strings.HasPrefix(e.Name(), prefix):
			archives++
		}
	}
	if live != 1 {
		t.Fatalf("expected exactly 1 live log file, got live=%d entries=%v", live, entries)
	}
	if archives < 1 {
		t.Fatalf("expected at least 1 archive after rotation, got archives=%d entries=%v", archives, entries)
	}
	if archives > 2 {
		t.Fatalf("expected keep=2 to bound archives, got archives=%d entries=%v", archives, entries)
	}
}

// TestLogRotateFlags_Documented locks the help text contract so a
// future rename of the flag (or a missed usage update) trips a test
// instead of silently breaking the operator-discoverable surface.
func TestLogRotateFlags_Documented(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-help"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil && err.Error() != "flag: help requested" {
		t.Fatalf("run(-help): unexpected error %v", err)
	}
	for _, want := range []string{"-log-rotate-size", "-log-rotate-keep"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("expected %q in usage block, got %q", want, stderr.String())
		}
	}
}
