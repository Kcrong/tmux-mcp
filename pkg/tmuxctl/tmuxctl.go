package tmuxctl

import (
	internalctl "github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// Controller drives a private tmux server. It is a type alias for the
// implementation in internal/tmuxctl, which means the full method set
// (CreateSession, KillSession, SendKeys, Capture, WaitForStable,
// WaitForText, ListSessions, HasSession, Resize, Socket, Shutdown,
// ...) is available without further wrapping.
type Controller = internalctl.Controller

// SessionSpec describes a session to create. See [Controller.CreateSession].
type SessionSpec = internalctl.SessionSpec

// CaptureMode selects which area of the pane [Controller.Capture]
// returns: only the visible region, or the visible region plus all
// scrollback.
type CaptureMode = internalctl.CaptureMode

// TextMatch describes where a regex matched the captured pane. Returned
// by [Controller.WaitForText].
type TextMatch = internalctl.TextMatch

// Capture areas re-exported from the internal package so callers do not
// need to import it.
const (
	// CaptureVisible captures only the currently visible region of the pane.
	CaptureVisible = internalctl.CaptureVisible
	// CaptureScrollback captures the visible region plus all scrollback.
	CaptureScrollback = internalctl.CaptureScrollback
)

// New creates a Controller with a private tmux socket under a fresh
// MkdirTemp directory. The tmux server itself is started lazily by the
// first command. Call [Controller.Shutdown] to tear it down.
func New() (*Controller, error) {
	return internalctl.New()
}
