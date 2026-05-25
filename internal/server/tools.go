package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/snapshot"
	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// defaultScrollbackMaxLines is the safety ceiling applied to
// mode=scrollback captures when the caller does not supply max_lines.
// Scrollback can run to tens of megabytes on long-lived shells, which
// would blow up the JSON-RPC frame and stress the client's memory.
const defaultScrollbackMaxLines = 5000

// MCP tool surface. Each entry is (name, description, JSON Schema).
//
// Schemas are written in plain JSON for clarity; they are passed back
// in tools/list verbatim.
var toolDefs = []map[string]any{
	{
		"name":        "session_create",
		"description": "Start a new detached tmux session running command. Width/height are columns × rows.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":    map[string]any{"type": "string"},
				"command": map[string]any{"type": "string"},
				"cwd":     map[string]any{"type": "string"},
				"width":   map[string]any{"type": "integer", "minimum": 20, "default": 120},
				"height":  map[string]any{"type": "integer", "minimum": 5, "default": 40},
				"env":     map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			},
			"required": []string{"name"},
		},
	},
	{
		"name":        "session_list",
		"description": "List names of sessions currently managed by this server.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	},
	{
		"name":        "session_kill",
		"description": "Kill the named session.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"name": map[string]any{"type": "string"}},
			"required":   []string{"name"},
		},
	},
	{
		"name": "send_keys",
		"description": "Type into a session. keys is an array; tmux interprets named keys " +
			"like \"Up\", \"Down\", \"Enter\", \"Tab\", \"C-c\". Set literal=true to bypass " +
			"key-name interpretation and send keystrokes verbatim.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{"type": "string"},
				"keys":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"literal": map[string]any{"type": "boolean", "default": false},
			},
			"required": []string{"session", "keys"},
		},
	},
	{
		"name": "capture",
		"description": "Read the visible pane (or full scrollback). When ansi=true the result " +
			"includes terminal escape sequences. mode=scrollback is capped at 5000 lines by " +
			"default; set max_lines to override (0 keeps the default cap for scrollback and " +
			"means no cap for visible). Scrollback captures larger than chunk_lines " +
			"(default 5000) are split into pages — the response carries a non-empty " +
			"cursor that the caller passes back on the next call to fetch the next chunk; " +
			"max_lines is ignored on follow-up calls. The cursor is opaque and rejected " +
			"with -32602 once the underlying buffer has been rotated by a newer capture " +
			"or has aged out (5-minute TTL).",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session":     map[string]any{"type": "string"},
				"mode":        map[string]any{"type": "string", "enum": []string{"visible", "scrollback"}, "default": "visible"},
				"ansi":        map[string]any{"type": "boolean", "default": false},
				"max_lines":   map[string]any{"type": "integer", "minimum": 0, "default": 0},
				"cursor":      map[string]any{"type": "string"},
				"chunk_lines": map[string]any{"type": "integer", "minimum": 0, "default": defaultChunkLines},
			},
			"required": []string{"session"},
		},
	},
	{
		"name":        "wait_for_stable",
		"description": "Block until the visible pane is unchanged for quiet_ms, then return the snapshot. Useful for waiting out a TUI redraw.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session":    map[string]any{"type": "string"},
				"quiet_ms":   map[string]any{"type": "integer", "minimum": 50, "default": 400},
				"step_ms":    map[string]any{"type": "integer", "minimum": 20, "default": 100},
				"timeout_ms": map[string]any{"type": "integer", "minimum": 100, "default": 10000},
			},
			"required": []string{"session"},
		},
	},
	{
		"name":        "wait_for_text",
		"description": "Block until pattern (Go regex) matches the visible pane. Returns the matched substring plus the snapshot.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session":    map[string]any{"type": "string"},
				"pattern":    map[string]any{"type": "string"},
				"step_ms":    map[string]any{"type": "integer", "minimum": 20, "default": 100},
				"timeout_ms": map[string]any{"type": "integer", "minimum": 100, "default": 10000},
			},
			"required": []string{"session", "pattern"},
		},
	},
	{
		"name":        "snapshot_diff",
		"description": "Capture the visible pane and return only the lines that changed since prior_token. Pass an empty prior_token on the first call.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session":     map[string]any{"type": "string"},
				"prior_token": map[string]any{"type": "string"},
			},
			"required": []string{"session"},
		},
	},
	{
		"name":        "resize",
		"description": "Resize the session window to width × height (cols × rows).",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session": map[string]any{"type": "string"},
				"width":   map[string]any{"type": "integer", "minimum": 20},
				"height":  map[string]any{"type": "integer", "minimum": 5},
			},
			"required": []string{"session", "width", "height"},
		},
	},
	{
		"name": "kill_all_sessions",
		"description": "Kill every session this server manages and forget all snapshot " +
			"history. Useful for agent error-recovery loops that want a clean slate " +
			"without restarting the server process. The tmux server itself stays running.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	},
}

