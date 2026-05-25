package server

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// waitForTestSetup spins up a fresh *Tools with an anchor session (so
// the tmux server is definitely running — wait-for against a server-
// less socket returns a different stderr shape we don't want to
// exercise here), and returns a pre-bound `call` helper plus the
// deadline context. Pulling the boilerplate into a helper keeps every
// test case focused on the assertion that actually matters.
//
// Each caller must invoke t.Parallel() itself — t.Helper() inside a
// helper does not propagate t.Parallel, and the user-facing
// concurrency contract is "one tmux server per top-level test".
func waitForTestSetup(t *testing.T) (
	tools *Tools,
	call func(name string, args any) (any, *rpcError),
	ctx context.Context,
) {
	t.Helper()
	skipIfNoTmux(t)
	tools = newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	call = func(name string, args any) (any, *rpcError) {
		t.Helper()
		params := mustJSON(t, map[string]any{"name": name, "arguments": args})
		return tools.Handle(ctx, "tools/call", params)
	}

	// Anchor with a real session so the tmux server is definitely up;
	// channels live on the server and require it to be running.
	res, rerr := call("session_create", map[string]any{
		"name": "anchor_wait_for", "command": "/bin/sh", "width": 80, "height": 24,
	})
	if rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}
	_ = res
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "anchor_wait_for"},
			}))
	})
	return tools, call, ctx
}

// decodeWaitFor pulls the {"woken": ..., "mode": ..., "channel": ...}
// envelope out of the tools/call result so the assertions in each
// test stay focused on the field that matters.
func decodeWaitFor(t *testing.T, result any) (woken bool, mode, channel string) {
	t.Helper()
	body := extractText(t, result)
	var obj struct {
		Woken   bool   `json:"woken"`
		Mode    string `json:"mode"`
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("decode wait_for body %q: %v", body, err)
	}
	return obj.Woken, obj.Mode, obj.Channel
}

// TestHandle_WaitFor_SignalReturnsImmediately drives the load-bearing
// non-blocking path: `mode=signal` against a channel with no waiters
// must succeed instantly and echo back the resolved mode/channel. The
// "no waiter buffering" semantic comes from tmux itself; the boundary
// just has to not invent an extra error on top.
func TestHandle_WaitFor_SignalReturnsImmediately(t *testing.T) {
	t.Parallel()
	_, call, _ := waitForTestSetup(t)

	start := time.Now()
	res, rerr := call("wait_for", map[string]any{
		"mode":    "signal",
		"channel": "fire_and_forget",
	})
	if rerr != nil {
		t.Fatalf("wait_for signal: %s", rerr.Message)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("wait_for signal blocked for %s; non-blocking modes must return immediately", elapsed)
	}

	woken, mode, channel := decodeWaitFor(t, res)
	if !woken {
		t.Fatalf("woken=false, want true")
	}
	if mode != "signal" {
		t.Errorf("mode = %q, want signal", mode)
	}
	if channel != "fire_and_forget" {
		t.Errorf("channel = %q, want fire_and_forget", channel)
	}
}

