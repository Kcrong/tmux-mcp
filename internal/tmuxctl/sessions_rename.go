package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// isSessionExistsMsg recognises tmux's "duplicate session" stderr from
// `rename-session` (and a handful of related commands across versions).
// Matching by message text is fragile in general, but tmux has used the
// same phrasing for at least a decade and the alternative — running a
// pre-flight has-session probe on every rename — costs an extra IPC
// round-trip on the hot path. Keep the predicate broad so a future
// minor-version drift in punctuation does not regress the mapping.
func isSessionExistsMsg(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "duplicate session")
}

// RenameSession renames an existing tmux session via
// `tmux rename-session -t OLD NEW`. Both names are expected to satisfy
// the boundary's session-name policy (the JSON-RPC layer validates them
// before calling); this method translates the raw tmux failures into
// typed sentinels the dispatcher can map to stable JSON-RPC codes.
//
// Error mapping:
//   - oldName not found: surfaced via run() as a wrapped
//     errs.ErrSessionNotFound (the underlying message is
//     "can't find session: <oldName>" which isSessionMissingMsg already
//     recognises).
//   - newName already in use: surfaced as a wrapped errs.ErrSessionExists
//     so the dispatcher can return CodeSessionExists.
//   - empty oldName / newName: rejected up front as plain errors. The
//     boundary should never let an empty value through, but defending
//     here keeps the controller usable from tests and other callers.
func (c *Controller) RenameSession(ctx context.Context, oldName, newName string) error {
	if oldName == "" {
		return errors.New("old session name required")
	}
	if newName == "" {
		return errors.New("new session name required")
	}
	if _, err := c.run(ctx, "rename-session", "-t", oldName, newName); err != nil {
		// "duplicate session" is the only failure mode that warrants a
		// dedicated wire code — every other path goes through the
		// existing sentinels (session-not-found via run(), generic
		// internal otherwise).
		if isSessionExistsMsg(err.Error()) && !errors.Is(err, errs.ErrSessionExists) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionExists)
		}
		return err
	}
	return nil
}
