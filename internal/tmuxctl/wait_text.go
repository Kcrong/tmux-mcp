package tmuxctl

import (
	"context"
	"fmt"
	"regexp"
	"time"
)

// TextMatch describes where pattern matched the captured pane.
type TextMatch struct {
	Match    string
	Snapshot string
}

// WaitForText polls the visible pane until pattern matches, or until
// timeout elapses. step controls the poll interval.
func (c *Controller) WaitForText(ctx context.Context, session, pattern string, step, timeout time.Duration) (TextMatch, error) {
	if pattern == "" {
		return TextMatch{}, fmt.Errorf("pattern required")
	}
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
			return TextMatch{Snapshot: body}, fmt.Errorf("wait_for_text: pattern %q not found within %s", pattern, timeout)
		}
		select {
		case <-ctx.Done():
			return TextMatch{Snapshot: body}, ctx.Err()
		case <-time.After(step):
		}
	}
}
