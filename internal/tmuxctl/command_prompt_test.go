package tmuxctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestCommandPrompt_HeadlessNoOp pins the headless idiom: a freshly
// constructed controller has no attached client, so `tmux
// command-prompt` (without an explicit `-t`) trips the "no current
// client" stderr path. We return nil there so the JSON-RPC layer can
// emit a successful no-op rather than a noisy error every call. Anchor
// the server with a real session first so we hit the "server up, no
// client attached" branch (a fresh controller without any session can
// produce a different "no server running" surface).
func TestCommandPrompt_HeadlessNoOp(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "cp_headless", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// No client pinned → tmux replies "no current client"; the method
	// must absorb that into a nil return.
	if err := c.CommandPrompt(ctx, "", "", "", "rename-window %%", false, false, false); err != nil {
		t.Fatalf("CommandPrompt headless: got %v, want nil (no-current-client must be a no-op)", err)
	}
}

// TestCommandPrompt_MissingClientWrapsSentinel pins the typed-error
// contract for an explicit-but-unknown client target. tmux phrases
// this as "can't find client" — the wrapper must surface it as
// errs.ErrSessionNotFound so the JSON-RPC dispatcher can map it to
// CodeSessionNotFound the same way display_message / list_clients do
// for missing windows / panes.
func TestCommandPrompt_MissingClientWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor the server so "no server running" is not what we hit.
	if err := c.CreateSession(ctx, SessionSpec{Name: "cp_anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// /dev/null is a real path but not a tmux client TTY, so tmux
	// emits "can't find client".
	err := c.CommandPrompt(ctx, "/dev/null", "", "", "rename-window %%", false, false, false)
	if err == nil {
		t.Fatal("expected error for missing client")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestCommandPrompt_RejectsNULInTemplate guards the defensive in-method
// NUL check. The boundary layer already strips control bytes, but a
// programmatic caller bypassing it should not be able to smuggle an
// argv-truncating byte through to tmux.
func TestCommandPrompt_RejectsNULInTemplate(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)

	err := c.CommandPrompt(ctx, "", "", "", "rename-window\x00 %%", false, false, false)
	if err == nil {
		t.Fatal("expected error for NUL in template")
	}
	if !strings.Contains(err.Error(), "must not contain NUL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestCommandPrompt_RejectsNULInPrompts mirrors the template guard for
// the prompts field. The defensive check covers every string arg, so
// pinning two of them keeps the inverse drift loud.
func TestCommandPrompt_RejectsNULInPrompts(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)

	err := c.CommandPrompt(ctx, "", "name\x00:", "", "rename-window %%", false, false, false)
	if err == nil {
		t.Fatal("expected error for NUL in prompts")
	}
	if !strings.Contains(err.Error(), "must not contain NUL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestIsCmdPromptNoClientMsg keeps the matcher honest: the substring
// must trigger on tmux's documented "no current client" phrase
// (case-insensitively) and reject unrelated stderr text.
func TestIsCmdPromptNoClientMsg(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		msg   string
		match bool
	}{
		{"exact lowercase", "tmux command-prompt: no current client", true},
		{"with surrounding text", "error: no current client; bailing", true},
		{"capitalised", "No current client present", true},
		{"unrelated", "can't find client: /dev/null", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isCmdPromptNoClientMsg(tc.msg); got != tc.match {
				t.Errorf("isCmdPromptNoClientMsg(%q) = %v, want %v", tc.msg, got, tc.match)
			}
		})
	}
}
