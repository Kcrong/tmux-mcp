package server

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// waitForChannelMaxLen caps the length of the channel identifier the
// boundary accepts. tmux treats the channel as opaque text; in
// practice agents pick short, descriptive names ("build_done",
// "deploy_ready"). 128 bytes is well above any realistic legitimate
// value while still being small enough that a hostile or buggy caller
// cannot smuggle a multi-megabyte identifier into tmux's argv.
const waitForChannelMaxLen = 128

// waitForChannelRE pins the conservative shape a channel identifier may
// take. We accept the same alnum / underscore / dash set every other
// boundary regex on the tool surface uses so a caller can reuse a
// session-name-shaped string as a channel without learning a second
// vocabulary. Crucially we deliberately reject whitespace, shell
// metachars, dots, colons, and slashes — none of those appear in
// legitimate channel names and admitting them would risk stray
// quoting / argv-injection if a future tmux version starts treating
// any of them specially in `wait-for CHANNEL`.
var waitForChannelRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// waitForToolDefs holds the JSON Schema for the wait_for tool. It is
// appended onto the main toolDefs slice via the package init() in
// this file so the registration site stays close to the handler — the
// dispatcher in tools.go only needs the single name → handler entry.
//
// The four `mode` values map onto tmux wait-for's flag matrix:
//
//   - "wait"   → `tmux wait-for CHANNEL`            (BLOCKING — see below)
//   - "lock"   → `tmux wait-for -L CHANNEL`         (BLOCKING when contended)
//   - "signal" → `tmux wait-for -S CHANNEL`         (returns immediately)
//   - "unlock" → `tmux wait-for -U CHANNEL`         (returns immediately)
//
// `timeout_ms` is enforced caller-side via context.WithTimeout because
// tmux's own wait-for has no built-in deadline — it really does block
// forever until somebody fires `-S` (or the whole tmux server is
// restarted). Without the caller-side deadline a "wait" or contended
// "lock" call could pin a tools/call goroutine indefinitely; the
// schema's default (10s) keeps that footgun closed by default while
// still being long enough to cover a realistic "wait for the next CI
// stage to start" rendezvous.
//
// MUTATING in spirit (signal/unlock change the channel state, lock
// holds it, and even a bare wait blocks a tmux client) — deliberately
// excluded from the read-only allowlist so a -read-only operator
// cannot accidentally pin a goroutine waiting for a signal that never
// arrives.
var waitForToolDefs = []map[string]any{
	{
		"name": "wait_for",
		"description": "Synchronise across tmux clients via `tmux wait-for [-L|-S|-U] CHANNEL`. " +
			"Four modes correspond to tmux's flag matrix: " +
			"`mode=\"wait\"` blocks until somebody fires a signal on the same channel " +
			"(maps to bare `wait-for CHANNEL`); " +
			"`mode=\"signal\"` wakes every blocked waiter on the channel (maps to `-S`); " +
			"`mode=\"lock\"` acquires the channel as a mutex, blocking if it is already " +
			"held (maps to `-L`); " +
			"`mode=\"unlock\"` releases a previously-acquired lock (maps to `-U`). " +
			"Channel names are conservative identifiers (regex `^[A-Za-z0-9_-]+$`, len 1-128). " +
			"Blocking modes (`wait`, contended `lock`) honour `timeout_ms` (default 10000, max 600000) — " +
			"the boundary attaches a context.WithTimeout so the call cannot pin a goroutine " +
			"forever; on expiry the response carries CodeContextCancelled (-32003). " +
			"Signal/unlock are non-blocking and ignore `timeout_ms`. " +
			"Returns `{\"woken\": true, \"mode\": \"wait\", \"channel\": \"<name>\"}` on success.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"mode": map[string]any{
					"type":        "string",
					"enum":        []string{"wait", "lock", "signal", "unlock"},
					"default":     "wait",
					"description": "wait-for variant. wait/lock may block; signal/unlock return immediately.",
				},
				"channel": map[string]any{
					"type":        "string",
					"minLength":   1,
					"maxLength":   waitForChannelMaxLen,
					"description": "Rendezvous identifier; regex `^[A-Za-z0-9_-]+$`, len 1-128.",
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"default":     10000,
					"description": "Caller-side deadline applied via context.WithTimeout for blocking modes (wait, lock). 0 = use the request's existing context (no extra timeout). Range 0..600000 (10 min). Ignored by signal/unlock.",
				},
			},
			"required": []string{"channel"},
			// Lock the schema so a typo'd field (e.g. "name", "lock_id")
			// fails fast with -32602 instead of silently being ignored.
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register wait_for onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, waitForToolDefs...)
}

// validateWaitForChannel enforces the conservative channel-name policy
// for wait_for's required `channel` argument. An empty value is
// rejected (the schema marks it required, but a defensive boundary
// re-check keeps the contract crisp for direct in-process callers); a
// non-empty value must satisfy the regex/length policy so a stray
// quote or shell metachar cannot slip through to tmux's argv.
func validateWaitForChannel(channel string) *rpcError {
	if channel == "" {
		return invalidParams("channel required")
	}
	if len(channel) > waitForChannelMaxLen {
		return invalidParams("channel length %d out of range [1..%d]", len(channel), waitForChannelMaxLen)
	}
	if !waitForChannelRE.MatchString(channel) {
		return invalidParams("channel %q must match %s", channel, waitForChannelRE.String())
	}
	return nil
}

