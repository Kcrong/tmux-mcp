package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// LogOutputStderr is the magic value for -log-output that keeps the slog
// stream on the supplied stderr writer (the default behaviour). Operators
// pass it explicitly when scripting around environments that may otherwise
// override the default.
const LogOutputStderr = "stderr"

// LogOutputStdout is the magic value for -log-output that routes slog
// output to stdout. It exists only for ad-hoc debugging in combination
// with -dry-run / -version (paths that never serve JSON-RPC frames).
// Using it while the server is actually serving stdio will interleave
// log records with JSON-RPC frames and corrupt the protocol.
const LogOutputStdout = "stdout"

// noopCloser is the closer returned for writer targets we do not own:
// stderr, stdout, and the disabled case. It mirrors the audit sink's
// "Close on a nil/borrowed sink is a no-op" contract so callers can
// always defer closer() unconditionally.
func noopCloser() error { return nil }

// openLogOutput resolves -log-output and returns the writer slog should
// install plus a closer the caller must invoke during shutdown.
//
// The two magic values:
//
//   - "" or "stderr"  → returns (stderr, noopCloser, nil). The default
//     behaviour: structured logs share the supplied stderr stream and
//     the caller does not own the fd's lifetime.
//   - "stdout"        → returns (stdout, noopCloser, nil). DANGER: this
//     interleaves log records with the JSON-RPC frames the server
//     writes when actually serving stdio. Only useful with -dry-run /
//     -version where stdout carries one well-known line.
//
// Any other value is treated as a filesystem path opened with
// O_APPEND|O_CREATE|O_WRONLY at mode 0600 — same shape as the
// audit-log sink so operator expectations transfer cleanly. The
// returned closer flushes via os.File.Sync (best-effort) and then
// closes the fd; on success the file outlives the call until the
// caller invokes the closer during shutdown.
//
// When rotateSize > 0, the returned writer is a [rotatingFile] that
// renames the live log to "<path>.<unix-ns>" and reopens it whenever
// the next Write would push the file past the cap. rotateKeep bounds
// the number of archive files retained on disk: the oldest (by mtime)
// are deleted once the count exceeds the limit. Both knobs are ignored
// when rotateSize <= 0, preserving the legacy "open once, never
// rotate" behaviour byte-for-byte for deployments that rely on
// logrotate(8).
//
// Errors carry the path so operators see exactly what was wrong (e.g.
// "open log output \"/no/such/dir/agent.log\": open …: no such file or
// directory") instead of a bare permission/missing-parent diagnostic.
func openLogOutput(target string, stderr, stdout io.Writer, rotateSize int64, rotateKeep int) (io.Writer, func() error, error) {
	switch target {
	case "", LogOutputStderr:
		return stderr, noopCloser, nil
	case LogOutputStdout:
		return stdout, noopCloser, nil
	}
	// O_APPEND keeps concurrent processes from clobbering each other's
	// records (e.g. an operator running two binaries against the same
	// log file during a rolling restart); mode 0600 keeps the log
	// private because slog records can carry hostnames, session
	// names, and timing data the operator would rather not leak via
	// a group-readable file.
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log output %q: %w", target, err)
	}
	if rotateSize <= 0 {
		closer := func() error {
			// Sync first so any buffered data hits disk even if the
			// process is about to exit; ignore the sync error because
			// not every backing fs supports it (e.g. /dev/null on some
			// kernels) and the subsequent Close error is what we
			// surface either way.
			_ = f.Sync()
			return f.Close()
		}
		return f, closer, nil
	}
	// Rotation enabled: wrap the fd in a rotatingFile so subsequent
	// Write calls roll over once size + len(p) would exceed the cap.
	// Stat the file we just opened (instead of starting at zero) so an
	// O_APPEND reopen of an existing file picks up the right initial
	// counter — otherwise the first rollover would only fire after we
	// wrote rotateSize bytes *on top* of the existing content.
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("open log output %q: %w", target, err)
	}
	rf := &rotatingFile{
		path: target,
		size: info.Size(),
		cap:  rotateSize,
		keep: rotateKeep,
		f:    f,
	}
	return rf, rf.Close, nil
}

// rotatingFile is the size-based log rotator used by -log-output when
// -log-rotate-size is set. It implements [io.WriteCloser] over a single
// underlying *os.File: each Write checks whether the new bytes would
// push the file past the configured cap and, if so, renames the live
// file out of the way and reopens a fresh one before writing.
//
// The size accounting is a counter held in the struct (size since the
// last open) rather than an fstat per Write — slog calls Write on the
// hot path of every record and an extra syscall per line is the kind
// of cost an operator would notice on a busy server. The counter is
// seeded from os.File.Stat() at open time so an O_APPEND reopen of an
// existing log resumes accounting from the right offset.
//
// Concurrency: slog handlers serialise access to the underlying writer
// with their own mutex, but we still take a sync.Mutex here so callers
// outside slog (an ad-hoc test, a future direct writer) cannot race
// the rollover. The mutex is held across the rename + reopen so a
// concurrent Close cannot land on a stale fd.
type rotatingFile struct {
	// path is the live log filename. Archives are renamed alongside it
	// as path.<unix-ns> so an operator listing the directory sees the
	// rotation history beside the active file.
	path string
	// cap is the maximum size in bytes the live file may reach before
	// the next Write triggers a rollover. Always > 0 when this type is
	// in use (the disabled case returns a plain *os.File).
	cap int64
	// keep is the maximum number of archive files retained on disk.
	// Once the directory contains more, the oldest (by mtime) are
	// deleted. Values <= 0 retain every archive (no GC).
	keep int

	// mu guards size + f against concurrent Write/Close. slog already
	// serialises calls into Write but tests and direct callers may
	// not, so we keep the mutex local to this type instead of relying
	// on an external lock.
	mu sync.Mutex
	// size is the byte count written since the active file was opened
	// (or the existing tail when O_APPEND reopened a non-empty file).
	// Cheaper than fstat(2) per record on the hot path.
	size int64
	// f is the active append-only file. nil after Close.
	f *os.File
}