// ToolHandler is the per-call signature every dynamically-registered
// tool must implement. It mirrors the dispatch contract of the
// statically-wired tools so registrants can reuse the same error
// helpers (invalidParams / internalError) the rest of the surface
// uses.
type ToolHandler func(ctx context.Context, args json.RawMessage) (any, *rpcError)

// Tools holds the dispatch state shared across calls.
type Tools struct {
	Ctl  *tmuxctl.Controller // controller backing every tmux operation the tools issue.
	Snap *snapshot.Store     // per-session capture history powering snapshot_diff.
	// captures is the per-session pending-capture buffer that backs the
	// `capture` tool's cursor pagination. Buffers are populated when a
	// scrollback capture is larger than chunk_lines, and dropped after
	// the last page is delivered or after the TTL expires.
	captures *captureBufferStore
	// Version is the binary version reported in the MCP initialize
	// response's serverInfo.version. It is populated from main.version
	// (ldflags-injected) at construction time. Empty/zero values fall
	// back to "dev" so the field always carries a sensible string.
	Version string
	// SessionPrefix, when non-empty, is prepended to every session name
	// the tool surface forwards to tmux. Created sessions land on tmux
	// under `<prefix><user-name>`, and references on every other session-
	// bearing tool (capture, send_keys, session_kill, …) are resolved the
	// same way before the call hits tmuxctl. session_list /
	// kill_all_sessions filter the controller's view to the prefix and
	// strip it from the response so the client always sees logical names.
	// Empty keeps the historical "no prefix" behaviour for back-compat.
	// Set from the operator-supplied -session-prefix CLI flag and
	// validated at startup against the same regex used for session names.
	SessionPrefix string

	// mu guards defs, dyn, and notify.
	//
	// Per the MCP spec the server may grow / shrink its tool surface at
	// runtime (see notifications/tools/list_changed). RegisterTool /
	// UnregisterTool walk these maps under mu so a concurrent
	// tools/list snapshot stays internally consistent — callers never
	// observe a half-applied registration.
	mu sync.Mutex
	// defs is the per-instance copy of the tool definitions surfaced
	// via tools/list. It is seeded lazily from the package-level
	// toolDefs slice so static registrations (the legacy init() hooks
	// in tools_panes.go / tools_signal.go / tools_describe.go) remain
	// the source of truth for the initial surface, with dynamic
	// Register/Unregister mutations layered on top.
	defs []map[string]any
	// dyn maps a dynamically-registered tool's name to its handler.
	// The static dispatcher in callTool retains its switch statement
	// for the built-in tools; dyn is consulted only when the switch
	// has no match, so adding a new built-in does not require touching
	// the dynamic path.
	dyn map[string]ToolHandler
	// allowlist gates which tool names are exposed via tools/list and
	// dispatchable via tools/call. nil = no filter (every registered
	// tool is exposed, the back-compat default). A non-nil empty map
	// means "no tools exposed" — operators who pass -allowlist with an
	// empty value get a degenerate but valid configuration. Non-nil and
	// populated means "expose only these names". Names not in the map
	// are filtered out of the tools/list response and rejected at
	// tools/call dispatch with a -32601 methodNotFound error.
	allowlist map[string]bool
	// notify is the writeMu-locked emitter Serve hands to *Tools via
	// SetNotifier. nil before Serve binds it (e.g. when RegisterTool
	// is called from a unit test that never starts the dispatcher),
	// in which case Register/Unregister silently skip the wire-level
	// notification — there is no client to notify.
	notify func()
	// initialized records whether the dispatcher has seen an
	// `initialize` request. The MCP spec only expects the server to
	// emit list-change notifications after the connection is up, so
	// Register/Unregister gate emission on this flag to avoid sending
	// notifications during the brief window between Serve binding the
	// notifier and the client driving initialize.
	initialized atomic.Bool
}

// NewTools wires a Controller and Store together. Any [snapshot.Option]
// args are forwarded verbatim to [snapshot.New], so callers can tune
// behaviour like the snapshot TTL without breaking the zero-arg call
// site (`NewTools(c)`) used by tests and the default deployment.
//
// The pending-capture buffer that powers the `capture` tool's cursor
// pagination is also wired in here. Always go through this constructor:
// bypassing it (a bare struct literal) leaves t.captures nil and the
// nil-receiver methods on captureBufferStore quietly degrade — paging
// state is silently lost, which is the right behaviour for tests that
// only exercise validation paths but very much not what a production
// server wants.
func NewTools(c *tmuxctl.Controller, opts ...snapshot.Option) *Tools {
	return &Tools{
		Ctl:      c,
		Snap:     snapshot.New(opts...),
		captures: newCaptureBufferStore(),
	}
}

// SetNotifier installs the callback Tools uses to emit
// notifications/tools/list_changed frames after a Register/Unregister
// mutation. Serve invokes this from its setup phase with a function
// that writes through the dispatcher's writeMu, so notifications stay
// interleaving-safe with regular RPC responses. Passing nil clears the
// hook (used by tests that want to assert on register/unregister
// without consuming notifications).
func (t *Tools) SetNotifier(notify func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.notify = notify
}

