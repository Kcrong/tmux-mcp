// Package server implements a stdio MCP server exposing tmux control.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// JSON-RPC 2.0 framing-level error codes. These cover failures detected
// before a method handler runs (parse/dispatch errors). Codes for handler
// failures live in internal/errs and are stable across the server's life.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	// codeInvalidParams and codeInternalError are kept here as aliases of
	// the canonical constants in internal/errs so the dispatcher and the
	// rest of the server can keep using the short names while sharing the
	// same underlying values.
	codeInvalidParams = errs.CodeInvalidParams
	codeInternalError = errs.CodeInternal
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Handler dispatches a single request and returns either a result or an
// error. A nil result is allowed (used for notifications).
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, *rpcError)

// Serve runs the JSON-RPC dispatch loop on a line-delimited reader and
// writer until the reader hits EOF or the context is cancelled.
func Serve(ctx context.Context, in io.Reader, out io.Writer, h Handler) error {
	r := bufio.NewReader(in)
	var writeMu sync.Mutex
	// wg tracks every dispatched handler goroutine so Serve can hold
	// shutdown until all in-flight calls have written their response.
	var wg sync.WaitGroup
	send := func(resp rpcResponse) {
		resp.JSONRPC = "2.0"
		buf, err := json.Marshal(resp)
		if err != nil {
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_, _ = out.Write(buf)
		_, _ = out.Write([]byte{'\n'})
	}
	for {
		if ctx.Err() != nil {
			// Drain in-flight handlers before surfacing cancellation so
			// the caller's tmux/process teardown can't race their writes.
			wg.Wait()
			return ctx.Err()
		}
		line, err := r.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				wg.Wait()
				return nil
			}
			wg.Wait()
			return err
		}
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if jerr := json.Unmarshal(line, &req); jerr != nil {
			slog.Warn("invalid request", "err", jerr)
			send(rpcResponse{Error: &rpcError{Code: codeParseError, Message: jerr.Error()}})
			continue
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			slog.Warn("invalid request", "method", req.Method, "jsonrpc", req.JSONRPC)
			send(rpcResponse{ID: req.ID, Error: &rpcError{Code: codeInvalidRequest, Message: "expected jsonrpc=2.0 with method"}})
			continue
		}
		// Generate a server-side request id and attach it (alongside
		// the method name) to a request-scoped logger. Every log line
		// emitted from the request path — here and inside Handler —
		// carries "rid" so operators can stitch concurrent requests
		// back together across goroutines.
		rid := newRequestID()
		reqLogger := slog.Default().With("rid", rid, "method", req.Method)
		reqCtx := WithLogger(ctx, reqLogger)
		reqLogger.Debug("rpc start", "id", string(req.ID))
		// Dispatch each request on its own goroutine so a slow tool call
		// doesn't block other traffic on the same stdio pipe.
		wg.Add(1)
		go func(req rpcRequest, reqCtx context.Context, reqLogger *slog.Logger) {
			// wg.Done is registered first so that, in defer-LIFO order,
			// the recovery defer below executes *before* wg.Done — i.e.
			// recover() runs, the error reply is written, and only then
			// is the WaitGroup released. This guarantees Shutdown's
			// wg.Wait() observes a fully-handled request even when the
			// handler panics.
			defer wg.Done()
			// Recover from any panic raised inside the user-supplied
			// Handler. Without this, a panic would (a) skip wg.Done and
			// hang Shutdown, and (b) deny the client any response.
			// We log the panic + stack to stderr at error level for
			// operators and reply with a generic "internal server error"
			// so we never leak Go internals (stack frames, panic value)
			// to the JSON-RPC client.
			defer func() {
				r := recover()
				if r == nil {
					return
				}
				reqLogger.Error("handler panic",
					"panic", fmt.Sprintf("%v", r),
					"stack", string(debug.Stack()),
				)
				// Notifications (no id) don't expect a response, even
				// on panic.
				if len(req.ID) == 0 {
					return
				}
				send(rpcResponse{
					ID:    req.ID,
					Error: &rpcError{Code: codeInternalError, Message: "internal server error"},
				})
			}()
			started := time.Now()
			result, rerr := h(reqCtx, req.Method, req.Params)
			durMs := time.Since(started).Milliseconds()
			// Notifications have no id field; they get no response.
			if len(req.ID) == 0 {
				reqLogger.Debug("rpc end", "dur_ms", durMs, "notification", true)
				return
			}
			if rerr != nil {
				reqLogger.Warn("rpc error", "code", rerr.Code, "message", rerr.Message, "dur_ms", durMs)
				send(rpcResponse{ID: req.ID, Error: rerr})
				return
			}
			reqLogger.Debug("rpc end", "dur_ms", durMs)
			send(rpcResponse{ID: req.ID, Result: result})
		}(req, reqCtx, reqLogger)
	}
}

// invalidParams builds a typed JSON-RPC error for malformed params.
func invalidParams(format string, args ...any) *rpcError {
	return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

// internalError builds a typed JSON-RPC error wrapping an upstream
// failure (tmux exit, regex error, etc.). The wire code is selected by
// errs.CodeOf so known sentinels (session not found, timeout, ...) get
// stable codes while everything else falls back to -32603.
func internalError(err error) *rpcError {
	return &rpcError{Code: errs.CodeOf(err), Message: err.Error()}
}

// methodNotFound for unsupported MCP methods.
func methodNotFound(method string) *rpcError {
	return &rpcError{Code: codeMethodNotFound, Message: "method not found: " + method}
}