// TestHandle_WaitFor_DefaultModeIsWait pins the schema default: a call
// without `mode` resolves to "wait" rather than (say) erroring on the
// missing field. We exercise this via the non-blocking signal->wait
// rendezvous below; here we only need to confirm the response echoes
// "wait" when the field is omitted, which lets a caller relying on
// the default see what actually ran.
func TestHandle_WaitFor_DefaultModeIsWait(t *testing.T) {
	t.Parallel()
	tools, _, ctx := waitForTestSetup(t)

	// Block in the background so the test goroutine can fire a signal
	// and inspect the resolved mode in the response. Without the
	// in-flight waiter the wait call would either hang on the
	// schema-default 10s timeout or return a CodeContextCancelled
	// error — neither is what we want to assert here.
	type result struct {
		res  any
		rerr *rpcError
	}
	done := make(chan result, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		params := mustJSON(t, map[string]any{
			"name": "wait_for",
			"arguments": map[string]any{
				"channel":    "default_mode_chan",
				"timeout_ms": 5000,
				// mode deliberately omitted — must default to "wait"
			},
		})
		res, rerr := tools.Handle(ctx, "tools/call", params)
		done <- result{res, rerr}
	}()

	// Give the waiter time to issue the bare wait-for to tmux before
	// we fire the signal. 200ms is enough for an exec.CommandContext
	// startup on every tmux build we support.
	time.Sleep(200 * time.Millisecond)
	_, rerr := tools.Handle(ctx, "tools/call", mustJSON(t, map[string]any{
		"name": "wait_for",
		"arguments": map[string]any{
			"mode":    "signal",
			"channel": "default_mode_chan",
		},
	}))
	if rerr != nil {
		t.Fatalf("wait_for signal: %s", rerr.Message)
	}

	select {
	case r := <-done:
		if r.rerr != nil {
			t.Fatalf("wait_for default mode: %s", r.rerr.Message)
		}
		woken, mode, _ := decodeWaitFor(t, r.res)
		if !woken {
			t.Fatalf("woken=false, want true")
		}
		if mode != "wait" {
			t.Errorf("mode echoed = %q, want wait (the schema default)", mode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("default-mode waiter did not return within 5s of signal")
	}
	wg.Wait()
}

// TestHandle_WaitFor_LockUnlockSerialises is the load-bearing mutex
// path through the dispatcher. After holding the lock with `mode=lock`,
// a second locker must block until `mode=unlock` releases the channel.
// We verify the contender is genuinely blocked (its result channel
// stays empty) for a small window, then release and assert the
// contender proceeds.
func TestHandle_WaitFor_LockUnlockSerialises(t *testing.T) {
	t.Parallel()
	tools, call, ctx := waitForTestSetup(t)

	// First locker: acquires the channel.
	_, rerr := call("wait_for", map[string]any{
		"mode":    "lock",
		"channel": "mutex_chan",
	})
	if rerr != nil {
		t.Fatalf("wait_for first lock: %s", rerr.Message)
	}

	// Second locker: must block until we unlock.
	type result struct {
		res  any
		rerr *rpcError
	}
	done := make(chan result, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		params := mustJSON(t, map[string]any{
			"name": "wait_for",
			"arguments": map[string]any{
				"mode":       "lock",
				"channel":    "mutex_chan",
				"timeout_ms": 10000,
			},
		})
		res, rerr := tools.Handle(ctx, "tools/call", params)
		done <- result{res, rerr}
	}()

	// Sample the contender's status: it must NOT have completed within
	// the small window before unlock. 300ms comfortably exceeds the
	// tmux child-process startup cost on the slowest CI runners.
	select {
	case r := <-done:
		t.Fatalf("second locker returned (rerr=%v) before unlock; lock semantics violated", r.rerr)
	case <-time.After(300 * time.Millisecond):
		// Still blocked — expected.
	}

	_, rerr = call("wait_for", map[string]any{
		"mode":    "unlock",
		"channel": "mutex_chan",
	})
	if rerr != nil {
		t.Fatalf("wait_for unlock: %s", rerr.Message)
	}

	select {
	case r := <-done:
		if r.rerr != nil {
			t.Fatalf("second locker returned error after unlock: %s", r.rerr.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second locker did not proceed within 5s after unlock")
	}
	wg.Wait()

	// Tidy: release the mutex so a sibling test (running in parallel
	// against a different controller, but defensive) does not
	// inherit a held lock.
	_, _ = call("wait_for", map[string]any{
		"mode":    "unlock",
		"channel": "mutex_chan",
	})
}

// TestHandle_WaitFor_TimeoutSurfacesContextCancelled is the
// load-bearing deadline path: a `wait` / `lock` call with no
// signaller must respect `timeout_ms` and surface the cancellation
// via the typed JSON-RPC code so a client can branch on it. Without
// this guard a misbehaving caller could pin a goroutine forever.
func TestHandle_WaitFor_TimeoutSurfacesContextCancelled(t *testing.T) {
	t.Parallel()
	_, call, _ := waitForTestSetup(t)

	start := time.Now()
	_, rerr := call("wait_for", map[string]any{
		"mode":       "wait",
		"channel":    "no_signaller_ever",
		"timeout_ms": 300,
	})
	elapsed := time.Since(start)
	if rerr == nil {
		t.Fatal("expected error after timeout_ms expired")
	}
	// Elapsed time is the load-bearing assertion: a non-cancelling
	// regression would block forever (or until the parent ctx fires).
	// Allow a generous upper bound to tolerate slow CI runners while
	// still catching a hang.
	if elapsed > 5*time.Second {
		t.Fatalf("wait_for blocked for %s after a 300ms timeout_ms; cancellation broken", elapsed)
	}
	if rerr.Code != errs.CodeContextCancelled {
		t.Fatalf("error code = %d, want CodeContextCancelled (%d); message=%q",
			rerr.Code, errs.CodeContextCancelled, rerr.Message)
	}
}

// TestHandle_WaitFor_RejectsBadChannel locks the regex check on
// `channel` so a stray quote, whitespace, or shell metachar cannot
// slip through to tmux's argv. The check runs before any tmux
// command, so the error must carry CodeInvalidParams.
func TestHandle_WaitFor_RejectsBadChannel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []struct {
		name    string
		channel string
	}{
		{"with_space", "bad name with spaces"},
		{"with_slash", "bad/slash"},
		{"with_semicolon", "x;rm"},
		{"with_dot", "with.dot"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "wait_for",
				"arguments": map[string]any{
					"mode":    "signal",
					"channel": tc.channel,
				},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params error for channel %q", tc.channel)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d); message=%q",
					rerr.Code, errs.CodeInvalidParams, rerr.Message)
			}
		})
	}
}