// SetAllowlist restricts which tools the dispatcher exposes via
// tools/list and accepts via tools/call. names is the operator-supplied
// list (typically the parsed value of -allowlist NAMES); each name is
// validated against the live tool registry — i.e. the same set
// snapshotDefs would have returned without the filter — so a typo
// surfaces as a clean startup error instead of a silently-empty surface.
//
// Behaviour:
//   - len(names) == 0: clears any previous allowlist. The dispatcher
//     reverts to the unfiltered default (every registered tool is
//     exposed and dispatchable). This is what main.go does when the
//     operator leaves -allowlist at its empty-string default.
//   - non-empty names: every entry must match a tool name currently in
//     the registry. Unknown names abort with a single error listing
//     them (in input order, deduplicated) — the validator runs in one
//     pass so all typos are reported together rather than one-by-one.
//   - whitespace around individual names is trimmed; empty entries
//     (e.g. from a stray comma) are skipped silently. This keeps the
//     CSV parser in main.go simple — it just splits on "," — without
//     pushing a "remove blanks" detail into every caller.
//
// On a validation error the existing allowlist (if any) is left
// untouched so a hot-swap path that passes a bad list does not flip
// the surface to an inconsistent state.
func (t *Tools) SetAllowlist(names []string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(names) == 0 {
		t.allowlist = nil
		return nil
	}

	// Build the set of currently-registered names so the validator
	// reflects the live registry, not a hardcoded list. Seed t.defs
	// the same way snapshotDefs would so the comparison covers every
	// init()-registered tool regardless of whether tools/list has
	// been called yet.
	if t.defs == nil {
		t.defs = make([]map[string]any, len(toolDefs))
		copy(t.defs, toolDefs)
	}
	registered := make(map[string]bool, len(t.defs))
	for _, def := range t.defs {
		if name, _ := def["name"].(string); name != "" {
			registered[name] = true
		}
	}
	if t.dyn != nil {
		for name := range t.dyn {
			registered[name] = true
		}
	}

	// allow accumulates the validated set in two passes so we can
	// report every typo at once rather than failing on the first one.
	// seenInput dedups entries the operator listed twice (e.g.
	// "capture,capture") without inflating the unknown-list error.
	allow := make(map[string]bool, len(names))
	seenInput := make(map[string]bool, len(names))
	var unknown []string
	for _, raw := range names {
		n := strings.TrimSpace(raw)
		if n == "" {
			continue
		}
		if seenInput[n] {
			continue
		}
		seenInput[n] = true
		if !registered[n] {
			unknown = append(unknown, n)
			continue
		}
		allow[n] = true
	}
	if len(unknown) > 0 {
		return fmt.Errorf("unknown tools in -allowlist: %s", strings.Join(unknown, ", "))
	}
	t.allowlist = allow
	return nil
}

// allowedLocked reports whether name is dispatchable under the current
// allowlist. The caller must hold t.mu. A nil allowlist returns true
// for every name (the back-compat "no filter" mode); otherwise the
// answer is the map's truth value for name. Empty-string is rejected
// regardless so a malformed tools/call (no name field) cannot bypass
// the filter — the static switch in callTool already rejects an empty
// name via its fallthrough branch, but the centralised check here keeps
// the contract uniform.
func (t *Tools) allowedLocked(name string) bool {
	if t.allowlist == nil {
		return true
	}
	if name == "" {
		return false
	}
	return t.allowlist[name]
}

// allowed is the lock-acquiring counterpart of allowedLocked, used by
// the dispatcher (callTool) which does not otherwise hold t.mu.
func (t *Tools) allowed(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.allowedLocked(name)
}

// snapshotDefs returns a copy of the current tool-definition slice for
// tools/list. The copy is needed so the response we hand to the
// dispatcher cannot be mutated by a concurrent RegisterTool /
// UnregisterTool while the JSON encoder is walking it. Map values are
// not deep-copied because the schemas are treated as immutable once
// registered.
//
// When SetAllowlist has installed a filter, definitions whose name is
// not on the allowlist are omitted from the returned slice. The
// original ordering of the surviving entries is preserved so a tools/list
// response with -allowlist=capture,wait_for_text returns those tools in
// the order they were registered, not the order they appeared on the
// CLI.
func (t *Tools) snapshotDefs() []map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.defs == nil {
		// First access: seed from the package-level toolDefs that the
		// init() hooks have already populated. Copying here keeps the
		// per-instance slice independent of the package global so
		// tests that spin up multiple *Tools don't observe each
		// other's mutations.
		t.defs = make([]map[string]any, len(toolDefs))
		copy(t.defs, toolDefs)
	}
	if t.allowlist == nil {
		out := make([]map[string]any, len(t.defs))
		copy(out, t.defs)
		return out
	}
	out := make([]map[string]any, 0, len(t.defs))
	for _, def := range t.defs {
		name, _ := def["name"].(string)
		if t.allowedLocked(name) {
			out = append(out, def)
		}
	}
	return out
}

