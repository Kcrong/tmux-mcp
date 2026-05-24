package tmuxctl

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TestListClients_EmptyOnFreshController is the load-bearing
// "no clients attached" path: a controller that just spun up its tmux
// server and hasn't been attached to is the common case for the
// headless servers tmux-mcp owns. The contract is "empty stdout means
// zero clients", not "an error" — agents would otherwise have to
// substring-match tmux stderr to tell the cases apart.
func TestListClients_EmptyOnFreshController(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "lc", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	clients, err := c.ListClients(ctx, "")
	if err != nil {
		t.Fatalf("ListClients: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("expected zero clients on a fresh controller, got %d (%+v)", len(clients), clients)
	}
}

// TestListClients_EmptyForUnattachedSession exercises the "session
// scoped, no clients" path: a session that exists but has nothing
// attached to it must still surface as a clean empty list rather than
// an error — symmetric to the server-wide empty case above.
func TestListClients_EmptyForUnattachedSession(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	if err := c.CreateSession(ctx, SessionSpec{Name: "una", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	clients, err := c.ListClients(ctx, "una")
	if err != nil {
		t.Fatalf("ListClients: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("expected zero clients for an unattached session, got %d (%+v)", len(clients), clients)
	}
}

// TestListClients_MissingSessionWrapsSentinel pins the typed-error
// contract for an unknown session: callers (and the JSON-RPC layer)
// must be able to errors.Is into errs.ErrSessionNotFound regardless of
// which exact phrase tmux emitted ("can't find session" vs "can't find
// window") so the dispatcher can map it uniformly to
// CodeSessionNotFound.
func TestListClients_MissingSessionWrapsSentinel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	c := newCtl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	// Anchor with a real session so we exercise "server up, session
	// missing" rather than "no server" (different stderr shape).
	if err := c.CreateSession(ctx, SessionSpec{Name: "anchor", Command: "/bin/sh"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	_, err := c.ListClients(ctx, "ghost_session_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !errors.Is(err, errs.ErrSessionNotFound) {
		t.Fatalf("error %v does not wrap errs.ErrSessionNotFound", err)
	}
}

// TestParseClientLine_BadFieldCount keeps the format-string parser
// honest — drift between listClientsFormat and parseClientLine should
// not silently produce zero-valued ClientInfos.
func TestParseClientLine_BadFieldCount(t *testing.T) {
	t.Parallel()
	if _, err := parseClientLine("only|three|fields"); err == nil {
		t.Fatal("expected error when row has too few fields")
	}
}

// TestParseClientLine_HappyPath round-trips a synthetic row through
// the parser and confirms every field decodes cleanly. This pins the
// listClientsFormat ordering: a swap inside the format string that
// goes unnoticed by the integration test would still trip this one.
func TestParseClientLine_HappyPath(t *testing.T) {
	t.Parallel()
	// Build a representative row: TTY path, session name, term name,
	// width, height, readonly=0, and a fixed seconds-since-epoch
	// timestamp. Use a value with a trailing space on the readonly
	// field so the TrimSpace path is exercised too.
	const epoch = int64(1_700_000_000)
	line := "/dev/pts/3|demo|xterm-256color|120|40|0 |" + "1700000000"
	got, err := parseClientLine(line)
	if err != nil {
		t.Fatalf("parseClientLine: %v", err)
	}
	if got.TTY != "/dev/pts/3" {
		t.Errorf("TTY = %q, want /dev/pts/3", got.TTY)
	}
	if got.Session != "demo" {
		t.Errorf("Session = %q, want demo", got.Session)
	}
	if got.Term != "xterm-256color" {
		t.Errorf("Term = %q, want xterm-256color", got.Term)
	}
	if got.Width != 120 {
		t.Errorf("Width = %d, want 120", got.Width)
	}
	if got.Height != 40 {
		t.Errorf("Height = %d, want 40", got.Height)
	}
	if got.ReadOnly {
		t.Errorf("ReadOnly = true, want false")
	}
	if want := time.Unix(epoch, 0).UTC(); !got.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %s, want %s", got.CreatedAt, want)
	}
}

// TestParseClientLine_ReadOnlyFlag checks the readonly=1 branch so a
// future regression where every client looks read-write is loud.
func TestParseClientLine_ReadOnlyFlag(t *testing.T) {
	t.Parallel()
	got, err := parseClientLine("/dev/pts/4|demo|xterm|80|24|1|1700000000")
	if err != nil {
		t.Fatalf("parseClientLine: %v", err)
	}
	if !got.ReadOnly {
		t.Fatal("ReadOnly = false, want true for readonly=1 row")
	}
}

// TestParseClientLine_BadTimestamp guards the parsing path: a non-
// numeric value in the client_created column must surface as an
// error rather than silently producing a zero-time ClientInfo.
func TestParseClientLine_BadTimestamp(t *testing.T) {
	t.Parallel()
	_, err := parseClientLine("/dev/pts/4|demo|xterm|80|24|0|notanint")
	if err == nil {
		t.Fatal("expected error for non-numeric client_created")
	}
}
