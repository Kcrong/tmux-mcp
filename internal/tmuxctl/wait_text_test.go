package tmuxctl

import (
	"context"
	"errors"
	"regexp/syntax"
	"strings"
	"testing"
	"time"
)

// TestWaitForText_InvalidRegexReturnsImmediately locks down the contract
// that the user's pattern is compiled once, before the polling loop runs.
// If a regression moved regexp.Compile inside the loop, an invalid
// pattern would surface only after the first Capture call — which on a
// Controller with no real tmux binary would produce an exec error
// instead of the typed "invalid regex" error users expect.
func TestWaitForText_InvalidRegexReturnsImmediately(t *testing.T) {
	t.Parallel()
	// A bogus binary path guarantees Capture would fail loudly if it
	// were ever reached. The regex check must short-circuit first.
	c := &Controller{bin: "/nonexistent/tmux/binary", socket: "/tmp/does-not-matter"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := c.WaitForText(ctx, "ignored", `[unterminated`, 50*time.Millisecond, 5*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
	if !strings.Contains(err.Error(), "invalid regex") {
		t.Fatalf("error %q must contain \"invalid regex\" (regression: compile may have moved into the loop)", err)
	}
	// The wrapped error must still be a regexp/syntax.Error so callers
	// can introspect it via errors.As.
	var syntaxErr *syntax.Error
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("error %T does not unwrap to *regexp/syntax.Error", err)
	}
	// Compile is a synchronous, in-process check — must return well
	// before a single poll step would have elapsed.
	if elapsed >= 50*time.Millisecond {
		t.Fatalf("invalid-regex error took %s (>= step); compile likely happened inside the polling loop", elapsed)
	}
}

func TestWaitForText_EmptyPatternReturnsImmediately(t *testing.T) {
	t.Parallel()
	c := &Controller{bin: "/nonexistent/tmux/binary", socket: "/tmp/does-not-matter"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.WaitForText(ctx, "ignored", "", 50*time.Millisecond, 5*time.Second)
	if err == nil {
		t.Fatal("expected error for empty pattern")
	}
	if !strings.Contains(err.Error(), "pattern required") {
		t.Fatalf("unexpected error: %v", err)
	}
}