// RegisterTool adds a tool to the dispatch surface at runtime and
// emits notifications/tools/list_changed so subscribed clients
// re-fetch tools/list. def must carry at least a "name" key whose
// value is a non-empty string — the same field the JSON Schema entry
// exposes via tools/list. A duplicate name replaces the previous
// registration in place (the slice entry is overwritten and the
// handler map updated) so callers can hot-swap an implementation
// without first calling UnregisterTool.
//
// The notification is suppressed when SetNotifier has not been called
// or when the server has not yet processed an `initialize` frame:
// there is no client to notify in either case, and the MCP spec
// expects list-change frames only after the connection is up.
func (t *Tools) RegisterTool(def map[string]any, handler ToolHandler) {
	if def == nil || handler == nil {
		return
	}
	name, _ := def["name"].(string)
	if name == "" {
		return
	}

	t.mu.Lock()
	if t.defs == nil {
		t.defs = make([]map[string]any, len(toolDefs))
		copy(t.defs, toolDefs)
	}
	if t.dyn == nil {
		t.dyn = make(map[string]ToolHandler)
	}
	// Replace in place when the name is already present so a hot-swap
	// does not leave a stale schema entry behind.
	replaced := false
	for i, existing := range t.defs {
		if existingName, _ := existing["name"].(string); existingName == name {
			t.defs[i] = def
			replaced = true
			break
		}
	}
	if !replaced {
		t.defs = append(t.defs, def)
	}
	t.dyn[name] = handler
	notify := t.notify
	t.mu.Unlock()

	if notify != nil && t.initialized.Load() {
		notify()
	}
}

// UnregisterTool drops the named tool from the dispatch surface and
// emits notifications/tools/list_changed. Names that do not match a
// previously-registered tool are silently ignored — the goal is "make
// this tool not be there", which is already true. Static built-ins
// (the entries seeded from the package toolDefs) can be removed too;
// callers wanting to "hide" a built-in for a particular client can
// drop it without restarting the server.
//
// Like RegisterTool, the notification is suppressed when no notifier
// has been bound or the server has not yet processed `initialize`.
func (t *Tools) UnregisterTool(name string) {
	if name == "" {
		return
	}
	t.mu.Lock()
	if t.defs == nil {
		t.defs = make([]map[string]any, len(toolDefs))
		copy(t.defs, toolDefs)
	}
	removed := false
	for i, existing := range t.defs {
		if existingName, _ := existing["name"].(string); existingName == name {
			t.defs = append(t.defs[:i], t.defs[i+1:]...)
			removed = true
			break
		}
	}
	if t.dyn != nil {
		if _, ok := t.dyn[name]; ok {
			delete(t.dyn, name)
			removed = true
		}
	}
	notify := t.notify
	t.mu.Unlock()

	if !removed {
		return
	}
	if notify != nil && t.initialized.Load() {
		notify()
	}
}

// dynamicHandler returns the handler registered for name, or nil if
// no dynamic registration matches. The static dispatcher consults
// this when its switch has no built-in case for the tool name.
func (t *Tools) dynamicHandler(name string) ToolHandler {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dyn == nil {
		return nil
	}
	return t.dyn[name]
}

// serverVersion returns the version string the server should advertise
// to clients in initialize. Falling back to "dev" matches the binary's
// default when ldflags didn't set a value.
func (t *Tools) serverVersion() string {
	if t.Version == "" {
		return "dev"
	}
	return t.Version
}

// resolveSessionRef rewrites a user-supplied session reference into the
// actual tmux session name. When SessionPrefix is empty (the historical
// default) the input is returned unchanged so existing deployments see
// no behaviour change. When SessionPrefix is set, the prefix is glued
// onto the front so every tmuxctl call lands on the prefixed session.
//
// The empty-string case is preserved verbatim: handlers still call this
// before validating, and an empty input produces an empty output so the
// downstream validate*() helpers can keep emitting their existing
// "session required" errors instead of complaining about a stray prefix
// the caller never typed.
func (t *Tools) resolveSessionRef(name string) string {
	if t == nil || t.SessionPrefix == "" || name == "" {
		return name
	}
	return t.SessionPrefix + name
}

// stripSessionPrefix is the inverse of resolveSessionRef: it converts an
// actual tmux session name back into the logical name a client supplied.
// Used by session_list and kill_all_sessions so the JSON payload carries
// the names the caller can use as-is on follow-up tool calls. When the
// prefix is empty the input is returned unchanged. Names that do not
// carry the prefix are left as-is too — those entries are filtered out
// by the caller, but returning them verbatim keeps this helper
// side-effect-free and easy to test in isolation.
func (t *Tools) stripSessionPrefix(name string) string {
	if t == nil || t.SessionPrefix == "" {
		return name
	}
	if !strings.HasPrefix(name, t.SessionPrefix) {
		return name
	}
	return name[len(t.SessionPrefix):]
}

