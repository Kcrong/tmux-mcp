package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
)

// loggerKey is a private context key under which a request-scoped
// *slog.Logger is stashed. Using an unexported type prevents collisions
// with context keys defined elsewhere in the program.
type loggerKey struct{}

// WithLogger returns a derived context carrying the supplied logger.
// Handlers running in a per-request goroutine should pull the logger
// back out via LoggerFrom and use it for every line they emit so that
// the request id ("rid") is consistently attached.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, loggerKey{}, l)
}

// LoggerFrom retrieves the logger previously stashed by WithLogger. It
// falls back to slog.Default() when no logger is attached, so callers
// never need to nil-check.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if ctx == nil {
		return slog.Default()
	}
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// newRequestID returns 8 hex chars (4 random bytes) suitable for
// correlating one JSON-RPC request across goroutines. It deliberately
// ignores the client-supplied req.ID — that value is not guaranteed to
// be unique, present, or sortable, and we want a server-generated tag
// that is always safe to log.
func newRequestID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read effectively cannot fail on supported
		// platforms; if it ever does, falling back to a fixed marker
		// keeps logging non-fatal rather than crashing the server.
		return "00000000"
	}
	return hex.EncodeToString(b[:])
}
