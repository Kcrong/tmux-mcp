package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
			"means no cap for visible).",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session":   map[string]any{"type": "string"},
				"mode":      map[string]any{"type": "string", "enum": []string{"visible", "scrollback"}, "default": "visible"},
				"ansi":      map[string]any{"type": "boolean", "default": false},
				"max_lines": map[string]any{"type": "integer", "minimum": 0, "default": 0},
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

// Tools holds the dispatch state shared across calls.
type Tools struct {
	Ctl  *tmuxctl.Controller // controller backing every tmux operation the tools issue.
	Snap *snapshot.Store     // per-session capture history powering snapshot_diff.
	// Version is the binary version reported in the MCP initialize
	// response's serverInfo.version. It is populated from main.version
	// (ldflags-injected) at construction time. Empty/zero values fall
	// back to "dev" so the field always carries a sensible string.
	Version string
}

// NewTools wires a Controller and Store together. Any [snapshot.Option]
// args are forwarded verbatim to [snapshot.New], so callers can tune
// behaviour like the snapshot TTL without breaking the zero-arg call
// site (`NewTools(c)`) used by tests and the default deployment.
func NewTools(c *tmuxctl.Controller, opts ...snapshot.Option) *Tools {
	return &Tools{Ctl: c, Snap: snapshot.New(opts...)}
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

// Handle is the dispatcher passed to server.Serve. It implements MCP's
// initialize / tools/list / tools/call surface.
func (t *Tools) Handle(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "tmux-mcp", "version": t.serverVersion()},
		}, nil
	case "notifications/initialized":
		return nil, nil
	case "tools/list":
		return map[string]any{"tools": toolDefs}, nil
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
	case "list_panes":
		return t.listPanes(ctx, call.Arguments)
	case "pane_select":
		return t.paneSelect(ctx, call.Arguments)
	case "session_describe":
		return t.sessionDescribe(ctx, call.Arguments)
	case "send_signal":
		return t.sendSignal(ctx, call.Arguments)
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
	if rerr := validateWidth(args.Width); rerr != nil {
		return nil, rerr
	}
	if rerr := validateHeight(args.Height); rerr != nil {
		return nil, rerr
	}
	if rerr := validateCwd(args.Cwd); rerr != nil {
		return nil, rerr
	}
	spec := tmuxctl.SessionSpec{
		Name: args.Name, Command: args.Command, Cwd: args.Cwd,
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
	if err := t.Ctl.KillSession(ctx, args.Name); err != nil {
		return nil, internalError(err)
	}
	// Drop snapshot history for the dead session so we don't leak
	// per-session entries across many create/kill cycles.
	t.Snap.Forget(args.Name)
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
	if err := t.Ctl.SendKeys(ctx, args.Session, args.Keys, args.Literal); err != nil {
		return nil, internalError(err)
	}
	return textBlock("ok"), nil
}

func (t *Tools) capture(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Session  string `json:"session"`
		Mode     string `json:"mode"`
		ANSI     bool   `json:"ansi"`
		MaxLines int    `json:"max_lines"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, invalidParams("capture: %v", err)
	}
	if rerr := validateSessionRef(args.Session); rerr != nil {
		return nil, rerr
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
	body, err := t.Ctl.Capture(ctx, args.Session, mode, args.ANSI)
	if err != nil {
		return nil, internalError(err)
	}
	body, truncated := capCaptureBody(body, mode, args.MaxLines)
	// Snapshot/diff machinery sees the truncated body so subsequent
	// snapshot_diff calls stay consistent with what the client received.
	snap := t.Snap.Record(args.Session, body)
	return jsonBlock(map[string]any{
		"snapshot":  body,
		"token":     snap.Token,
		"changed":   snap.Changed,
		"truncated": truncated,
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
	body, err := t.Ctl.WaitForStable(
		ctx, args.Session,
		time.Duration(args.QuietMs)*time.Millisecond,
		time.Duration(args.StepMs)*time.Millisecond,
		time.Duration(args.TimeoutMs)*time.Millisecond,
	)
	if err != nil {
		return nil, internalError(err)
	}
	snap := t.Snap.Record(args.Session, body)
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
	m, err := t.Ctl.WaitForText(
		ctx, args.Session, args.Pattern,
		time.Duration(args.StepMs)*time.Millisecond,
		time.Duration(args.TimeoutMs)*time.Millisecond,
	)
	if err != nil {
		return nil, internalError(err)
	}
	snap := t.Snap.Record(args.Session, m.Snapshot)
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
	body, err := t.Ctl.Capture(ctx, args.Session, tmuxctl.CaptureVisible, false)
	if err != nil {
		return nil, internalError(err)
	}
	snap, diffs := t.Snap.DiffSince(args.Session, args.PriorToken, body)
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
	if err := t.Ctl.Resize(ctx, args.Session, args.Width, args.Height); err != nil {
		return nil, internalError(err)
	}
	return textBlock(fmt.Sprintf("resized %s to %dx%d", args.Session, args.Width, args.Height)), nil
}