// hasSessionPrefix reports whether name belongs to this server's prefix
// namespace. Always true when no prefix is configured (every session is
// "ours"), which keeps the kill_all_sessions / session_list filtering
// path branch-free for the back-compat default.
func (t *Tools) hasSessionPrefix(name string) bool {
	if t == nil || t.SessionPrefix == "" {
		return true
	}
	return strings.HasPrefix(name, t.SessionPrefix)
}

// resolvePaneTarget rewrites a pane-target string (the kind accepted by
// pane_select / pane_kill / pane_swap / pane_resize / clear_history) so
// the session component picks up the configured prefix. tmux pane
// targets come in three shapes:
//
//   - "%N"                    — pane id; carries no session reference,
//     returned unchanged.
//   - "session"               — bare session name; prefix it.
//   - "session:window[.pane]" — qualified target; prefix the session
//     half, leave window/pane untouched.
//
// Empty input is returned unchanged so the downstream validator can
// still emit "target required" without seeing a stray prefix.
func (t *Tools) resolvePaneTarget(target string) string {
	if t == nil || t.SessionPrefix == "" || target == "" {
		return target
	}
	if strings.HasPrefix(target, "%") {
		return target
	}
	idx := strings.Index(target, ":")
	if idx < 0 {
		return t.SessionPrefix + target
	}
	return t.SessionPrefix + target[:idx] + target[idx:]
}

// resolveWindowMoveTarget rewrites a window_move src/dst string. The
// shape is `<session>:<window>` where `<window>` may be empty (dst
// only). We split on the first colon, prefix the session half, and
// rejoin so tmux sees the actual prefixed session name. Empty input is
// returned unchanged so the downstream validator emits the standard
// "src required" / "dst required" errors instead of complaining about a
// stray prefix.
func (t *Tools) resolveWindowMoveTarget(target string) string {
	if t == nil || t.SessionPrefix == "" || target == "" {
		return target
	}
	idx := strings.Index(target, ":")
	if idx < 0 {
		// No separator: the validator will reject this anyway, but
		// passing it through unchanged keeps the error message about
		// the missing colon intact.
		return target
	}
	return t.SessionPrefix + target[:idx] + target[idx:]
}

// Handle is the dispatcher passed to server.Serve. It implements MCP's
// initialize / tools/list / tools/call surface.
//
// The tools/list response is built from a per-instance snapshot of the
// tool definitions so concurrent RegisterTool / UnregisterTool calls
// cannot race a tools/list encode. Beyond returning the static
// built-ins seeded at construction time, the server now supports
// dynamic registration: a RegisterTool / UnregisterTool call mutates
// the surface and emits notifications/tools/list_changed so
// subscribed clients re-fetch the list.
//
// The advertised capabilities include `tools.listChanged: true` to
// signal that the server emits list-change notifications when its
// surface mutates — clients that opt in re-fetch tools/list on every
// notification rather than caching the original response forever.
//
// initialize also flips an internal flag so subsequent register /
// unregister calls actually emit the notification. The MCP spec only
// expects servers to surface list-change frames after the connection
// is up, so the flag suppresses spurious frames during the brief
// window between Serve binding the notifier and the first initialize
// arriving.
func (t *Tools) Handle(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		t.initialized.Store(true)
		return map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": true},
			},
			"serverInfo": map[string]any{"name": "tmux-mcp", "version": t.serverVersion()},
		}, nil
	case "notifications/initialized":
		return nil, nil
	case "tools/list":
		return map[string]any{"tools": t.snapshotDefs()}, nil
	case "tools/call":
		return t.callTool(ctx, params)
	}
	return nil, methodNotFound(method)
}