// TestHandle_WaitFor_RejectsMissingChannel pins the up-front
// required-field guard. Without `channel` the call must fail with
// CodeInvalidParams before any tmux command runs.
func TestHandle_WaitFor_RejectsMissingChannel(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name":      "wait_for",
		"arguments": map[string]any{"mode": "signal"},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for missing channel")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
}

// TestHandle_WaitFor_RejectsUnknownMode locks the enum gate. Any
// value outside {wait, lock, signal, unlock} must surface
// CodeInvalidParams so a typo'd mode does not silently dispatch the
// default `wait` shape (which would block).
func TestHandle_WaitFor_RejectsUnknownMode(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	params := mustJSON(t, map[string]any{
		"name": "wait_for",
		"arguments": map[string]any{
			"mode":    "lockify",
			"channel": "any",
		},
	})
	_, rerr := tools.Handle(context.Background(), "tools/call", params)
	if rerr == nil {
		t.Fatal("expected invalid params error for unknown mode")
	}
	if rerr.Code != errs.CodeInvalidParams {
		t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
	}
	if !strings.Contains(rerr.Message, "wait, lock, signal, unlock") {
		t.Errorf("error msg = %q, expected to enumerate the legal modes", rerr.Message)
	}
}

// TestHandle_WaitFor_RejectsTimeoutOutOfRange enforces the standard
// duration bound the rest of the surface uses (0..600000). A value
// outside that range must surface CodeInvalidParams before any tmux
// command runs.
func TestHandle_WaitFor_RejectsTimeoutOutOfRange(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	cases := []struct {
		name string
		ms   int
	}{
		{"negative", -1},
		{"too_large", 600001},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mustJSON(t, map[string]any{
				"name": "wait_for",
				"arguments": map[string]any{
					"mode":       "wait",
					"channel":    "x",
					"timeout_ms": tc.ms,
				},
			})
			_, rerr := tools.Handle(context.Background(), "tools/call", params)
			if rerr == nil {
				t.Fatalf("expected invalid params error for timeout_ms=%d", tc.ms)
			}
			if rerr.Code != errs.CodeInvalidParams {
				t.Fatalf("code = %d, want CodeInvalidParams (%d)", rerr.Code, errs.CodeInvalidParams)
			}
		})
	}
}

