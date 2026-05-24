package server

import (
	"path/filepath"
	"regexp"
)

// Bounds applied to tool arguments. These are intentionally liberal:
// real-world usage (large terminals, long-running waits) stays well
// within range. Anything outside these bounds is almost certainly a
// buggy or hostile caller — accepting it risks tmux allocating massive
// pty buffers or pinning a goroutine for hours.
const (
	// Session name limits — tmux silently misbehaves on absurd names
	// and on characters its own command parser treats specially.
	maxSessionNameLen = 64

	// Terminal size bounds. Real terminals top out well before 1000
	// columns; values like 999999 force tmux to allocate huge pty
	// buffers.
	minWidth  = 20
	maxWidth  = 1000
	minHeight = 5
	maxHeight = 500

	// Time-based bounds in milliseconds. 10 minutes is the longest
	// any single tool call should ever pin a goroutine waiting for a
	// terminal to settle.
	maxDurationMs = 600000
)

// sessionNameRE matches names that are safe to pass to tmux. tmux is
// fussy about session names — colons start a target spec, dots split
// pane indices — so we restrict to a conservative alnum/underscore/dash
// set.
var sessionNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// validateSessionName enforces the conservative session-name policy.
func validateSessionName(name string) *rpcError {
	if name == "" {
		return invalidParams("session name required")
	}
	if len(name) > maxSessionNameLen {
		return invalidParams("session name length %d out of range [1..%d]", len(name), maxSessionNameLen)
	}
	if !sessionNameRE.MatchString(name) {
		return invalidParams("session name %q must match %s", name, sessionNameRE.String())
	}
	return nil
}

// validateSessionRef enforces the same policy for a session reference
// (i.e. the "session" field on tools that operate on an existing
// session). Same rules as validateSessionName but with a field-name
// suffix on errors so callers can tell which arg was wrong.
func validateSessionRef(session string) *rpcError {
	if session == "" {
		return invalidParams("session required")
	}
	if len(session) > maxSessionNameLen {
		return invalidParams("session length %d out of range [1..%d]", len(session), maxSessionNameLen)
	}
	if !sessionNameRE.MatchString(session) {
		return invalidParams("session %q must match %s", session, sessionNameRE.String())
	}
	return nil
}

// validateWidth enforces the terminal-width bounds. Width 0 is
// permitted because callers may rely on the default; the default is
// applied downstream in the handler.
func validateWidth(width int) *rpcError {
	if width == 0 {
		return nil
	}
	if width < minWidth || width > maxWidth {
		return invalidParams("width %d out of range [%d..%d]", width, minWidth, maxWidth)
	}
	return nil
}

// validateHeight enforces the terminal-height bounds. Height 0 is
// permitted because callers may rely on the default; the default is
// applied downstream in the handler.
func validateHeight(height int) *rpcError {
	if height == 0 {
		return nil
	}
	if height < minHeight || height > maxHeight {
		return invalidParams("height %d out of range [%d..%d]", height, minHeight, maxHeight)
	}
	return nil
}

// validateResizeDims enforces the bounds for resize, where width and
// height are required and zero is not a valid input.
func validateResizeDims(width, height int) *rpcError {
	if width < minWidth || width > maxWidth {
		return invalidParams("width %d out of range [%d..%d]", width, minWidth, maxWidth)
	}
	if height < minHeight || height > maxHeight {
		return invalidParams("height %d out of range [%d..%d]", height, minHeight, maxHeight)
	}
	return nil
}

// validateCwd allows an empty cwd (use default) but rejects anything
// non-absolute. Relative paths would be resolved against the server's
// own cwd, which is rarely what the caller actually wants.
func validateCwd(cwd string) *rpcError {
	if cwd == "" {
		return nil
	}
	if !filepath.IsAbs(cwd) {
		return invalidParams("cwd %q must be an absolute path", cwd)
	}
	return nil
}

// validateDurationMs enforces the upper bound on any *_ms arg.
// Negative values are also rejected so the caller can't smuggle in a
// negative timeout that the int→time.Duration conversion would
// silently wrap.
func validateDurationMs(name string, ms int) *rpcError {
	if ms < 0 || ms > maxDurationMs {
		return invalidParams("%s %d out of range [0..%d]", name, ms, maxDurationMs)
	}
	return nil
}
