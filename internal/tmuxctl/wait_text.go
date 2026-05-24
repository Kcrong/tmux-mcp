package tmuxctl

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// TextMatch describes where pattern matched the captured pane.
type TextMatch struct {
	Match    string // the matched substring returned by the regex.
	Snapshot string // the full visible pane at the moment of the match.
}

// WaitForText polls the visible pane until pattern matches, or until
// timeout elapses. step controls the poll interval.
func (c *Controller) WaitForText(ctx context.Context, session, pattern string, step, timeout time.Duration) (TextMatch, error) {
	if pattern == "" {
		return TextMatch{}, fmt.Errorf("pattern required")
	}
	// Compile once per call, before the polling loop. Recompiling on
	// every iteration would be wasted work for any non-trivial regex
	// and a measurable hot path on long timeouts. An invalid pattern
	// must also surface immediately, not after the first poll.
	re, err := regexp.Compile(pattern)
	if err != nil {
		return TextMatch{}, fmt.Errorf("invalid regex %q: %w", pattern, err)
	}
	if step <= 0 {
		step = 100 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for {
		body, err := c.Capture(ctx, session, CaptureVisible, false)
		if err != nil {
			return TextMatch{}, err
		}
		if m := re.FindString(body); m != "" {
			return TextMatch{Match: m, Snapshot: body}, nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			// Wrap ErrTimeout so the JSON-RPC layer can surface
			// CodeTimeout (-32002). The legacy "not found within"
			// substring is preserved for log/grep compatibility.
			return TextMatch{Snapshot: body}, fmt.Errorf("wait_for_text: pattern %q not found within %s: %w", pattern, timeout, errs.ErrTimeout)
		}
		select {
		case <-ctx.Done():
			return TextMatch{Snapshot: body}, ctx.Err()
		case <-time.After(step):
		}
	}
}