func (t *Tools) callTool(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &call); err != nil {
		return nil, invalidParams("tools/call params: %v", err)
	}
	// Enforce the allowlist before dispatch so a filtered tool gets a
	// typed -32601 reply instead of executing. The check sits ahead of
	// the static switch (and the dyn fallback below) so adding a new
	// built-in does not require touching this guard, and so a client
	// that calls tools/call without first enumerating tools/list still
	// sees a uniform "tool not exposed" error.
	if !t.allowed(call.Name) {
		return nil, &rpcError{
			Code:    codeMethodNotFound,
			Message: fmt.Sprintf("tool %q is not in -allowlist", call.Name),
		}
	}
	switch call.Name {
	case "session_create":
		return t.sessionCreate(ctx, call.Arguments)
	case "session_list":
		return t.sessionList(ctx)
	case "session_kill":
		return t.sessionKill(ctx, call.Arguments)
	case "send_keys":
		return t.sendKeys(ctx, call.Arguments)
	case "capture":
		return t.capture(ctx, call.Arguments)
	case "wait_for_stable":
		return t.waitStable(ctx, call.Arguments)
	case "wait_for_text":
		return t.waitText(ctx, call.Arguments)
	case "snapshot_diff":
		return t.snapshotDiff(ctx, call.Arguments)
	case "resize":
		return t.resize(ctx, call.Arguments)
	case "kill_all_sessions":
		return t.handleKillAll(ctx, call.Arguments)
	case "start_server":
		return t.startServer(ctx, call.Arguments)
	case "kill_server":
		return t.handleKillServer(ctx, call.Arguments)
	case "kill_window":
		return t.killWindow(ctx, call.Arguments)
	case "list_panes":
		return t.listPanes(ctx, call.Arguments)
	case "pane_select":
		return t.paneSelect(ctx, call.Arguments)
	case "select_pane":
		return t.selectPane(ctx, call.Arguments)
	case "pane_split":
		return t.paneSplit(ctx, call.Arguments)
	case "pane_kill":
		return t.paneKill(ctx, call.Arguments)
	case "pane_swap":
		return t.paneSwap(ctx, call.Arguments)
	case "pane_join":
		return t.paneJoin(ctx, call.Arguments)
	case "pane_resize":
		return t.paneResize(ctx, call.Arguments)
	case "pane_break":
		return t.paneBreak(ctx, call.Arguments)
	case "move_pane":
		return t.movePane(ctx, call.Arguments)
	case "respawn_pane":
		return t.respawnPane(ctx, call.Arguments)
	case "clear_history":
		return t.clearHistory(ctx, call.Arguments)
	case "clock_mode":
		return t.clockMode(ctx, call.Arguments)
	case "run_shell":
		return t.runShell(ctx, call.Arguments)
	case "session_describe":
		return t.sessionDescribe(ctx, call.Arguments)
	case "has_session":
		return t.hasSession(ctx, call.Arguments)
	case "session_rename":
		return t.sessionRename(ctx, call.Arguments)
	case "session_inspect":
		return t.sessionInspect(ctx, call.Arguments)
	case "display_message":
		return t.displayMessage(ctx, call.Arguments)
	case "send_signal":
		return t.sendSignal(ctx, call.Arguments)
	case "window_create":
		return t.windowCreate(ctx, call.Arguments)
	case "new_window":
		return t.newWindow(ctx, call.Arguments)
	case "window_kill":
		return t.windowKill(ctx, call.Arguments)
	case "window_select":
		return t.windowSelect(ctx, call.Arguments)
	case "window_rename":
		return t.windowRename(ctx, call.Arguments)
	case "window_move":
		return t.windowMove(ctx, call.Arguments)
	case "swap_window":
		return t.swapWindow(ctx, call.Arguments)
	case "list_windows":
		return t.listWindows(ctx, call.Arguments)
	case "list_clients":
		return t.listClients(ctx, call.Arguments)
	case "choose_client":
		return t.chooseClient(ctx, call.Arguments)
	case "show_messages":
		return t.showMessages(ctx, call.Arguments)
	case "detach_client":
		return t.detachClient(ctx, call.Arguments)
	case "list_keys":
		return t.listKeys(ctx, call.Arguments)
	case "unbind_key":
		return t.unbindKey(ctx, call.Arguments)
	case "choose_tree":
		return t.chooseTree(ctx, call.Arguments)
	case "show_options":
		return t.showOptions(ctx, call.Arguments)
	case "set_window_option":
		return t.setWindowOption(ctx, call.Arguments)
	case "set_buffer":
		return t.setBuffer(ctx, call.Arguments)
	case "list_buffers":
		return t.listBuffers(ctx, call.Arguments)
	case "show_buffer":
		return t.showBuffer(ctx, call.Arguments)
	case "switch_client":
		return t.switchClient(ctx, call.Arguments)
	}
	// Fall back to the dynamic registry. Tools added via RegisterTool
	// don't have a hard-coded case above, so this is the only path
	// that reaches them. Returning methodNotFound preserves the
	// existing wire contract for genuinely-unknown names.
	if h := t.dynamicHandler(call.Name); h != nil {
		return h(ctx, call.Arguments)
	}
	return nil, methodNotFound("tools/call:" + call.Name)
}

func textBlock(s string) any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": s},
		},
	}
}

func jsonBlock(v any) (any, *rpcError) {
	buf, err := json.Marshal(v)
	if err != nil {
		return nil, internalError(err)
	}
	return textBlock(string(buf)), nil
}

func (t *Tools) sessionCreate(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Name    string            `json:"name"`
		Command string            `json:"command"`
		Cwd     string            `json:"cwd"`
		Width   int               `json:"width"`
		Height  int               `json:"height"`
		Env     map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("session_create: %v", err)
	}
	if rerr := validateSessionName(args.Name); rerr != nil {
		return nil, rerr
	}
	if rerr := validateCombinedSessionName(t.SessionPrefix, args.Name); rerr != nil {
		return nil, rerr
	}
	if rerr := validateWidth(args.Width); rerr != nil {
		return nil, rerr
	}
	if rerr := validateHeight(args.Height); rerr != nil {
		return nil, rerr
	}
	if rerr := validateCwd(args.Cwd); rerr != nil {
		return nil, rerr
	}
	// resolved is the actual tmux session name; the user keeps seeing
	// args.Name in the response so logical references stay stable.
	resolved := t.resolveSessionRef(args.Name)
	spec := tmuxctl.SessionSpec{
		Name: resolved, Command: args.Command, Cwd: args.Cwd,
		Width: args.Width, Height: args.Height, Env: args.Env,
	}
	if err := t.Ctl.CreateSession(ctx, spec); err != nil {
		return nil, internalError(err)
	}
	return textBlock(fmt.Sprintf("session %q created", args.Name)), nil
}

