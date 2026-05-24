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
	case errors.Is(err, ErrTmuxVersionUnsupported):
		return CodeTmuxVersionUnsupported
	case errors.Is(err, ErrTimeout):
		return CodeTimeout
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return CodeContextCancelled
	}
	return CodeInternal
}
