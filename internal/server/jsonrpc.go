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

// ServeOption configures Serve. Options compose into a serveConfig
// struct; pass them as variadic trailing args so existing callers that
// don't need to tune anything (`Serve(ctx, in, out, h)`) keep working
// unchanged.
type ServeOption func(*serveConfig)

// serveConfig is the internal bag-of-knobs Serve assembles from its
// ServeOption arguments. Keeping it unexported pins down the public API
// to the option setters — we can grow new knobs (timeouts, max frame
// size, …) without rewriting every test that constructs a server.
type serveConfig struct {
	// maxConcurrentCalls is the cap on simultaneously-executing
	// tools/call frames. <=0 means unbounded (the historical default).
	maxConcurrentCalls int
	// audit is the optional JSONL audit sink. When non-nil, every
	// `tools/call` (success or failure) produces a record. Other
	// JSON-RPC methods are excluded — they are protocol bookkeeping
	// and would dominate the log without adding signal.
	audit *Audit
	// shutdownTimeout caps how long Serve will wait for in-flight
	// handlers to finish writing their responses after the read loop
	// exits (ctx cancel or EOF). The zero value (shutdownTimeoutSet
	// false) preserves the pre-flag behaviour of waiting indefinitely.
	// shutdownTimeoutSet=true with shutdownTimeout=0 means "skip the
	// drain entirely"; any in-flight handlers keep running detached.
	shutdownTimeout    time.Duration
	shutdownTimeoutSet bool
	// reaper, when non-nil, runs in the background and kills sessions
	// that have had no tools/call activity for at least the configured
	// idle timeout. The dispatcher records activity on every
	// session-bearing tools/call. nil disables the feature, matching
	// the -session-idle-timeout=0 default.
	reaper *IdleReaper
	// installListChanged is the hook Serve uses to hand a
	// writeMu-bound emitter to whoever owns tool-surface mutations.
	// The callback receives a parameterless `notify` closure that, on
	// every invocation, writes a single
	// `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`
	// frame through the same writeMu the response path uses — so a
	// notification cannot interleave with a half-written reply. The
	// callback runs once during Serve setup, before the read loop, so
	// the receiver has the emitter in place by the time the client's
	// first frame arrives.
	installListChanged func(notify func())
	// metrics is the optional Prometheus exporter handle. When non-nil
	// the dispatcher records every tools/call (counter + duration
	// histogram) into the collectors it owns. nil keeps the
	// metrics-disabled fast-path: no allocations, no Inc/Observe.
	metrics *Metrics
	// maxResponseBytes caps the marshalled JSON-RPC response body
	// length (excluding the trailing newline) before it is written to
	// stdout. <=0 disables the cap (the historical default). When a
	// reply would exceed the ceiling, the dispatcher replaces it with
	// a typed [errs.CodeOversizedResponse] error so a misbehaving tool
	// (e.g. capture_pane on a 10MB scrollback) cannot dump a
	// multi-megabyte frame onto a client whose reader can't tolerate
	// it.
	maxResponseBytes int64
}

// WithMaxConcurrentCalls caps how many tools/call frames may be
// in-flight simultaneously. Limit <=0 disables the cap (preserving the
// pre-flag behaviour of unbounded goroutines). Other JSON-RPC methods
// (initialize, notifications/initialized, tools/list) are not gated —
// only tools/call frames consume a slot — so the limit reflects "how
// many tool invocations am I willing to run at once" rather than total
// RPC concurrency.
func WithMaxConcurrentCalls(limit int) ServeOption {
	return func(c *serveConfig) { c.maxConcurrentCalls = limit }
}

// WithAudit installs an optional JSONL audit sink. When non-nil, every
// successful or failed `tools/call` produces a record. Other JSON-RPC
// methods (initialize, notifications/initialized, tools/list) are
// intentionally excluded — they are protocol bookkeeping and would
// dominate the log without adding signal. A nil audit handle keeps the
// audit-disabled fast-path (no record emission, no allocations).
func WithAudit(a *Audit) ServeOption {
	return func(c *serveConfig) { c.audit = a }
}