func (t *Tools) sessionList(ctx context.Context) (any, *rpcError) {
	names, err := t.Ctl.ListSessions(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	// When -session-prefix is set, filter to sessions in our namespace
	// and strip the prefix so the client sees the same logical names it
	// passed to session_create. Cross-prefix isolation: a session
	// created outside our prefix never appears in the listing.
	if t.SessionPrefix != "" {
		out := make([]string, 0, len(names))
		for _, n := range names {
			if t.hasSessionPrefix(n) {
				out = append(out, t.stripSessionPrefix(n))
			}
		}
		names = out
	}
	return jsonBlock(map[string]any{"sessions": names})
}

func (t *Tools) sessionKill(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("session_kill: %v", err)
	}
	if rerr := validateSessionName(args.Name); rerr != nil {
		return nil, rerr
	}
	resolved := t.resolveSessionRef(args.Name)
	if err := t.Ctl.KillSession(ctx, resolved); err != nil {
		return nil, internalError(err)
	}
	// Drop snapshot history for the dead session so we don't leak
	// per-session entries across many create/kill cycles. Snapshot keys
	// are the actual tmux names, so use the resolved (prefixed) value.
	t.Snap.Forget(resolved)
	return textBlock(fmt.Sprintf("session %q killed", args.Name)), nil
}