// Write writes p to the underlying file, rotating first when adding p
// would push the file past the configured cap. The rotation rename and
// reopen happen synchronously inside Write because slog handlers do
// not buffer through goroutines — every record arrives on the writer
// in order, and ordering is what operators care about when correlating
// rotated archives back to the live file.
func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, errors.New("rotatingFile: write after close")
	}
	// Roll over if appending p would exceed the cap. We rotate even
	// when p is itself bigger than cap — the alternative (skip the
	// rollover and keep growing) defeats the operator's intent of "no
	// file ever bigger than N bytes" and a single huge slog record is
	// a debugging aid worth keeping in its own archive.
	if r.size > 0 && r.size+int64(len(p)) > r.cap {
		if err := r.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// Close flushes and closes the active file. Idempotent: a second Close
// returns nil so the caller can defer it unconditionally.
func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	_ = r.f.Sync()
	err := r.f.Close()
	r.f = nil
	return err
}

// rotateLocked renames the live file to "<path>.<unix-ns>", reopens a
// fresh "<path>", and (when keep > 0) deletes any archives beyond the
// retention bound. Caller must hold r.mu.
//
// The rename uses time.Now().UnixNano() so two rotations inside the
// same second still get unique filenames — operators inspecting the
// directory get a stable lexicographic ordering that matches mtime
// ordering, which makes scripted log archival straightforward.
func (r *rotatingFile) rotateLocked() error {
	// Sync before renaming so any buffered page-cache writes land in
	// the file we are about to archive. Best-effort: a filesystem
	// that does not support fsync (rare on production hosts but
	// possible inside containers / fuse mounts) still gets a useful
	// rotation, just without the extra durability guarantee.
	_ = r.f.Sync()
	// Closing before rename is the portable choice: Linux is happy to
	// rename an open file but Windows is not, and even on Linux the
	// open fd would keep writing to the renamed inode until reopen
	// finished, blurring the boundary between "before rotation" and
	// "after rotation" records.
	if err := r.f.Close(); err != nil {
		// Continue past a Close error: the rename below is what
		// matters for the operator-visible archive, and surfacing the
		// Close error masks the more interesting rename / reopen
		// errors that follow.
		_ = err
	}
	r.f = nil
	archive := fmt.Sprintf("%s.%d", r.path, time.Now().UnixNano())
	if err := os.Rename(r.path, archive); err != nil {
		// Try to recover: reopen the original path so the next Write
		// has somewhere to land. If even that fails, surface the
		// rename error (the more informative one) so the operator
		// knows what actually went wrong.
		f, oerr := os.OpenFile(r.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if oerr == nil {
			r.f = f
			info, ierr := f.Stat()
			if ierr == nil {
				r.size = info.Size()
			}
		}
		return fmt.Errorf("rotate log %q: rename: %w", r.path, err)
	}
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("rotate log %q: open: %w", r.path, err)
	}
	r.f = f
	r.size = 0
	// Best-effort archive GC. A failure to enumerate or delete is not
	// worth aborting the rollover — the active file is already happy,
	// and the operator's worst case is a few extra archives until
	// next rotation. We deliberately do not surface the error so a
	// transient permission flap on a sibling file does not turn into
	// a Write error on a perfectly good rotation.
	if r.keep > 0 {
		_ = pruneArchives(r.path, r.keep)
	}
	return nil
}

// pruneArchives deletes all but the newest `keep` archive files
// matching "<path>.<unix-ns>" in the directory holding path. Sort key
// is mtime so an external archive job that touches the file (chmod,
// `touch -t`) cannot trick the rotator into deleting the wrong copy.
//
// Errors are returned but the caller may choose to ignore them: the
// rotator does, because losing the GC pass is strictly less bad than
// failing the user-visible Write that triggered it.
func pruneArchives(path string, keep int) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path) + "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type stamped struct {
		name  string
		mtime time.Time
	}
	var archives []stamped
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || len(name) <= len(base) || name[:len(base)] != base {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		archives = append(archives, stamped{name: name, mtime: info.ModTime()})
	}
	if len(archives) <= keep {
		return nil
	}
	// Newest first so we keep the head of the slice and delete the
	// tail. Using ModTime() means an out-of-band `touch` on an old
	// archive bumps it back to the front, which is the right thing:
	// operators who deliberately update an archive's mtime are
	// signalling "this one is interesting".
	sort.Slice(archives, func(i, j int) bool {
		return archives[i].mtime.After(archives[j].mtime)
	})
	for _, a := range archives[keep:] {
		_ = os.Remove(filepath.Join(dir, a.name))
	}
	return nil
}
