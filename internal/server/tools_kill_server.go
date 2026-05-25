package server

import (
	"context"
	"encoding/json"
)

// killServerToolDefs holds the JSON Schema for the kill_server tool.
// The block is appended onto the main toolDefs slice from this file's
// init() so the registration site stays close to the handler — and the
// dispatcher in tools.go only needs the single name → handler entry.
//
// The schema declares no fields and locks `additionalProperties: false`
// so a stray argument (e.g. a mistakenly-passed "session") fails fast
// with -32602 (invalid params) before the controller is touched.
// kill_server is the most blast-radius-heavy tool on this surface;
// rejecting unexpected payloads up front prevents an agent from feeling
// like an extra knob exists when it does not.
var killServerToolDefs = []map[string]any{
	{
		"name": "kill_server",
		"description": "Ask the controller's private tmux daemon to exit via " +
			"`tmux kill-server`. EVERY session, window, and pane on this " +
			"server is torn down in one shot — including unrelated work " +
			"belonging to other agents sharing the same controller. The " +
			"call is idempotent: when no daemon is running the response is " +
			"still a clean ack, so an agent looping in a recovery dance " +
			"never sees a spurious failure for a state that is already " +
			"correct. Use kill_all_sessions for a softer reset that leaves " +
			"the tmux server itself alive (and pays no re-spawn cost on " +
			"the next session_create).",
		"inputSchema": map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	},
}

func init() {
	// Register kill_server onto the main toolDefs slice. Doing this from
	// init() keeps the new tool surface entirely contained in this file
	// (apart from the single dispatcher case in tools.go) and avoids
	// touching the shared toolDefs literal that other PRs are editing.
	toolDefs = append(toolDefs, killServerToolDefs...)
}

// handleKillServer drives tmuxctl.Controller.KillServer and forgets
// snapshot history for every session the daemon was carrying just
// before it died. The handler ignores its arguments (the schema
// declares no fields and locks additionalProperties), but tolerates an
// empty / null arguments value so clients can call it without sending
// an arguments object at all.
//
// Snapshot bookkeeping. kill-server tears down every session on the
// controller's tmux daemon — including ones outside this server's
// -session-prefix scope — so every cached snapshot becomes stale on
// success. We enumerate sessions before issuing the kill and then walk
// the resulting names through Snap.Forget, mirroring what
// kill_all_sessions does. Doing the listing pre-kill avoids racing the
// freshly-exited daemon (post-kill ListSessions is the "no server
// running" no-op path, which would surface zero names and silently
// leak history until TTL eviction). A best-effort listing failure is
// not fatal: the kill still proceeds, and any orphaned snapshot
// entries age out via the store's TTL sweep.
//
// -session-prefix interaction. kill_server intentionally does not
// honour the prefix. tmux kill-server has no per-session form, and
// emulating one ("kill every session in our prefix") would silently
// duplicate kill_all_sessions while leaving the operator's mental model
// of the tool wrong. The description above carries the operator-facing
// caution; the handler itself just does what kill-server does.
func (t *Tools) handleKillServer(ctx context.Context, _ json.RawMessage) (any, *rpcError) {
	// Enumerate first so we can purge snapshot history for every
	// session about to disappear. Any error is best-effort — we still
	// want the kill to land — so we drop down to an empty `pre` slice
	// and let TTL eviction reclaim any orphaned entries.
	pre, _ := t.Ctl.ListSessions(ctx)
	if err := t.Ctl.KillServer(ctx); err != nil {
		return nil, internalError(err)
	}
	for _, name := range pre {
		t.Snap.Forget(name)
	}
	return jsonBlock(map[string]any{"killed": true})
}