// TestHandle_WaitFor_ListedInTools confirms the init()-time
// registration actually wired wait_for into tools/list. Without this
// guard a regression in the package-init append could silently drop
// the tool from the surface even though the dispatcher still
// recognised it.
func TestHandle_WaitFor_ListedInTools(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	res, rerr := tools.Handle(context.Background(), "tools/list", nil)
	if rerr != nil {
		t.Fatalf("tools/list: %s", rerr.Message)
	}
	listing := res.(map[string]any)["tools"].([]map[string]any)
	for _, def := range listing {
		if def["name"] == "wait_for" {
			schema, ok := def["inputSchema"].(map[string]any)
			if !ok {
				t.Fatal("wait_for inputSchema missing or not an object")
			}
			// additionalProperties:false is the load-bearing surface
			// guard against typo'd fields. A regression that dropped
			// it would silently accept "channel_name", "timeoutMs",
			// etc., and surprise every caller.
			if ap, ok := schema["additionalProperties"].(bool); !ok || ap {
				t.Errorf("wait_for inputSchema additionalProperties = %v, want false", schema["additionalProperties"])
			}
			return
		}
	}
	t.Fatal("tools/list missing wait_for")
}

// TestHandle_WaitFor_ZeroTimeoutHonoursParentContext exercises the
// "0 means use the request's existing context" sentinel. We attach a
// short deadline to the request context and verify the wait
// terminates by that deadline rather than the schema's 10s default.
// Without this contract a caller that pinned a 30s deadline upstream
// would silently get the 10s schema default instead.
func TestHandle_WaitFor_ZeroTimeoutHonoursParentContext(t *testing.T) {
	t.Parallel()
	skipIfNoTmux(t)
	tools := newTools(t)
	t.Cleanup(func() {
		// The test does not run setup helper; a kill_all_sessions
		// keeps the controller tidy on shutdown.
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "kill_all_sessions",
				"arguments": map[string]any{},
			}))
	})

	// Anchor the daemon. We can't reuse waitForTestSetup here because
	// that helper bakes a 20s ctx into the closure, and we need a
	// short request-scoped ctx for the assertion below.
	anchorCtx, cancelAnchor := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancelAnchor)
	if _, rerr := tools.Handle(anchorCtx, "tools/call", mustJSON(t, map[string]any{
		"name":      "session_create",
		"arguments": map[string]any{"name": "wf_zero_ctx", "command": "/bin/sh"},
	})); rerr != nil {
		t.Fatalf("session_create anchor: %s", rerr.Message)
	}

	// 400ms request-scoped deadline; the wait_for call passes
	// timeout_ms=0 so the handler must NOT layer a fresh
	// context.WithTimeout on top — it must honour the caller's ctx.
	reqCtx, cancelReq := context.WithTimeout(context.Background(), 400*time.Millisecond)
	t.Cleanup(cancelReq)

	start := time.Now()
	_, rerr := tools.Handle(reqCtx, "tools/call", mustJSON(t, map[string]any{
		"name": "wait_for",
		"arguments": map[string]any{
			"mode":       "wait",
			"channel":    "zero_timeout_chan",
			"timeout_ms": 0,
		},
	}))
	elapsed := time.Since(start)
	if rerr == nil {
		t.Fatal("expected error after parent ctx expired")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("wait blocked for %s past the 400ms parent ctx; cancellation broken", elapsed)
	}
	if rerr.Code != errs.CodeContextCancelled {
		t.Fatalf("error code = %d, want CodeContextCancelled (%d)", rerr.Code, errs.CodeContextCancelled)
	}
}