// WithShutdownTimeout caps how long Serve waits for in-flight handlers
// to finish after the read loop exits (ctx cancel or stdin EOF). When
// the timeout fires Serve returns ErrShutdownTimedOut so the caller can
// surface a non-zero exit status. d=0 disables the drain (Serve returns
// immediately, in-flight handlers are abandoned mid-write); negative
// values are treated as 0. Callers that don't apply this option keep
// the historical behaviour of an unbounded drain.
func WithShutdownTimeout(d time.Duration) ServeOption {
	return func(c *serveConfig) {
		if d < 0 {
			d = 0
		}
		c.shutdownTimeout = d
		c.shutdownTimeoutSet = true
	}
}

// WithSessionIdleTimeout enables the background reaper that kills tmux
// sessions that have had no tools/call activity for at least `d`. Pass
// the controller's KillSession method as `kill` so the reaper can
// terminate sessions in the same tmux server the rest of the dispatcher
// is driving. d <= 0 disables the feature entirely (matching the
// -session-idle-timeout=0 CLI default), in which case the reaper
// goroutine is never started and the dispatcher's Touch calls become
// no-ops.
//
// Activity is defined as any tools/call that names a session — the
// session-bearing list is encoded inside [sessionFromArgs]. Methods
// that operate on the table as a whole (session_list,
// kill_all_sessions) are deliberately excluded so they cannot keep
// otherwise-idle sessions alive.
func WithSessionIdleTimeout(d time.Duration, kill KillFunc) ServeOption {
	return func(c *serveConfig) { c.reaper = NewIdleReaper(d, kill) }
}

// ErrShutdownTimedOut is returned by Serve when the in-flight drain
// fails to complete inside the timeout configured via
// WithShutdownTimeout. Callers can use [errors.Is] to recognise it and
// map it to a non-zero exit code.
var ErrShutdownTimedOut = errors.New("shutdown drain timed out")

// WithMaxResponseBytes caps the marshalled JSON-RPC response body length
// (excluding the trailing newline) the dispatcher will write to stdout.
// Limit <=0 disables the cap (preserving the pre-flag behaviour of
// streaming whatever the handler produced). When a reply exceeds the
// ceiling, the dispatcher replaces it with a typed JSON-RPC error
// carrying [errs.CodeOversizedResponse] and a message of the form
// "response body N bytes exceeds max-response-bytes M" so the client
// gets a structured signal instead of a truncated payload. The
// underlying tools/call still ran — its audit / metrics records are
// emitted with the oversize sentinel as the error so operators can
// distinguish "the tool failed" from "the answer was too big".
// Notifications (no id) get nothing, same as today.
func WithMaxResponseBytes(limit int64) ServeOption {
	return func(c *serveConfig) { c.maxResponseBytes = limit }
}

// WithToolsListChangedNotifier wires the spec-defined
// notifications/tools/list_changed surface. Serve runs setter once,
// before the read loop starts, with a parameterless emitter that
// pushes a single
// `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`
// frame through the same writeMu the response path uses. Pass
// e.g. `WithToolsListChangedNotifier(tools.SetNotifier)` so a
// runtime RegisterTool / UnregisterTool call automatically pushes a
// spec-compliant frame to the client.
//
// Passing a nil setter is a no-op so callers that only sometimes
// care about the notifier (e.g. tests) don't have to branch.
func WithToolsListChangedNotifier(setter func(notify func())) ServeOption {
	return func(c *serveConfig) { c.installListChanged = setter }
}

