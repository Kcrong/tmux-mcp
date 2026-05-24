// Package server implements a stdio MCP server exposing tmux control.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Standard JSON-RPC 2.0 error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
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
			return ctx.Err()
		}
		line, err := r.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if jerr := json.Unmarshal(line, &req); jerr != nil {
			send(rpcResponse{Error: &rpcError{Code: codeParseError, Message: jerr.Error()}})
			continue
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			send(rpcResponse{ID: req.ID, Error: &rpcError{Code: codeInvalidRequest, Message: "expected jsonrpc=2.0 with method"}})
			continue
		}
		// Dispatch each request on its own goroutine so a slow tool call
		// doesn't block other traffic on the same stdio pipe.
		go func(req rpcRequest) {
			result, rerr := h(ctx, req.Method, req.Params)
			// Notifications have no id field; they get no response.
			if len(req.ID) == 0 {
				return
			}
			if rerr != nil {
				send(rpcResponse{ID: req.ID, Error: rerr})
				return
			}
			send(rpcResponse{ID: req.ID, Result: result})
		}(req)
	}
}

// invalidParams builds a typed JSON-RPC error for malformed params.
func invalidParams(format string, args ...any) *rpcError {
	return &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

// internalError builds a typed JSON-RPC error wrapping an upstream
// failure (tmux exit, regex error, etc.).
func internalError(err error) *rpcError {
	return &rpcError{Code: codeInternalError, Message: err.Error()}
}

// methodNotFound for unsupported MCP methods.
func methodNotFound(method string) *rpcError {
	return &rpcError{Code: codeMethodNotFound, Message: "method not found: " + method}
}