// waitForModeFromString maps the JSON-Schema enum value onto the
// internal WaitForMode. The empty string defaults to "wait" so a
// caller can omit `mode` entirely and get the canonical blocking
// rendezvous shape; the schema declares the same default so the wire
// behaviour matches the registered surface.
func waitForModeFromString(mode string) (tmuxctl.WaitForMode, *rpcError) {
	switch mode {
	case "", "wait":
		return tmuxctl.WaitForWait, nil
	case "lock":
		return tmuxctl.WaitForLock, nil
	case "signal":
		return tmuxctl.WaitForSignal, nil
	case "unlock":
		return tmuxctl.WaitForUnlock, nil
	}
	return 0, invalidParams("mode %q must be one of wait, lock, signal, unlock", mode)
}

// waitForModeBlocks reports whether the given mode is one of the
// blocking shapes (`wait`, `lock`). The handler uses this to gate the
// caller-side context.WithTimeout: signal / unlock are non-blocking
// and would only see the timeout as dead overhead, so we skip the
// extra context derivation for them. Returning false for the
// non-blocking modes also keeps the response payload clean — no
// "timed out" path can fire on a tool call that never blocks.
func waitForModeBlocks(mode tmuxctl.WaitForMode) bool {
	return mode == tmuxctl.WaitForWait || mode == tmuxctl.WaitForLock
}

// waitFor drives tmuxctl.Controller.WaitFor and serialises the result
// to the standard `{"content":[{"type":"text","text":"<json>"}]}`
// envelope MCP expects from a tools/call. The response shape is a
// flat object keyed by "woken" so a future addition (e.g. a wait
// duration field) can land alongside without breaking callers that
// only read the boolean.
//
// Argument handling:
//   - `channel` is required and must satisfy the conservative
//     regex/length policy — a stray quote / metachar / whitespace
//     would otherwise reach tmux's argv.
//   - `mode` defaults to "wait" (the canonical blocking rendezvous
//     shape); other accepted values are "lock", "signal", "unlock".
//   - `timeout_ms` defaults to 10000 (10s). 0 means "honour the
//     request's existing context" — useful when the caller has
//     already pinned a longer deadline upstream and does not want
//     the tool to override it.
//   - The schema sets additionalProperties:false, so an unknown field
//     is rejected at the JSON-decode layer with -32602.
//
// Blocking-mode contract. tmux's own `wait-for` has no built-in
// deadline, so for `wait` / `lock` we attach a caller-side
// context.WithTimeout before dispatching. On expiry the controller
// surfaces context.DeadlineExceeded which errs.CodeOf maps to
// CodeContextCancelled (-32003) — clients can branch on the stable
// code instead of substring-matching the message.
//
// MUTATING in spirit (signal/unlock change channel state; the
// blocking modes hold a tmux client). Deliberately NOT in
// readOnlyTools so a -read-only operator cannot accidentally pin a
// goroutine waiting for a signal that never arrives.
func (t *Tools) waitFor(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Mode      string `json:"mode"`
		Channel   string `json:"channel"`
		TimeoutMs int    `json:"timeout_ms"`
	}
	// Allow an explicit `null` / empty body so a tools/call frame with
	// `arguments: {}` still surfaces the required-field validation
	// below rather than choking on the unmarshal. The schema's
	// additionalProperties:false enforcement runs at the JSON-Schema
	// layer; this defensive decode keeps the handler honest for any
	// in-process caller that bypasses the schema.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, invalidParams("wait_for: %v", err)
		}
	}
	if rerr := validateWaitForChannel(args.Channel); rerr != nil {
		return nil, rerr
	}
	mode, rerr := waitForModeFromString(args.Mode)
	if rerr != nil {
		return nil, rerr
	}
	if rerr := validateDurationMs("timeout_ms", args.TimeoutMs); rerr != nil {
		return nil, rerr
	}

	// 0 is a deliberate sentinel meaning "no extra timeout — honour
	// the request's existing context". For blocking modes we attach a
	// caller-side context.WithTimeout when timeout_ms > 0; the schema
	// default (10000ms) covers the omitted-field case at the wire
	// layer, so a 0 reaching this point came from an explicit caller
	// choice and we honour it verbatim. Non-blocking modes (signal /
	// unlock) skip the derivation regardless — the extra context
	// would only add overhead.
	callCtx := ctx
	var cancel context.CancelFunc
	if waitForModeBlocks(mode) && args.TimeoutMs > 0 {
		callCtx, cancel = context.WithTimeout(ctx, time.Duration(args.TimeoutMs)*time.Millisecond)
		defer cancel()
	}

	if err := t.Ctl.WaitFor(callCtx, mode, args.Channel); err != nil {
		// When exec.CommandContext kills tmux on context expiry, the
		// resulting cmd.Run() error reads "signal: killed" rather than
		// the canonical context.DeadlineExceeded. The Controller.run
		// path passes that string through verbatim, so the JSON-RPC
		// error code would land on CodeInternal (-32603) — silently
		// hiding the cancellation from clients who want to branch on
		// the typed -32003. Promote the ctx error explicitly when it
		// is present so the wire code reflects the actual cause.
		if cerr := callCtx.Err(); cerr != nil {
			return nil, internalError(fmt.Errorf("wait_for: %w", cerr))
		}
		return nil, internalError(fmt.Errorf("wait_for: %w", err))
	}
	return jsonBlock(map[string]any{
		"woken":   true,
		"mode":    waitForModeToString(mode),
		"channel": args.Channel,
	})
}

// waitForModeToString is the inverse of waitForModeFromString; used to
// echo the resolved mode back in the response so a caller that
// omitted `mode` (and got the "wait" default) sees what actually ran.
func waitForModeToString(mode tmuxctl.WaitForMode) string {
	switch mode {
	case tmuxctl.WaitForWait:
		return "wait"
	case tmuxctl.WaitForLock:
		return "lock"
	case tmuxctl.WaitForSignal:
		return "signal"
	case tmuxctl.WaitForUnlock:
		return "unlock"
	}
	return ""
}
