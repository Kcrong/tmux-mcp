//go:build windows

package tmuxctl

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestSendSignal_RejectsUserSignalsOnWindows pins the Windows-only
// behaviour: SIGUSR1 / SIGUSR2 are POSIX-only constants, so we
// surface a friendly "not supported on Windows" diagnostic rather
// than the generic whitelist-mismatch error (which would lie, since
// SignalNames() still lists USR1/USR2 to keep the JSON-RPC schema
// uniform across platforms).
//
// Lives behind a `//go:build windows` tag because Linux/macOS
// Controller.SendSignal accepts USR1/USR2 normally — they would not
// be rejected there, so the assertion would not hold.
func TestSendSignal_RejectsUserSignalsOnWindows(t *testing.T) {
	t.Parallel()
	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Shutdown(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, name := range []string{"USR1", "USR2"} {
		err := c.SendSignal(ctx, "any-session", name)
		if err == nil {
			t.Fatalf("SendSignal(%q) returned nil, want a not-supported error", name)
		}
		if !strings.Contains(err.Error(), "not supported on Windows") {
			t.Fatalf("SendSignal(%q) error = %v, want it to mention Windows unsupport", name, err)
		}
	}
}
