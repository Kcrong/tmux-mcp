package tmuxctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestSourceFile_LoadsOptions runs the happy path: write a tmux.conf
// that sets a sentinel server-wide option, anchor a session so the
// tmux server is up, source the file, then read the option back via
// ShowOptions. The sentinel value (escape-time 17) is deliberately
// different from tmux's default (500) so an accidental "default
// matched the assertion" pass is impossible.
func TestSourceFile_LoadsOptions(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the tmux server is definitely up;
	// source-file against a server-less socket reports "error
	// connecting" rather than re-loading the conf.
	if err := c.CreateSession(ctx, SessionSpec{Name: "src", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	dir := t.TempDir()
	conf := filepath.Join(dir, "tmux.conf")
	if err := os.WriteFile(conf, []byte("set -g escape-time 17\n"), 0o600); err != nil {
		t.Fatalf("write conf: %v", err)
	}

	if err := c.SourceFile(ctx, conf, false); err != nil {
		t.Fatalf("SourceFile: %v", err)
	}

	options, err := c.ShowOptions(ctx, OptionScopeServer, "", "", false)
	if err != nil {
		t.Fatalf("ShowOptions(server): %v", err)
	}
	got, ok := options["escape-time"]
	if !ok {
		t.Fatalf("escape-time missing from server options: %v", options)
	}
	if got != "17" {
		t.Fatalf("escape-time = %q, want %q (source-file did not reload)", got, "17")
	}
}

// TestSourceFile_MissingPathWrapsSentinel pins the typed-error contract
// for an unknown file: callers (and the JSON-RPC layer) must be able
// to errors.Is into errs.ErrSessionNotFound regardless of which exact
// phrase tmux emitted ("no such file", "can't open"). Same convention
// the rest of the controller upholds for "named thing does not exist".
func TestSourceFile_MissingPathWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, file
	// missing" rather than "no server" (different stderr shape).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	missing := filepath.Join(t.TempDir(), "definitely-not-there.conf")
	err := c.SourceFile(ctx, missing, false)
	if err == nil {
		t.Fatal("expected error for missing config")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestSourceFile_QuietSwallowsMissing pins the load-bearing contract
// for `-q`: with quiet=true tmux suppresses the "no such file"
// non-fatal error, so the controller surfaces no error either. Without
// this, an agent that wants "best-effort reload" would have to inspect
// the message text — which is exactly what the typed-error contract
// is meant to avoid.
func TestSourceFile_QuietSwallowsMissing(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so the tmux server is up — `-q`
	// against a server-less socket would still surface the "error
	// connecting" failure, which is a different code path.
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	missing := filepath.Join(t.TempDir(), "definitely-not-there.conf")
	if err := c.SourceFile(ctx, missing, true); err != nil {
		t.Fatalf("SourceFile(quiet=true) returned %v; want nil so agents can do best-effort reload", err)
	}
}

// TestSourceFile_RejectsEmptyPath locks the up-front guard. tmux would
// otherwise treat "" as a positional arg and emit a confusing error;
// rejecting at the boundary keeps the diagnostic close to the bug.
func TestSourceFile_RejectsEmptyPath(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.SourceFile(ctx, "", false)
	if err == nil {
		t.Fatal("expected error for empty path")
	}
	if !strings.Contains(err.Error(), "path required") {
		t.Fatalf("unexpected error: %v", err)
	}
}
