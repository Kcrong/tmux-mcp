package server

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
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

// ValidateSessionPrefix is the startup-time check for the
// -session-prefix CLI flag. The empty value disables prefixing and is
// always accepted; otherwise the prefix must satisfy the same
// conservative regex used for session names (no whitespace, no shell
// metachars, no colons or dots that would let a malicious caller break
// out of the prefix into a sibling session). A trailing dash is
// rejected so a name like "agent_alice_-build" can't appear; the
// idiomatic separator is "_".
//
// We also bound the prefix length to leave room for at least a 1-byte
// user-supplied name within the per-session 64-byte ceiling, so the
// runtime-time path (session_create combining prefix+name) cannot
// surprise an operator with rejections their flag value implied.
//
// Returns a fully-formed error suitable for emitting on stderr; main()
// wraps it in a sentinel so the exit code stays 2 ("CLI usage error").
func ValidateSessionPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if len(prefix) >= maxSessionNameLen {
		return fmt.Errorf("session prefix length %d leaves no room for a session name (max %d)",
			len(prefix), maxSessionNameLen-1)
	}
	if !sessionNameRE.MatchString(prefix) {
		return fmt.Errorf("session prefix %q must match %s",
			prefix, sessionNameRE.String())
	}
	if strings.HasSuffix(prefix, "-") {
		// A trailing dash is regex-legal but creates surprising names
		// like "agent--build" when the user-supplied name itself has a
		// leading dash; rejecting it up-front nudges operators toward
		// the conventional "_" separator.
		return fmt.Errorf("session prefix %q must not end with '-'", prefix)
	}
	return nil
}

// validateCombinedSessionName is the per-call check that the prefix +
// user-supplied name still fits within the session-name length budget.
// Without it, an operator picking a long prefix and a client picking a
// long name would silently produce a session that tmux misbehaves on.
// The check fires only when a prefix is configured — the prefix-less
// path keeps the legacy single-name validation unchanged.
func validateCombinedSessionName(prefix, name string) *rpcError {
	if prefix == "" {
		return nil
	}
	total := len(prefix) + len(name)
	if total > maxSessionNameLen {
		return invalidParams(
			"prefixed session name length %d (prefix %q + name %q) exceeds %d",
			total, prefix, name, maxSessionNameLen,
		)
	}
	return nil
}