// Serve runs the JSON-RPC dispatch loop on a line-delimited reader and
// writer until the reader hits EOF or the context is cancelled.
//
// Shutdown semantics: when ctx is cancelled (typically because main
// caught SIGTERM), Serve stops dispatching new tools/call frames and
// instead replies to any newly-arrived request with -32603
// "shutting down". It then waits for previously-dispatched handlers to
// finish — bounded by [WithShutdownTimeout] (default: unbounded) — so
// in-flight responses get a chance to land on the wire before the
// process exits.
//
// Client disconnect (EOF on stdin) is treated like ctx cancellation for
// the purpose of in-flight handlers: the request-scoped context Serve
// passes to each Handler invocation is derived from a child context
// that is cancelled both on parent ctx.Done() and on stdin EOF. A
// long-running poll loop (e.g. wait_for_text with timeout_ms=10000)
// that watches its own request ctx therefore exits within one poll
// step of the disconnect, instead of running until its timeout fires
// — there is nobody on the other side to receive the eventual
// response. The drain still bounds Serve's return; cancelling the
// dispatch ctx just unblocks handlers that were watching it.
func Serve(ctx context.Context, in io.Reader, out io.Writer, h Handler, opts ...ServeOption) error {
	var cfg serveConfig
	for _, o := range opts {
		o(&cfg)
	}
	limiter := newCallLimiter(cfg.maxConcurrentCalls)
	audit := cfg.audit
	reaper := cfg.reaper
	metrics := cfg.metrics
	r := bufio.NewReader(in)
	var writeMu sync.Mutex
	// dispatchCtx is the parent of every request-scoped context Serve
	// hands to a Handler. It is cancelled on either parent ctx.Done()
	// (signal-driven shutdown) or stdin EOF / read error (client
	// disconnect). Without this child context, a `wait_for_text`
	// polling loop with a 10s timeout would keep running for the full
	// 10s after the client closed the pipe, even though no response
	// can ever land on the wire — burning CPU and tmux IPC for nothing.
	dispatchCtx, cancelDispatch := context.WithCancel(ctx)
	defer cancelDispatch()
	// wg tracks every dispatched handler goroutine so Serve can hold
	// shutdown until all in-flight calls have written their response.
	var wg sync.WaitGroup
	// Launch the idle-session reaper when -session-idle-timeout > 0.
	// Run is a no-op on a nil reaper, but we guard the goroutine launch
	// so an operator who left the flag at the default 0 doesn't pay an
	// extra goroutine. The reaper exits cleanly on ctx.Done(); we
	// deliberately don't track it on wg because the shutdown drain
	// should not have to wait an extra reapInterval tick for the
	// goroutine to wake up — ctx cancel is enough.
	if reaper != nil {
		go reaper.Run(ctx)
	}
	send := func(resp rpcResponse) {
		resp.JSONRPC = "2.0"
		buf, err := json.Marshal(resp)
		if err != nil {
			return
		}
		// Cap the wire-frame length when -max-response-bytes is set.
		// We deliberately measure the marshalled body (excluding the
		// trailing newline) so the limit reflects what actually crosses
		// the pipe — re-marshalling the rpcResponse just to count its
		// fields would diverge from the bytes the client receives.
		// Notifications carry no id, so a synthesised error has nowhere
		// to land; we suppress them entirely (matching the existing
		// "notifications get nothing" contract above).
		if cfg.maxResponseBytes > 0 && int64(len(buf)) > cfg.maxResponseBytes {
			if len(resp.ID) == 0 {
				return
			}
			rerr := &rpcError{
				Code: errs.CodeOversizedResponse,
				Message: fmt.Sprintf(
					"response body %d bytes exceeds max-response-bytes %d",
					len(buf), cfg.maxResponseBytes,
				),
			}
			replacement := rpcResponse{JSONRPC: "2.0", ID: resp.ID, Error: rerr}
			rb, rerrMarshal := json.Marshal(replacement)
			if rerrMarshal != nil {
				return
			}
			buf = rb
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_, _ = out.Write(buf)
		_, _ = out.Write([]byte{'\n'})
	}
	// oversizeRerr returns the typed error the dispatcher should attribute
	// to a tools/call whose marshalled response exceeded
	// -max-response-bytes. Audit and metrics consult it pre-send so the
	// record reflects "the call ran, but its output was suppressed" rather
	// than the silent success the handler reported. Returns nil when
	// either the cap is disabled (preserving the historical fast-path) or
	// the response would have fit.
	oversizeRerr := func(resp rpcResponse) *rpcError {
		if cfg.maxResponseBytes <= 0 {
			return nil
		}
		if resp.Error != nil {
			// A failed call already carries an error code; resizing
			// it to a different sentinel would mask the real cause
			// the handler reported.
			return nil
		}
		probe := resp
		probe.JSONRPC = "2.0"
		buf, err := json.Marshal(probe)
		if err != nil {
			return nil
		}
		if int64(len(buf)) <= cfg.maxResponseBytes {
			return nil
		}
		return &rpcError{
			Code: errs.CodeOversizedResponse,
			Message: fmt.Sprintf(
				"response body %d bytes exceeds max-response-bytes %d",
				len(buf), cfg.maxResponseBytes,
			),
		}
	}
	// Build the spec-defined list-change emitter and hand it to
	// whoever opted in via WithToolsListChangedNotifier (typically
	// *Tools.SetNotifier). The frame carries no id/params per the MCP
	// spec, so we marshal a fixed payload here instead of going
	// through send/rpcResponse — those types always emit either a
	// result or an error field. Installed BEFORE the reader goroutine
	// starts so the receiver has the emitter in place by the time the
	// client's first frame arrives.
	if cfg.installListChanged != nil {
		// Pre-marshal once: the payload is constant and avoiding the
		// per-call json.Marshal keeps the notifier cheap when
		// register/unregister fires in a hot loop.
		notifyFrame := []byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}` + "\n")
		notify := func() {
			writeMu.Lock()
			defer writeMu.Unlock()
			_, _ = out.Write(notifyFrame)
		}
		cfg.installListChanged(notify)
	}
	// readFrame ferries lines from the blocking ReadBytes call into a
	// channel so the dispatch loop can select on ctx.Done() too. Without
	// this hand-off, a SIGTERM with no further client traffic would
	// keep ReadBytes parked indefinitely on the underlying read syscall
	// — Go's runtime won't always interrupt a blocking pipe read just
	// because os.Stdin.Close() was called from another goroutine. The
	// reader exits when the underlying stream EOFs (or errors); on ctx
	// cancellation we abandon it and let the OS reap it on process
	// exit, which is fine because stdio is single-use anyway.
	type frame struct {
		line []byte
		err  error
	}
	frames := make(chan frame, 1)
	go func() {
		for {
			line, err := r.ReadBytes('\n')
			frames <- frame{line: line, err: err}
			if err != nil {
				return
			}
		}
	}()
	// drain runs after the read loop exits. It bounds the wg.Wait by
	// shutdownTimeout so a wedged handler can't block process teardown
	// forever. While waiting it also pulls any frames that arrive on
	// the read channel and replies -32603 "shutting down" so a flooding
	// client can't extend the drain window. Returns
	// ErrShutdownTimedOut when the timeout fires.
	drain := func() error {
		// Caller explicitly opted in to a 0 timeout: skip the drain.
		// Any in-flight goroutines keep running detached until the
		// process exits.
		if cfg.shutdownTimeoutSet && cfg.shutdownTimeout == 0 {
			return nil
		}
		waitDone := make(chan struct{})
		go func() {
			wg.Wait()
			close(waitDone)
		}()
		// rejectFrame replies "shutting down" to any post-cancel frame
		// that still parses as a request with an id. Notifications and
		// malformed frames are swallowed silently — there's nothing
		// useful to send back.
		rejectFrame := func(f frame) {
			if f.err != nil {
				return
			}
			if len(f.line) == 0 {
				return
			}
			var req rpcRequest
			if jerr := json.Unmarshal(f.line, &req); jerr != nil {
				return
			}
			if len(req.ID) == 0 {
				return
			}
			send(rpcResponse{ID: req.ID, Error: &rpcError{Code: codeInternalError, Message: "shutting down"}})
		}
		// drainPendingFrames pulls every frame currently sitting in the
		// channel and rejects it. Used after waitDone fires to make sure
		// a frame the reader goroutine queued just before / during ctx
		// cancel still gets a "shutting down" reply instead of being
		// silently dropped. We only consume frames that are already
		// ready — if the channel is empty we don't wait, since the
		// in-flight side has finished and there's nothing left to
		// coordinate with.
		drainPendingFrames := func() {
			for {
				select {
				case f := <-frames:
					rejectFrame(f)
				default:
					return
				}
			}
		}
		// No timeout configured (back-compat for callers that don't
		// pass WithShutdownTimeout): wait indefinitely, matching the
		// pre-flag behaviour where Serve always blocked on every
		// handler.
		if !cfg.shutdownTimeoutSet {
			for {
				select {
				case <-waitDone:
					drainPendingFrames()
					return nil
				case f := <-frames:
					rejectFrame(f)
				}
			}
		}
		t := time.NewTimer(cfg.shutdownTimeout)
		defer t.Stop()
		// shortPoll bounds how long we wait, after handlers drain, for
		// straggler frames the reader goroutine may have queued just as
		// ctx fired. Without it the test (and any client that writes
		// post-cancel frames) races the wg.Wait() goroutine and may
		// observe drain returning before the frame is rejected.
		const shortPoll = 100 * time.Millisecond
		for {
			select {
			case <-waitDone:
				// Give late-arriving frames a brief window to land before
				// we exit the drain. The remaining time budget is also
				// honoured so a flooding client can't extend shutdown
				// past the configured timeout.
				postWait := time.NewTimer(shortPoll)
				for {
					select {
					case f := <-frames:
						rejectFrame(f)
					case <-postWait.C:
						drainPendingFrames()
						return nil
					case <-t.C:
						postWait.Stop()
						drainPendingFrames()
						return nil
					}
				}
			case <-t.C:
				slog.Warn("shutdown drain timed out", "timeout", cfg.shutdownTimeout)
				return ErrShutdownTimedOut
			case f := <-frames:
				rejectFrame(f)
			}
		}
	}
	for {
		var f frame
		select {
		case <-ctx.Done():
			return drain()
		case f = <-frames:
		}
		if f.err != nil {
			if errors.Is(f.err, io.EOF) {
				// Client closed stdin: cancel any in-flight handler
				// contexts so polling loops (wait_for_text, …) bail
				// out promptly instead of running until their per-call
				// timeout. drain() then bounds how long Serve waits
				// for those handlers to actually return.
				cancelDispatch()
				return drain()
			}
			// Non-EOF read errors are also a terminal condition — the
			// pipe is in an unrecoverable state, so propagate the
			// cancellation to in-flight handlers the same way.
			cancelDispatch()
			if derr := drain(); derr != nil {
				return derr
			}
			// If ctx was cancelled, surface that — it's the more useful
			// signal for callers (read errors during shutdown are
			// expected when stdin is closed to break the blocking read).
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return f.err
		}
		line := f.line
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
		// Belt-and-suspenders: if ctx fired between the channel recv
		// and here (e.g. signal raced the dispatch), reject the frame
		// the same way we reject post-cancel arrivals. Notifications
		// get no response either way.
		if ctx.Err() != nil {
			if len(req.ID) > 0 {
				send(rpcResponse{ID: req.ID, Error: &rpcError{Code: codeInternalError, Message: "shutting down"}})
			}
			continue
		}
		// Generate a server-side request id and attach it (alongside
		// the method name) to a request-scoped logger. Every log line
		// emitted from the request path — here and inside Handler —
		// carries "rid" so operators can stitch concurrent requests
		// back together across goroutines. The same id is also passed
		// to the audit sink so audit records and the structured log
		// stream share a correlation key.
		rid := newRequestID()
		reqLogger := slog.Default().With("rid", rid, "method", req.Method)
		// Derive from dispatchCtx (not ctx) so a stdin EOF cancels the
		// handler's view of the world even when the parent context
		// itself is still alive. Signal-driven shutdown reaches the
		// handler the same way: cancelling ctx propagates to
		// dispatchCtx by parentage.
		reqCtx := WithLogger(dispatchCtx, reqLogger)
		reqLogger.Debug("rpc start", "id", string(req.ID))
		// Dispatch each request on its own goroutine so a slow tool call
		// doesn't block other traffic on the same stdio pipe.
		wg.Add(1)
		go func(req rpcRequest, reqCtx context.Context, reqLogger *slog.Logger, rid string) {
			// wg.Done is registered first so that, in defer-LIFO order,
			// the recovery defer below executes *before* wg.Done — i.e.
			// recover() runs, the error reply is written, and only then
			// is the WaitGroup released. This guarantees Shutdown's
			// wg.Wait() observes a fully-handled request even when the
			// handler panics.
			defer wg.Done()
			// Concurrency gate. Only tools/call frames are gated so
			// initialize / tools/list / notifications stay snappy even
			// when a flood of tool invocations is queued — those frames
			// don't touch tmux and don't justify back-pressure. limiter
			// is nil when the operator left -max-concurrent-calls at 0,
			// in which case Acquire/Release are no-ops.
			if req.Method == "tools/call" {
				if err := limiter.Acquire(reqCtx, req.Method); err != nil {
					if len(req.ID) == 0 {
						return
					}
					send(rpcResponse{ID: req.ID, Error: internalError(err)})
					return
				}
				defer limiter.Release()
			}
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
			// Mark per-session activity for the idle reaper before the
			// handler runs. Doing it pre-dispatch (rather than after the
			// handler returns) means a long-running wait_for_text that
			// happens to span the timeout window cannot race the reap
			// — Touch resets the clock to "this call started", which
			// matches operator intent ("this session is being used right
			// now"). No-op when the reaper is disabled or the call is
			// not a session-bearing tools/call.
			if reaper != nil && req.Method == "tools/call" {
				if name := sessionFromArgs(toolNameFromParams(req.Params), toolArgsFromParams(req.Params)); name != "" {
					reaper.Touch(name)
				}
			}
			started := time.Now()
			result, rerr := h(reqCtx, req.Method, req.Params)
			dur := time.Since(started)
			durMs := dur.Milliseconds()
			// Pre-check the marshalled response against
			// -max-response-bytes so audit / metrics see the oversize
			// sentinel instead of a silent success when the handler
			// produced a payload too big to ship. Notifications are
			// excluded — they carry no id, so a synthesised error has
			// nowhere to land. The actual wire-replacement happens
			// inside `send`, but mirroring the decision here keeps the
			// audit record honest. No-op (returns nil) when the cap is
			// disabled or the response would have fit.
			if rerr == nil && len(req.ID) > 0 {
				if oversize := oversizeRerr(rpcResponse{ID: req.ID, Result: result}); oversize != nil {
					rerr = oversize
					result = nil
				}
			}
			// Audit only `tools/call` — initialize / notifications /
			// tools/list are protocol bookkeeping and would drown the
			// log in noise without adding signal. Notifications go
			// through here too, but tools/call is always a request
			// (carries an id) so the early-return below for
			// notifications fires first only on non-tools/call
			// methods.
			if req.Method == "tools/call" {
				toolName := toolNameFromParams(req.Params)
				audit.Record(rid, toolName, toolArgsFromParams(req.Params), dur, rerr)
				// Same scope as audit: metrics live on the tools/call
				// surface only. Other RPCs are framing noise that
				// would inflate cardinality without operator value.
				metrics.observeToolCall(toolName, dur, rerr)
			}
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
		}(req, reqCtx, reqLogger, rid)
	}
}

// toolNameFromParams pulls the {"name": "..."} field out of a
// `tools/call` params payload. Returns "" when the params are absent
// or malformed — Record will still emit a record with tool="" so the
// failure is visible in the audit log instead of swallowed.
func toolNameFromParams(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var probe struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.Name
}

// toolArgsFromParams returns the raw bytes of the "arguments" field
// inside a `tools/call` params payload. The audit record only stores
// the byte length of this slice (never its contents — see Audit.Record),
// but we extract it as bytes so [extractSession] can peek at a single
// well-known field without needing to know any tool's full schema.
func toolArgsFromParams(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var probe struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil
	}
	return probe.Arguments
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