func (t *Tools) sendKeys(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session string   `json:"session"`
		Keys    []string `json:"keys"`
		Literal bool     `json:"literal"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("send_keys: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	if len(args.Keys) == 0 {
		return nil, invalidParams("send_keys: keys array must be non-empty")
	}
	if err := t.Ctl.SendKeys(ctx, t.resolveSessionRef(args.Session), args.Keys, args.Literal); err != nil {
		return nil, internalError(err)
	}
	return textBlock("ok"), nil
}

func (t *Tools) capture(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session    string `json:"session"`
		Mode       string `json:"mode"`
		ANSI       bool   `json:"ansi"`
		MaxLines   int    `json:"max_lines"`
		Cursor     string `json:"cursor"`
		ChunkLines int    `json:"chunk_lines"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("capture: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	// Lazy TTL eviction. Doing it on every capture (rather than spawning
	// a goroutine) keeps the package goroutine-free and bounds the work
	// to one map-walk per call.
	t.captures.cleanup()
	if args.Cursor != "" {
		// Follow-up page. mode/ansi/max_lines are intentionally ignored —
		// the buffer was captured under the original args, and the
		// caller only steers chunk_lines now. Key by the resolved tmux
		// name so the buffer/cursor lookup stays consistent under
		// -session-prefix.
		resolved := t.resolveSessionRef(args.Session)
		res, rerr := t.captureFromCursor(resolved, args.Cursor, args.ChunkLines)
		if rerr != nil {
			return nil, rerr
		}
		return jsonBlock(map[string]any{
			"snapshot":    res.body,
			"cursor":      res.cursor,
			"total_lines": res.totalLines,
			"truncated":   res.truncated,
		})
	}
	var mode tmuxctl.CaptureMode
	switch args.Mode {
	case "", "visible":
		mode = tmuxctl.CaptureVisible
	case "scrollback":
		mode = tmuxctl.CaptureScrollback
	default:
		return nil, invalidParams("capture mode %q must be \"visible\" or \"scrollback\"", args.Mode)
	}
	resolved := t.resolveSessionRef(args.Session)
	body, err := t.Ctl.Capture(ctx, resolved, mode, args.ANSI)
	if err != nil {
		return nil, internalError(err)
	}
	res := t.captureFirstPage(resolved, body, mode, args.MaxLines, args.ChunkLines)
	// Snapshot/diff machinery sees the *first page* of a paginated
	// capture so subsequent snapshot_diff calls stay consistent with
	// what the client received on this call. Reassembling all pages and
	// recording the joined body would be defensible too, but a paged
	// caller almost certainly does not want a multi-megabyte token.
	// Key by the resolved tmux name so capture/snapshot_diff stay
	// consistent under -session-prefix.
	snap := t.Snap.Record(resolved, res.body)
	return jsonBlock(map[string]any{
		"snapshot":    res.body,
		"token":       snap.Token,
		"changed":     snap.Changed,
		"truncated":   res.truncated,
		"cursor":      res.cursor,
		"total_lines": res.totalLines,
	})
}

// capCaptureBody enforces the max-lines policy for the capture tool.
//
// Rules:
//   - mode=visible: cap only when the caller asked for one (max_lines > 0).
//     Visible panes are already bounded by the terminal size, so leaving
//     them untouched preserves back-compat.
//   - mode=scrollback: cap at max_lines if set, otherwise fall back to
//     defaultScrollbackMaxLines so a careless or hostile caller cannot
//     pull tens of MB through the JSON-RPC channel.
//
// Truncation drops the *oldest* lines (the top of the buffer) so the
// returned snapshot keeps the most recent activity, which is what
// callers almost always actually want.
func capCaptureBody(body string, mode tmuxctl.CaptureMode, maxLines int) (string, bool) {
	limit := maxLines
	if mode == tmuxctl.CaptureScrollback && limit <= 0 {
		limit = defaultScrollbackMaxLines
	}
	if limit <= 0 {
		return body, false
	}
	// tmux capture-pane emits lines separated by '\n'. Splitting on '\n'
	// keeps a trailing empty "line" when the body ends with a newline,
	// which we preserve to avoid a spurious diff next to the truncation.
	lines := strings.Split(body, "\n")
	if len(lines) <= limit {
		return body, false
	}
	return strings.Join(lines[len(lines)-limit:], "\n"), true
}

func (t *Tools) waitStable(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session   string `json:"session"`
		QuietMs   int    `json:"quiet_ms"`
		StepMs    int    `json:"step_ms"`
		TimeoutMs int    `json:"timeout_ms"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("wait_for_stable: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	if rerr := validateDurationMs("quiet_ms", args.QuietMs); rerr != nil {
		return nil, rerr
	}
	if rerr := validateDurationMs("step_ms", args.StepMs); rerr != nil {
		return nil, rerr
	}
	if rerr := validateDurationMs("timeout_ms", args.TimeoutMs); rerr != nil {
		return nil, rerr
	}
	if args.QuietMs <= 0 {
		args.QuietMs = 400
	}
	if args.StepMs <= 0 {
		args.StepMs = 100
	}
	if args.TimeoutMs <= 0 {
		args.TimeoutMs = 10000
	}
	resolved := t.resolveSessionRef(args.Session)
	body, err := t.Ctl.WaitForStable(
		ctx, resolved,
		time.Duration(args.QuietMs)*time.Millisecond,
		time.Duration(args.StepMs)*time.Millisecond,
		time.Duration(args.TimeoutMs)*time.Millisecond,
	)
	if err != nil {
		return nil, internalError(err)
	}
	snap := t.Snap.Record(resolved, body)
	return jsonBlock(map[string]any{
		"snapshot": body,
		"token":    snap.Token,
	})
}

func (t *Tools) waitText(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session   string `json:"session"`
		Pattern   string `json:"pattern"`
		StepMs    int    `json:"step_ms"`
		TimeoutMs int    `json:"timeout_ms"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("wait_for_text: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	if rerr := validateDurationMs("step_ms", args.StepMs); rerr != nil {
		return nil, rerr
	}
	if rerr := validateDurationMs("timeout_ms", args.TimeoutMs); rerr != nil {
		return nil, rerr
	}
	if args.StepMs <= 0 {
		args.StepMs = 100
	}
	if args.TimeoutMs <= 0 {
		args.TimeoutMs = 10000
	}
	resolved := t.resolveSessionRef(args.Session)
	m, err := t.Ctl.WaitForText(
		ctx, resolved, args.Pattern,
		time.Duration(args.StepMs)*time.Millisecond,
		time.Duration(args.TimeoutMs)*time.Millisecond,
	)
	if err != nil {
		return nil, internalError(err)
	}
	snap := t.Snap.Record(resolved, m.Snapshot)
	return jsonBlock(map[string]any{
		"match":    m.Match,
		"snapshot": m.Snapshot,
		"token":    snap.Token,
	})
}

func (t *Tools) snapshotDiff(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session    string `json:"session"`
		PriorToken string `json:"prior_token"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("snapshot_diff: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	resolved := t.resolveSessionRef(args.Session)
	body, err := t.Ctl.Capture(ctx, resolved, tmuxctl.CaptureVisible, false)
	if err != nil {
		return nil, internalError(err)
	}
	snap, diffs := t.Snap.DiffSince(resolved, args.PriorToken, body)
	out := make([]map[string]any, 0, len(diffs))
	for _, d := range diffs {
		out = append(out, map[string]any{
			"line":    d.Line,
			"old":     d.Old,
			"new":     d.New,
			"removed": d.Removed,
		})
	}
	return jsonBlock(map[string]any{
		"token":   snap.Token,
		"changed": snap.Changed,
		"diff":    out,
	})
}

func (t *Tools) resize(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session string `json:"session"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("resize: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
	}
	if rerr := validateResizeDims(args.Width, args.Height); rerr != nil {
		return nil, rerr
	}
	if err := t.Ctl.Resize(ctx, t.resolveSessionRef(args.Session), args.Width, args.Height); err != nil {
		return nil, internalError(err)
	}
	return textBlock(fmt.Sprintf("resized %s to %dx%d", args.Session, args.Width, args.Height)), nil
}
