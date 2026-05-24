// Package errs centralises the typed errors that tmux-mcp surfaces to MCP
// clients, together with the JSON-RPC error codes used to represent them
// on the wire. Sentinel errors are wrapped (via fmt.Errorf %w) at the
// site where the failure originates; the dispatcher then maps them with
// CodeOf so callers can switch on a stable code instead of substring-
// matching free-form messages.
package errs

import (
	"context"
	"errors"
)

// Stable JSON-RPC error codes. The first two are JSON-RPC 2.0 standard
// (see https://www.jsonrpc.org/specification#error_object); the rest sit
// in the -32000…-32099 server-error band reserved for the implementation
// and MUST remain constant for the lifetime of the server so clients can
// rely on them.
const (
	// CodeInvalidParams is the JSON-RPC standard code for malformed params.
	CodeInvalidParams = -32602
	// CodeInternal is the JSON-RPC standard code for an unspecified server failure.
	CodeInternal = -32603
	// CodeSessionNotFound is returned when a request names a tmux session
	// that this controller does not know about.
	CodeSessionNotFound = -32000
	// CodeTmuxVersionUnsupported is returned when the local tmux binary is
	// older than the minimum this server supports.
	CodeTmuxVersionUnsupported = -32001
	// CodeTimeout is returned when a wait_* tool exceeds its deadline.
	CodeTimeout = -32002
	// CodeContextCancelled is returned when the caller cancels the context
	// (or it hits a deadline) mid-call.
	CodeContextCancelled = -32003
	// CodeSessionExists is returned when a request would create or
	// rename to a tmux session name that is already in use on this
	// controller. Distinct from CodeSessionNotFound (the existing
	// sentinel for "name does not exist") so clients can branch on the
	// collision case explicitly.
	CodeSessionExists = -32004
	// CodePaneActive is returned when a request to respawn a pane (via
	// `respawn_pane`) targets a pane whose original command is still
	// running and the caller did not pass `kill=true`. Distinct from
	// CodeInvalidParams so clients can recognise "the request itself
	// was well-formed but the pane is busy" and retry with kill=true,
	// rather than treating it as a malformed-arguments problem.
	CodePaneActive = -32005
	// CodeOversizedResponse is returned when a handler's marshalled
	// JSON-RPC response body exceeds the cap configured via the server's
	// -max-response-bytes flag. The dispatcher replaces the original
	// payload with a typed error carrying this code so a misbehaving
	// tool (e.g. capture_pane on a 10MB scrollback) cannot dump a
	// multi-megabyte frame onto a client whose reader can't tolerate it.
	// Distinct from CodeInternal so clients can recognise "the call
	// succeeded but the answer was too big" instead of conflating it
	// with an unspecified server failure.
	CodeOversizedResponse = -32010
	// CodeReadOnly is returned when a tools/call names a tool that
	// mutates tmux state and the server was started with the -read-only
	// flag. The dispatcher rejects the call before any handler runs and
	// replies with this code so an LLM agent constrained to inspection
	// can branch on a stable signal instead of substring-matching the
	// rejection message. Distinct from CodeMethodNotFound (the
	// -allowlist guard's code) so operators can tell "the tool was
	// filtered out by policy" from "the tool exists but the server
	// refuses to mutate state right now".
	CodeReadOnly = -32011
)

// Sentinel errors. Wrap them with fmt.Errorf("%w: ...", err) at the call
// site; downstream code should always use errors.Is to detect them.
var (
	// ErrSessionNotFound signals that a named session does not exist.
	ErrSessionNotFound = errors.New("session not found")
	// ErrTmuxVersionUnsupported signals that the tmux binary on PATH is
	// too old for this server.
	ErrTmuxVersionUnsupported = errors.New("tmux version unsupported")
	// ErrTimeout signals that a polling wait exceeded its deadline.
	ErrTimeout = errors.New("timeout")
	// ErrSessionExists signals that a tmux session name collides with an
	// existing one — typically surfaced by session_rename when the
	// requested new name is already taken on this controller.
	ErrSessionExists = errors.New("session already exists")
	// ErrOversizedResponse signals that a handler's marshalled JSON-RPC
	// response exceeded the configured -max-response-bytes ceiling. The
	// dispatcher itself synthesises this — handlers do not return it —
	// but it is exported as a sentinel so audit / metrics consumers can
	// recognise the oversize case via [errors.Is] instead of substring-
	// matching the message.
	ErrOversizedResponse = errors.New("response body exceeds max-response-bytes")
	// ErrReadOnly signals that a tools/call was rejected because the
	// server is running with -read-only and the named tool mutates tmux
	// state. The dispatcher synthesises it before any handler runs;
	// handlers themselves never return it. Audit and metrics consumers
	// can recognise the rejection via [errors.Is] instead of substring-
	// matching the message.
	ErrReadOnly = errors.New("server in read-only mode")
	// ErrPaneActive signals that a `respawn_pane` request targeted a
	// pane whose original command is still running, while the caller
	// did not opt into the kill-then-respawn behaviour with `kill=true`.
	// tmux returns this (with stderr "pane <target> still active")
	// instead of forcibly stopping the foreground process — the typed
	// sentinel lets the JSON-RPC layer surface a stable code so clients
	// can recognise the case and re-issue the call with kill=true.
	ErrPaneActive = errors.New("pane still active")
)

// CodeOf returns the JSON-RPC error code that best describes err. It
// recognises every sentinel in this package plus context.Canceled /
// context.DeadlineExceeded; anything else falls back to CodeInternal so
// existing JSON-RPC behaviour is preserved.
func CodeOf(err error) int {
	switch {
	case err == nil:
		return CodeInternal
	case errors.Is(err, ErrSessionNotFound):
		return CodeSessionNotFound
	case errors.Is(err, ErrSessionExists):
		return CodeSessionExists
	case errors.Is(err, ErrTmuxVersionUnsupported):
		return CodeTmuxVersionUnsupported
	case errors.Is(err, ErrTimeout):
		return CodeTimeout
	case errors.Is(err, ErrOversizedResponse):
		return CodeOversizedResponse
	case errors.Is(err, ErrReadOnly):
		return CodeReadOnly
	case errors.Is(err, ErrPaneActive):
		return CodePaneActive
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return CodeContextCancelled
	}
	return CodeInternal
}
