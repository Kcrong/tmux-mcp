# Tool reference

The MCP tool surface tmux-mcp exposes over `tools/list` / `tools/call`.
Schemas are the canonical source of truth and live in
[`internal/server/tools.go`](../internal/server/tools.go); this page is
the human-readable companion.

Every tool is invoked via JSON-RPC 2.0:

```jsonc
{ "jsonrpc": "2.0", "id": 1, "method": "tools/call",
  "params": { "name": "<tool>", "arguments": { /* tool args */ } } }
```

Successful responses return a `content` array with a single text block
whose `text` field is a JSON string (for tools that return structured
data) or a plain status string (for tools that return only "ok"). All
examples below show only the `arguments` object for brevity.

## Error codes

Tool calls fail with a JSON-RPC `error` object. Codes are stable:

| Code     | Sentinel                       | Meaning                                                                          |
| -------- | ------------------------------ | -------------------------------------------------------------------------------- |
| `-32602` | (JSON-RPC standard)            | Malformed `arguments`, bound violation, or invalid enum value.                   |
| `-32603` | (JSON-RPC standard)            | Generic server failure not matched by any sentinel below.                        |
| `-32000` | `errs.ErrSessionNotFound`      | Tool referenced a session this server does not know about.                       |
| `-32001` | `errs.ErrTmuxVersionUnsupported` | tmux on `$PATH` is older than the supported floor.                             |
| `-32002` | `errs.ErrTimeout`              | A polling wait (`wait_for_*`) exceeded its `timeout_ms` budget.                  |
| `-32003` | `context.Canceled` / `DeadlineExceeded` | Caller (or its transport) cancelled the request mid-call.               |
| `-32004` | `errs.ErrSessionExists`        | A session name collides with an existing one (e.g. `session_rename` to a name in use). |

Sentinels live in [`internal/errs`](../internal/errs/errs.go); the
mapping is performed by `errs.CodeOf`.

## Common bounds

These bounds apply across every tool that takes a session reference,
terminal size, or timing argument. Out-of-range inputs are rejected with
`-32602` before any tmux call is issued.

| Field                         | Range / rule                                |
| ----------------------------- | ------------------------------------------- |
| `name` / `session`            | length 1-64, regex `^[A-Za-z0-9_-]+$`       |
| `width`                       | 20-1000 (or 0 to accept the default)        |
| `height`                      | 5-500 (or 0 to accept the default)          |
| `cwd`                         | empty or absolute path                      |
| `*_ms` (`quiet_ms`, `step_ms`, `timeout_ms`) | 0-600000 (10 minute ceiling) |

---

## `session_create`

Start a new detached tmux session running a command at a given
terminal size.

### Input

| Field     | Type     | Required | Default            | Notes                                              |
| --------- | -------- | -------- | ------------------ | -------------------------------------------------- |
| `name`    | string   | yes      | —                  | session id; len 1-64, `[A-Za-z0-9_-]+`             |
| `command` | string   | no       | user's login shell | program to run inside the new session              |
| `cwd`     | string   | no       | server's cwd       | must be absolute when set                          |
| `width`   | integer  | no       | 120                | columns; 20-1000                                   |
| `height`  | integer  | no       | 40                 | rows; 5-500                                        |
| `env`     | object   | no       | `{}`               | extra env vars (string→string)                     |

### Output

A status text block: `session "<name>" created`.

### Errors

- `-32602` — name missing/invalid, dimensions out of range, `cwd` not absolute.
- `-32603` — tmux failed to start the session.

### Example

```jsonc
{ "name": "demo", "command": "/bin/sh", "width": 100, "height": 30 }
```

---

## `session_list`

List the names of every session this server currently manages.

### Input

No fields. Pass `{}`.

### Output

JSON text block: `{"sessions": ["demo", "build", ...]}`. Returns
`{"sessions": []}` when no sessions exist.

### Errors

- `-32603` — tmux failed to enumerate sessions.

### Example

```jsonc
{}
```

---

## `session_kill`

Kill the named session and drop any snapshot history kept for it.

### Input

| Field  | Type   | Required | Notes                                    |
| ------ | ------ | -------- | ---------------------------------------- |
| `name` | string | yes      | len 1-64, `[A-Za-z0-9_-]+`               |

### Output

Status text block: `session "<name>" killed`.

### Errors

- `-32602` — invalid name.
- `-32000` — session does not exist.
- `-32603` — tmux refused to kill.

### Example

```jsonc
{ "name": "demo" }
```

---

## `kill_all_sessions`

Kill every session this server manages and forget all snapshot history
in one shot. The tmux server itself stays running, so the next
`session_create` does not pay the re-spawn cost. Best-effort: a single
broken session does not strand the rest. Useful for agent
error-recovery loops that want a clean slate without restarting the
server process.

### Input

No fields. Pass `{}`.

### Output

JSON text block:

```jsonc
{ "killed": ["demo", "build"], "count": 2 }
```

`killed` is the list of sessions that were torn down successfully.
`count` is `len(killed)` for callers that just want the headline
number.

### Errors

- `-32603` — tmux failed to enumerate sessions before the loop began.

### Example

```jsonc
{}
```

---

## `session_describe`

Return structured metadata for a single session: window count, total
pane count, current width/height (cols × rows), and the creation
timestamp as RFC3339. Useful when an agent needs to confirm a session
layout or correlate logs by creation time.

### Input

| Field  | Type   | Required | Notes                                    |
| ------ | ------ | -------- | ---------------------------------------- |
| `name` | string | yes      | len 1-64, `[A-Za-z0-9_-]+`               |

### Output

JSON text block:

```jsonc
{
  "name":       "demo",
  "windows":    1,
  "panes":      1,
  "width":      120,
  "height":     40,
  "created_at": "2025-01-02T03:04:05Z"
}
```

`width` / `height` are the most-recent window size — accurate for the
detached sessions tmux-mcp owns (where tmux's `client_*` variables
would be empty).

### Errors

- `-32602` — invalid `name`.
- `-32000` — session does not exist.
- `-32603` — tmux failed to describe the session.

### Example

```jsonc
{ "name": "demo" }
```

---

## `session_rename`

Rename an existing session via `tmux rename-session -t OLD NEW`. Useful
when an agent's first label was a placeholder (e.g. `"scratch"`) and
the work has settled into a recognisable identity (e.g.
`"build-3128"`). After the call, every subsequent `session_describe` /
`send_keys` / `capture` must be addressed by the new name; the old
name is gone.

The schema sets `additionalProperties: false`, so any field other than
`name` / `new_name` is rejected with `-32602` (invalid params) before
tmux is consulted — a typo like `"newname"` fails fast.

### Input

| Field      | Type   | Required | Notes                                                  |
| ---------- | ------ | -------- | ------------------------------------------------------ |
| `name`     | string | yes      | existing session name; len 1-64, regex `^[A-Za-z0-9_-]+$` |
| `new_name` | string | yes      | new session name; same regex/length policy as `name`   |

`name` and `new_name` must differ — passing the same value rejects with
`-32602` ("nothing to rename") before tmux is invoked, so the dedicated
`-32004` (`errs.ErrSessionExists`) stays reserved for genuine collisions
with a *different* session.

### Output

JSON text block:

```jsonc
{ "old_name": "scratch", "new_name": "build-3128" }
```

Snapshot history is not migrated to the new key — the next `capture`
against the new name seeds a fresh entry. Callers that depend on a
`snapshot_diff` chain across the rename should treat the first
post-rename diff as a baseline.

### Errors

| Code     | Cause                                                                              |
| -------- | ---------------------------------------------------------------------------------- |
| `-32602` | Missing/invalid `name` / `new_name`, both names equal, or an unknown field was sent. |
| `-32000` | `name` does not exist on this server (`errs.ErrSessionNotFound`).                  |
| `-32004` | `new_name` already names another session on this server (`errs.ErrSessionExists`). |
| `-32603` | tmux refused the rename for an unexpected reason.                                  |

### Example

```jsonc
{ "name": "scratch", "new_name": "build-3128" }
```

Pair with `session_describe` to confirm the new identity surfaced
correctly (and that tools/list reflects only the new name):

```jsonc
{ "name": "session_rename",   "arguments": { "name": "scratch", "new_name": "build-3128" } }
{ "name": "session_describe", "arguments": { "name": "build-3128" } }
```

---

## `session_inspect`

Return process-level metadata for the active pane of a session: the
foreground PID, current working directory, and command name. Useful
for debugging a stuck shell, asserting that the expected program is
still running before sending more keys, or routing follow-up commands
based on the current cwd.

Distinct from a layout-style describe: `session_inspect` reports the
active pane's process state (pid / cwd / command), not session-wide
window/pane geometry. Environment variables are intentionally NOT
exposed because they routinely carry tokens, API keys, or other
secrets that have no business crossing the JSON-RPC boundary.

### Input

| Field     | Type   | Required | Notes                                            |
| --------- | ------ | -------- | ------------------------------------------------ |
| `session` | string | yes      | session id; len 1-64, regex `^[A-Za-z0-9_-]+$`   |

### Output

JSON block:

```jsonc
{ "name": "demo", "pid": 12345, "cwd": "/home/user/repo", "command": "bash" }
```

Fields come straight from a single `tmux display-message` against
`#{pane_pid}` / `#{pane_current_path}` / `#{pane_current_command}`,
so the data is exactly what tmux itself sees. No `/proc` reads, which
keeps the implementation portable to macOS.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Missing/invalid `session`.                                          |
| `-32000` | `session` does not exist on this server (`errs.ErrSessionNotFound`). |
| `-32603` | tmux returned an unparseable response (e.g. `pane_pid` blank).      |

### Example

```jsonc
{ "session": "demo" }
```

Pair with `send_signal` to drive a stuck program: inspect first to
confirm the foreground PID, then signal it directly.

```jsonc
{ "name": "session_inspect", "arguments": { "session": "demo" } }
{ "name": "send_signal",     "arguments": { "session": "demo", "signal": "TERM" } }
```

---

## `send_keys`

Type into a session. Each entry of `keys` is interpreted by tmux: bare
text is sent literally, named keys (`Up`, `Enter`, `Tab`, `C-c`,
`F1`-`F12`, `BSpace`, …) emit the corresponding key event. Set
`literal: true` to disable key-name interpretation and send the raw
characters instead.

### Input

| Field     | Type             | Required | Default | Notes                                |
| --------- | ---------------- | -------- | ------- | ------------------------------------ |
| `session` | string           | yes      | —       | existing session name                |
| `keys`    | array of strings | yes      | —       | non-empty                            |
| `literal` | boolean          | no       | `false` | `true` bypasses tmux key-name parser |

### Output

Status text block: `ok`.

### Errors

- `-32602` — invalid session, empty `keys` array.
- `-32000` — session does not exist.
- `-32603` — tmux send-keys failed.

### Example

```jsonc
{ "session": "demo", "keys": ["echo hello", "Enter"] }
```

---

## `capture`

Read the visible pane (or full scrollback) as text. With `ansi: true`
the result includes terminal escape sequences so the caller can render
colours; otherwise the body is plain text.

### Input

| Field       | Type    | Required | Default     | Notes                                                       |
| ----------- | ------- | -------- | ----------- | ----------------------------------------------------------- |
| `session`   | string  | yes      | —           | existing session name                                       |
| `mode`      | string  | no       | `"visible"` | `"visible"` or `"scrollback"`                               |
| `ansi`      | boolean | no       | `false`     | keep ANSI escape sequences in the body                      |
| `max_lines` | integer | no       | `0`         | `>0` caps to last N lines; `0` = no cap (visible) / 5000 (scrollback) |

`mode=scrollback` defaults to a 5000-line cap so a long-lived shell
cannot return tens of MB through the JSON-RPC frame. When the snapshot
is truncated the *oldest* lines are dropped so the most recent activity
is preserved.

### Output

JSON text block:

```jsonc
{
  "snapshot":  "...",      // captured pane body
  "token":     "ab12cd34", // hand to snapshot_diff later
  "changed":   true,       // body differs from the previous capture for this session
  "truncated": false       // true when max_lines clipped the body
}
```

### Errors

- `-32602` — invalid session, unknown mode, negative `max_lines`.
- `-32000` — session does not exist.
- `-32603` — tmux capture-pane failed.

### Example

```jsonc
{ "session": "demo", "mode": "scrollback", "max_lines": 200 }
```

---

## `wait_for_stable`

Block until the visible pane has been unchanged for `quiet_ms`, then
return the snapshot. Useful for waiting out a TUI redraw before
capturing.

### Input

| Field        | Type    | Required | Default | Notes                          |
| ------------ | ------- | -------- | ------- | ------------------------------ |
| `session`    | string  | yes      | —       | existing session name          |
| `quiet_ms`   | integer | no       | 400     | 0-600000; idle window required |
| `step_ms`    | integer | no       | 100     | 0-600000; poll interval        |
| `timeout_ms` | integer | no       | 10000   | 0-600000 (10 min ceiling)      |

### Output

JSON text block:

```jsonc
{ "snapshot": "...", "token": "ab12cd34" }
```

### Errors

- `-32602` — invalid session, durations out of range.
- `-32000` — session does not exist.
- `-32002` — pane never settled before `timeout_ms`.
- `-32003` — caller cancelled context.
- `-32603` — tmux capture failed mid-poll.

### Example

```jsonc
{ "session": "demo", "quiet_ms": 500, "timeout_ms": 5000 }
```

---

## `wait_for_text`

Block until a Go [RE2](https://pkg.go.dev/regexp/syntax) regex matches
the visible pane. Returns the matched substring plus the snapshot at
match time.

### Input

| Field        | Type    | Required | Default | Notes                       |
| ------------ | ------- | -------- | ------- | --------------------------- |
| `session`    | string  | yes      | —       | existing session name       |
| `pattern`    | string  | yes      | —       | Go regex (RE2)              |
| `step_ms`    | integer | no       | 100     | 0-600000; poll interval     |
| `timeout_ms` | integer | no       | 10000   | 0-600000 (10 min ceiling)   |

### Output

JSON text block:

```jsonc
{
  "match":    "READY-42", // first regex match in the pane body
  "snapshot": "...",      // pane body at match time
  "token":    "ab12cd34"  // snapshot token for snapshot_diff
}
```

### Errors

- `-32602` — invalid session, missing/malformed `pattern`, durations out of range.
- `-32000` — session does not exist.
- `-32002` — pattern never matched before `timeout_ms`.
- `-32003` — caller cancelled context.
- `-32603` — tmux capture failed mid-poll.

### Example

```jsonc
{ "session": "demo", "pattern": "READY-\\d+", "timeout_ms": 30000 }
```

---

## `snapshot_diff`

Capture the visible pane and return only the lines that differ from a
prior capture. Pass an empty `prior_token` on the first call; on
subsequent calls pass the `token` from the previous response.

### Input

| Field         | Type   | Required | Notes                                         |
| ------------- | ------ | -------- | --------------------------------------------- |
| `session`     | string | yes      | existing session name                         |
| `prior_token` | string | no       | empty on first call; reset if older than 2 captures |

### Output

JSON text block:

```jsonc
{
  "token":   "cd34ef56",   // token for the new capture
  "changed": true,         // true when body differs from the previous capture
  "diff": [
    { "line": 3, "old": "before", "new": "after",  "removed": false },
    { "line": 7, "old": "gone",   "new": "",       "removed": true  }
  ]
}
```

History keeps only the **two most recent** captures per session — older
tokens trigger a full reset (every line reported as new).

### Errors

- `-32602` — invalid session.
- `-32000` — session does not exist.
- `-32603` — tmux capture failed.

### Example

```jsonc
{ "session": "demo", "prior_token": "ab12cd34" }
```

---

## `resize`

Resize the session window to the given column × row dimensions.

### Input

| Field     | Type    | Required | Notes               |
| --------- | ------- | -------- | ------------------- |
| `session` | string  | yes      | existing session    |
| `width`   | integer | yes      | columns; 20-1000    |
| `height`  | integer | yes      | rows; 5-500         |

### Output

Status text block: `resized <session> to <width>x<height>`.

### Errors

- `-32602` — invalid session, dimensions out of range.
- `-32000` — session does not exist.
- `-32603` — tmux refresh-client failed.

### Example

```jsonc
{ "session": "demo", "width": 100, "height": 30 }
```

---

## `list_panes`

Enumerate panes visible to this server. Pass `session` to scope the
listing to a single tmux session; omit it to list every pane on the
server. Each entry includes the `session:window` pair plus the pane
index, so callers can build a `session:window.pane` target for
`pane_select` / `send_keys` / `capture`.

### Input

| Field     | Type   | Required | Notes                                                   |
| --------- | ------ | -------- | ------------------------------------------------------- |
| `session` | string | no       | when set, list only panes inside that session           |

### Output

JSON text block:

```jsonc
{
  "panes": [
    {
      "id":          "%0",       // stable tmux #{pane_id}
      "title":       "vim",      // current #{pane_title}
      "session_win": "demo:0",   // build "<session_win>.<index>" for tmux targets
      "index":       0,          // 0-based #{pane_index} within the window
      "active":      true,       // true when this is the active pane of its window
      "width":       120,
      "height":      40
    }
  ]
}
```

### Errors

- `-32602` — `session` provided but malformed.
- `-32603` — tmux list-panes failed.

### Example

```jsonc
{ "session": "demo" }
```

---

The server supports dynamic tool registration: `tools/list` reflects
the live surface, and the server emits
`notifications/tools/list_changed` (per the MCP spec) whenever a tool
is added or removed at runtime — clients see `tools.listChanged: true`
in the `initialize` capabilities reply.

## `list_windows`

Enumerate windows visible to this server. Useful for an agent that
needs to discover the layout of a session before driving it (which
window is focused, how many panes each window holds, what targets are
available for `window_kill` / `send_keys`).

### Input

| Field     | Type   | Required | Notes                                                  |
| --------- | ------ | -------- | ------------------------------------------------------ |
| `session` | string | no       | session id; len 1-64, regex `^[A-Za-z0-9_-]+$`. Omit to list every window on the server (`-a`). |

The schema sets `additionalProperties: false`, so any field other than
`session` is rejected with `-32602` (invalid params) before tmux is
consulted — a typo like `"sesion"` fails fast instead of silently
behaving like the unscoped variant.

### Output

JSON text block with a flat object keyed by `windows`:

```jsonc
{
  "windows": [
    { "index": 0, "name": "bash",  "active": true,  "panes": 1 },
    { "index": 1, "name": "build", "active": false, "panes": 2 }
  ]
}
```

| Field    | Type    | Notes                                                                |
| -------- | ------- | -------------------------------------------------------------------- |
| `index`  | integer | Window index inside its session (0-based). Combine with the session name to form a `session:index` target string. |
| `name`   | string  | Whatever tmux assigned (caller-supplied `-n`, or the auto label).    |
| `active` | boolean | True when this window is the currently focused one of its session.   |
| `panes`  | integer | Number of panes currently in the window.                             |

### Errors

| Code     | Cause                                                                |
| -------- | -------------------------------------------------------------------- |
| `-32602` | `session` present but malformed, or an unknown field was sent.       |
| `-32000` | `session` does not exist on this server (`errs.ErrSessionNotFound`). |
| `-32603` | tmux failed for an unexpected reason (server crashed, IO error).     |

### Examples

```jsonc
// scope to a single session
{ "session": "demo" }

// list every window on the server
{}
```

A typical chain looks like: discover the layout, jump to a specific
window, drive it.

```jsonc
{ "name": "list_windows", "arguments": { "session": "demo" } }
{ "name": "send_keys",    "arguments": { "session": "demo", "keys": ["C-b", "1"] } }
{ "name": "capture",      "arguments": { "session": "demo" } }
```

---

## `list_clients`

Enumerate clients (attached terminals) visible to this server via
`tmux list-clients`. Useful for an agent that needs to know whether a
human is currently watching a session before driving it, or to
sanity-check that a headless tmux server it owns has nothing
attached. The boundary returns each client's controlling TTY, the
session it is bound to, the TERM string it advertised, the current
size, the read-only flag, and an RFC3339 attachment timestamp.

### Input

| Field     | Type   | Required | Notes                                                                              |
| --------- | ------ | -------- | ---------------------------------------------------------------------------------- |
| `session` | string | no       | session id; len 1-64, regex `^[A-Za-z0-9_-]+$`. Omit to list every client on the server. |

The schema sets `additionalProperties: false`, so any field other than
`session` is rejected with `-32602` (invalid params) before tmux is
consulted — a typo like `"sesion"` fails fast instead of silently
behaving like the unscoped variant.

### Output

JSON text block with a flat object keyed by `clients`:

```jsonc
{
  "clients": [
    {
      "tty":           "/dev/pts/3",
      "session":       "demo",
      "term":          "xterm-256color",
      "size":          { "cols": 120, "rows": 40 },
      "readonly":      false,
      "creation_time": "2025-01-02T03:04:05Z"
    }
  ]
}
```

| Field           | Type    | Notes                                                                           |
| --------------- | ------- | ------------------------------------------------------------------------------- |
| `tty`           | string  | Absolute path of the client's controlling terminal device.                      |
| `session`       | string  | Name of the session the client is currently attached to.                        |
| `term`          | string  | TERM value the client advertised when it attached (`#{client_termname}`).       |
| `size.cols`     | integer | Client terminal width in columns at the moment of the listing.                  |
| `size.rows`     | integer | Client terminal height in rows at the moment of the listing.                    |
| `readonly`      | boolean | True when the client attached read-only (`tmux attach -r`).                     |
| `creation_time` | string  | RFC3339 attachment timestamp parsed from `#{client_created}`.                   |

A server with nothing attached returns `{"clients": []}` — a clean
empty list rather than an error — so callers can iterate the response
without a separate "is this an error" branch. This is the load-bearing
case for the headless tmux servers tmux-mcp owns.

### Errors

| Code     | Cause                                                                |
| -------- | -------------------------------------------------------------------- |
| `-32602` | `session` present but malformed, or an unknown field was sent.       |
| `-32000` | `session` does not exist on this server (`errs.ErrSessionNotFound`). |
| `-32603` | tmux failed for an unexpected reason (server crashed, IO error).     |

### Examples

```jsonc
// scope to a single session
{ "session": "demo" }

// list every client on the server
{}
```

Pair with `session_list` to find the live sessions, then ask which
ones have a human watching them:

```jsonc
{ "name": "session_list",  "arguments": {} }
{ "name": "list_clients",  "arguments": { "session": "demo" } }
```

---

## `pane_select`

Make `target` the active pane of its window. Subsequent `send_keys` /
`capture` calls that name the surrounding session will then act on the
newly selected pane. Useful for multi-pane TUIs (vim+terminal split,
zellij-style layouts) where the agent needs to flip focus between
panes between commands.

### Input

| Field    | Type   | Required | Notes                                                |
| -------- | ------ | -------- | ---------------------------------------------------- |
| `target` | string | yes      | tmux `session:window.pane` form (e.g. `demo:0.1`)    |

### Output

Status text block: `ok`.

### Errors

- `-32602` — missing/empty `target`.
- `-32603` — tmux select-pane failed (target malformed or pane gone).

### Example

```jsonc
{ "target": "demo:0.1" }
```

---

## `pane_split`

Split a pane in two via `tmux split-window`. Useful when an agent
wants a side car (build/test/log tail) running next to the main pane
without spawning a new session or window. Pairs with `list_panes` (to
discover the just-created pane) and `pane_select` / `send_keys` (to
drive it).

### Input

| Field         | Type    | Required | Notes                                                                              |
| ------------- | ------- | -------- | ---------------------------------------------------------------------------------- |
| `session`     | string  | yes      | existing session id; len 1-64, regex `^[A-Za-z0-9_-]+$`                            |
| `target_pane` | string  | no       | tmux target form (`session`, `session:window`, or `session:window.pane`); defaults to the active pane of `session` |
| `direction`   | string  | yes      | `"horizontal"` (-h, side-by-side) or `"vertical"` (-v, stacked)                    |
| `command`     | string  | no       | optional initial command, max 4096 chars; defaults to the user's shell              |
| `detach`      | boolean | no       | when `true`, focus stays on the original pane (-d); default `false`                |

### Output

JSON block: `{"id": "%5", "index": 1}`. `id` is the tmux `#{pane_id}`
(stable for the pane's lifetime); `index` is the 0-based
`#{pane_index}` within the window. Combine with the surrounding
session/window pair to build a `session:window.pane` target for
follow-up tools.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Missing/invalid `session`, missing/invalid `direction`, malformed `target_pane`, or `command` longer than 4096 chars. |
| `-32000` | `session` does not exist on this server (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the split (e.g. window already at maximum pane count, command not found in PATH). |

### Example

```jsonc
{ "session": "demo", "direction": "vertical", "command": "tail -f app.log", "detach": true }
```

Pair the call with `list_panes` to confirm the new pane shape, then
target it via `pane_select`:

```jsonc
{ "name": "pane_split",  "arguments": { "session": "demo", "direction": "horizontal", "detach": true } }
{ "name": "list_panes",  "arguments": { "session": "demo" } }
{ "name": "pane_select", "arguments": { "target": "demo:0.1" } }
```

---

## `pane_kill`

Destroy a single pane via `tmux kill-pane -t <target_pane>`. Useful
for tearing down a side-car pane (build/test/log tail) created with
`pane_split` once the agent is done with it. The natural tmux
semantics flow through untouched: killing the only remaining pane of
a window also reaps the window, and if it was the only window of the
session the session itself is destroyed — pre-check with `list_panes`
or `list_windows` when the caller needs to guard against that.

### Input

| Field         | Type   | Required | Notes                                                                              |
| ------------- | ------ | -------- | ---------------------------------------------------------------------------------- |
| `session`     | string | no       | optional, informational; len 1-64, regex `^[A-Za-z0-9_-]+$`. The pane target alone already pins the pane, so an agent that built a fully-qualified `target_pane` doesn't have to repeat it. |
| `target_pane` | string | yes      | tmux target form (`session`, `session:window`, or `session:window.pane`)            |

### Output

JSON block: `{"killed": true}`. The boundary deliberately does not
expose whether the kill collapsed the surrounding window or session —
that information is one `list_panes` / `list_windows` call away if
the caller actually needs it.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Missing/empty `target_pane`, malformed `target_pane`, or invalid `session` if supplied. |
| `-32000` | `target_pane` does not resolve on this server (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the kill for any other reason (e.g. permission, internal tmux failure). |

### Example

```jsonc
{ "target_pane": "demo:0.1" }
```

Pair with `pane_split` and `list_panes` to wind a side-car pane up
and back down again:

```jsonc
{ "name": "pane_split", "arguments": { "session": "demo", "direction": "horizontal", "detach": true } }
{ "name": "list_panes", "arguments": { "session": "demo" } }
{ "name": "pane_kill",  "arguments": { "target_pane": "demo:0.1" } }
```

---

## `clear_history`

Drop the scrollback buffer of a pane via `tmux clear-history -t <target>`.
Useful when a long-running interactive command (build watcher, log
tail) has accumulated megabytes of scrollback that bloats subsequent
`capture` (mode=scrollback) payloads and snapshot diffs. Only the
scrollback is dropped — the visible region is left untouched, the
running process is undisturbed, and the pane id stays valid across
the call.

### Input

| Field    | Type   | Required | Notes                                                                              |
| -------- | ------ | -------- | ---------------------------------------------------------------------------------- |
| `target` | string | yes      | tmux pane-target form (`session`, `session:window`, or `session:window.pane`)      |

### Output

JSON block: `{"cleared": true}`. The boundary deliberately does not
echo a cleared-line count — tmux clear-history reports nothing of the
sort, and a follow-up `capture` is one call away if the agent wants
confirmation.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Missing/empty `target`, or malformed `target` (regex/length policy). |
| `-32000` | `target` does not resolve on this server (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the clear for any other reason (e.g. internal tmux failure). |

### Example

```jsonc
{ "target": "demo:0.1" }
```

A common pattern is to clear right before a fresh `capture` so the
returned snapshot stays small even after hours of activity:

```jsonc
{ "name": "clear_history", "arguments": { "target": "demo" } }
{ "name": "capture",       "arguments": { "session": "demo", "mode": "scrollback" } }
```

---

## `pane_swap`

Exchange two panes in place via `tmux swap-pane -s <src> -t <dst>`. tmux
swaps the layout slots: each pane keeps its `#{pane_id}`, contents, and
running process while the positions trade. Useful for rearranging a
multi-pane TUI layout (e.g. moving the build log next to the editor)
without recreating panes.

### Input

| Field | Type   | Required | Notes                                                          |
| ----- | ------ | -------- | -------------------------------------------------------------- |
| `src` | string | yes      | Source pane target (`session`, `session:window`, or `session:window.pane`) |
| `dst` | string | yes      | Destination pane target (same target forms as `src`)           |

Both targets must match `^[A-Za-z0-9_-]+(:[0-9]+(\.[0-9]+)?)?$` —
the same conservative shape the other pane tools accept.

### Output

Status text block: `ok`.

### Errors

| Code     | Cause                                                                          |
| -------- | ------------------------------------------------------------------------------ |
| `-32602` | Missing/empty `src` or `dst`, or a target that does not match the pane regex.  |
| `-32000` | Either target points at a session this server does not know about (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the swap (e.g. one of the targets resolved to a pane that has gone away). |

### Example

```jsonc
{ "src": "demo:0.0", "dst": "demo:0.1" }
```

Pair with `list_panes` (before and after) when you need to confirm the
layout actually flipped:

```jsonc
{ "name": "list_panes", "arguments": { "session": "demo" } }
{ "name": "pane_swap",  "arguments": { "src": "demo:0.0", "dst": "demo:0.1" } }
{ "name": "list_panes", "arguments": { "session": "demo" } }
```

---

## `pane_join`

Move a pane out of its current window and re-attach it to another
window via `tmux join-pane -s <src> -t <dst>` (with `-h` when
`horizontal` is true). The source pane keeps its `#{pane_id}`,
contents, and running process — only the layout slot changes — so
follow-up `pane_select` / `send_keys` calls against the moved pane see
the new placement immediately. Useful for consolidating panes from
multiple windows back into one (e.g. moving a long-lived REPL out of
its own window and into the editor's window without restarting the
process).

When the donor window has no remaining panes after the join, tmux
reaps it: a `list_windows` call after a join may return one fewer
window than it did before.

### Input

| Field        | Type    | Required | Notes                                                                       |
| ------------ | ------- | -------- | --------------------------------------------------------------------------- |
| `src`        | string  | yes      | Source pane target (e.g. `"mysession:1.0"`)                                 |
| `dst`        | string  | yes      | Destination window target (e.g. `"mysession:0"`)                            |
| `horizontal` | boolean | no       | When true, split the destination left/right (`-h`); default is top/bottom.   |

Both targets must match `^[A-Za-z0-9_-]+(:[0-9]+(\.[0-9]+)?)?$` (or a
tmux `%N` pane id) — the same conservative shape the other pane tools
accept.

### Output

Status text block: `ok`.

### Errors

| Code     | Cause                                                                                              |
| -------- | -------------------------------------------------------------------------------------------------- |
| `-32602` | Missing/empty `src` or `dst`, or a target that does not match the pane regex.                      |
| `-32000` | Either target points at a session/window/pane this server does not know about (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the join (e.g. trying to join a pane to its own window).                              |

### Example

```jsonc
{ "src": "mysession:1.0", "dst": "mysession:0" }
```

Pair with `list_windows` (before and after) when you need to confirm
the donor window was reaped after the move:

```jsonc
{ "name": "list_windows", "arguments": { "session": "mysession" } }
{ "name": "pane_join",    "arguments": { "src": "mysession:1.0", "dst": "mysession:0" } }
{ "name": "list_windows", "arguments": { "session": "mysession" } }
```

---

## `pane_resize`

Resize a pane via `tmux resize-pane -t <target> -{U|D|L|R} <amount>`.
`direction` selects the side the boundary moves toward — `"up"` /
`"down"` shift the horizontal divider (taller / shorter), `"left"` /
`"right"` shift the vertical divider (wider / narrower). Useful for
tweaking a multi-pane TUI layout (e.g. giving the build log more rows
without dropping the editor) without recreating panes.

### Input

| Field       | Type    | Required | Notes                                                                              |
| ----------- | ------- | -------- | ---------------------------------------------------------------------------------- |
| `target`    | string  | yes      | tmux target form (`session`, `session:window`, or `session:window.pane`)            |
| `direction` | string  | yes      | one of `"up"` (-U), `"down"` (-D), `"left"` (-L), `"right"` (-R)                   |
| `amount`    | integer | yes      | number of cells to resize; 1-200                                                    |

`target` must match `^[A-Za-z0-9_-]+(:[0-9]+(\.[0-9]+)?)?$` (or a
tmux `%N` pane id) — the same conservative shape the other pane tools
accept. tmux silently clamps a request that would shrink a pane below
its minimum size, so callers get the largest move tmux will allow
without erroring; `amount > 200` is rejected up front because it is
almost always a typo (pixels mistaken for cells).

### Output

Status text block: `ok`.

### Errors

| Code     | Cause                                                                                          |
| -------- | ---------------------------------------------------------------------------------------------- |
| `-32602` | Missing/empty `target`, malformed `target`, `direction` outside the whitelist, or `amount` outside [1..200]. |
| `-32000` | `target` does not resolve on this server (`errs.ErrSessionNotFound`).                          |
| `-32603` | tmux refused the resize for an unexpected reason.                                              |

### Example

```jsonc
{ "target": "demo:0.1", "direction": "up", "amount": 5 }
```

Pair with `list_panes` (before and after) when you need to confirm the
new size:

```jsonc
{ "name": "list_panes",  "arguments": { "session": "demo" } }
{ "name": "pane_resize", "arguments": { "target": "demo:0.1", "direction": "up", "amount": 5 } }
{ "name": "list_panes",  "arguments": { "session": "demo" } }
```

---

## `pane_break`

Detach a pane from its window into a brand-new window via
`tmux break-pane -P -F "#{window_id}" -s <target>`. tmux moves the
targeted pane out of its current window (which keeps its remaining
panes) and re-homes it as the sole pane of a freshly-created window
on the same session. Useful when an agent split a side-car pane next
to the main work and later wants to give it its own window —
typically to free up the original layout for an unrelated task
without losing the running process.

### Input

| Field    | Type   | Required | Notes                                                                              |
| -------- | ------ | -------- | ---------------------------------------------------------------------------------- |
| `target` | string | yes      | tmux pane-target form (`session`, `session:window`, or `session:window.pane`)      |

`target` must match `^[A-Za-z0-9_-]+(:[0-9]+(\.[0-9]+)?)?$` (or a
tmux `%N` pane id) — the same conservative shape the other pane tools
accept.

### Output

JSON block: `{"window": "@7"}`. `window` is the tmux `#{window_id}`
the new home of the broken-off pane received. Stable for the lifetime
of the window and unique across the whole tmux server, so callers
can hand it straight to `window_select` / `list_panes` / `send_keys`.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Missing/empty `target`, or malformed `target` (regex/length policy). |
| `-32000` | `target` does not resolve on this server (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the break for any other reason (e.g. internal tmux failure). |

### Example

```jsonc
{ "target": "demo:0.1" }
```

Pair with `pane_split` and `list_windows` to wind a side-car pane up
and then promote it into a window of its own:

```jsonc
{ "name": "pane_split",   "arguments": { "session": "demo", "direction": "horizontal", "detach": true } }
{ "name": "pane_break",   "arguments": { "target": "demo:0.1" } }
{ "name": "list_windows", "arguments": { "session": "demo" } }
```

---

## `send_signal`

Deliver a POSIX signal to the PID of the session's currently active
pane. tmux-mcp resolves the PID via `tmux display-message
'#{pane_pid}'` and signals the process directly. More precise than
`send_keys "C-c"` because the signal targets the foreground program
directly — it works even when the program has stolen the keyboard
(raw-mode TUIs, daemons that swallow `Ctrl-C`).

### Input

| Field     | Type   | Required | Notes                                            |
| --------- | ------ | -------- | ------------------------------------------------ |
| `session` | string | yes      | session id; len 1-64, regex `^[A-Za-z0-9_-]+$`   |
| `signal`  | string | yes      | one of the whitelisted signal names (see below)  |

### Signal whitelist

Only signals an agent realistically needs for process control are
exposed. Anything outside this list is rejected with `-32602`
(invalid params) before tmux is consulted.

| Name   | Effect                                                                |
| ------ | --------------------------------------------------------------------- |
| `TERM` | polite shutdown; honoured by most well-behaved CLIs                   |
| `HUP`  | "controlling terminal closed"; daemons usually reload config          |
| `INT`  | same effect as a Ctrl-C keypress, but bypasses raw-mode interception  |
| `QUIT` | like `INT` but produces a core dump on most platforms                 |
| `USR1` | application-defined; many servers use it to rotate logs              |
| `USR2` | application-defined; second app-specific channel                     |
| `KILL` | uncatchable termination; use only when `TERM` failed                  |

The list and its order come from `tmuxctl.SignalNames()` so the
schema, the runtime check, and this table stay in sync.

### Output

Plain text block: `"ok"`.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Missing/invalid `session`, missing/empty `signal`, or `signal` not in the whitelist. |
| `-32000` | `session` does not exist on this server (`errs.ErrSessionNotFound`). |
| `-32603` | tmux failed to resolve `pane_pid`, or the kernel rejected the signal (no such process, permission denied). |

### Example

```jsonc
{ "session": "demo", "signal": "TERM" }
```

Pair `send_signal` with a short `wait_for_stable` to confirm the
program actually exited:

```jsonc
{ "name": "send_signal",      "arguments": { "session": "demo", "signal": "TERM" } }
{ "name": "wait_for_stable",  "arguments": { "session": "demo", "quiet_ms": 200, "timeout_ms": 3000 } }
```

---

## `window_create`

Add a new window to an existing session via `tmux new-window`.
Useful for splitting work across logical contexts (build / test / repl)
inside a single session without spawning extra tmux sessions.

### Input

| Field     | Type    | Required | Notes                                                                              |
| --------- | ------- | -------- | ---------------------------------------------------------------------------------- |
| `session` | string  | yes      | existing session id; len 1-64, regex `^[A-Za-z0-9_-]+$`                            |
| `name`    | string  | no       | optional window label (`-n`); len 1-64, regex `^[A-Za-z0-9_-]+$`. Tmux auto-names when omitted. |
| `command` | string  | no       | optional initial command. Defaults to the session's shell when blank.              |
| `select`  | boolean | no       | when `true` (default) tmux focuses the new window; `false` creates it in the background (`-d`). |

### Output

Plain text block: `window "<name-or-index>" created in "<session>"`.
The label is the explicit `name` when one was supplied, otherwise the
numeric tmux index — both forms are valid targets for follow-up
`window_kill` calls.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Missing/invalid `session`, or `name` outside the regex/length policy. |
| `-32000` | `session` does not exist on this server (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused to create the window (e.g. command not found in PATH). |

### Example

```jsonc
{ "session": "demo", "name": "build", "command": "make watch", "select": false }
```

Chain a follow-up `wait_for_text` against the new window's session if
you need to know when the spawned command settled.

---

## `window_kill`

Destroy a single window via `tmux kill-window -t <session>:<window>`.
Use when an agent is done with a transient build/test pane and wants
to free up the slot without tearing down its sibling work.

### Input

| Field     | Type   | Required | Notes                                                                              |
| --------- | ------ | -------- | ---------------------------------------------------------------------------------- |
| `session` | string | yes      | existing session id; len 1-64, regex `^[A-Za-z0-9_-]+$`                            |
| `window`  | string | yes      | window name (1-64, `^[A-Za-z0-9_-]+$`) or numeric tmux index (`\d+`)               |

### Output

Plain text block: `window "<session>:<window>" killed`.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Missing/invalid `session` or `window`, or the targeted window is the **only** window left in the session — use `session_kill` for that case to keep the boundary between window and session lifecycles distinct. |
| `-32000` | `session` does not exist on this server (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the kill (e.g. window not found in the session).      |

### Example

```jsonc
{ "session": "demo", "window": "build" }
```

For agents that don't track the window inventory locally, pair the
call with the at-a-glance check first:

```jsonc
{ "name": "window_create", "arguments": { "session": "demo", "name": "scratch", "select": false } }
{ "name": "window_kill",   "arguments": { "session": "demo", "window": "scratch" } }
```

---

## `window_select`

Make `target` the active window of `session` via `tmux select-window`.
Subsequent `send_keys` / `capture` calls that name the session will
then act on the newly focused window. Pair with `list_windows` to
discover the available targets when an agent doesn't track the layout
locally.

### Input

| Field     | Type   | Required | Notes                                                                              |
| --------- | ------ | -------- | ---------------------------------------------------------------------------------- |
| `session` | string | yes      | existing session id; len 1-64, regex `^[A-Za-z0-9_-]+$`                            |
| `target`  | string | yes      | window name (1-64, `^[A-Za-z0-9_-]+$`) or numeric tmux index (`\d+`)               |

The schema sets `additionalProperties: false`, so any unknown field is
rejected with `-32602` before tmux is consulted.

### Output

Plain text block: `ok`.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Missing/invalid `session` or `target`, or an unknown field was sent. |
| `-32000` | `session` does not exist on this server (`errs.ErrSessionNotFound`), or no window matches `target`. |
| `-32603` | tmux refused the selection for an unexpected reason.               |

### Example

```jsonc
{ "session": "demo", "target": "build" }
```

A typical chain looks like: discover the layout, jump to a specific
window, drive it.

```jsonc
{ "name": "list_windows",  "arguments": { "session": "demo" } }
{ "name": "window_select", "arguments": { "session": "demo", "target": "build" } }
{ "name": "send_keys",     "arguments": { "session": "demo", "keys": ["make test", "Enter"] } }
```

---

## `window_rename`

Rename a window via `tmux rename-window -t <session>:<target> <name>`.
`target` may be the existing window name or its numeric index; `name`
is the new label and must satisfy the same conservative regex/length
policy as `window_create`'s optional `name`.

### Input

| Field     | Type   | Required | Notes                                                                              |
| --------- | ------ | -------- | ---------------------------------------------------------------------------------- |
| `session` | string | yes      | existing session id; len 1-64, regex `^[A-Za-z0-9_-]+$`                            |
| `target`  | string | yes      | existing window name (1-64, `^[A-Za-z0-9_-]+$`) or numeric tmux index (`\d+`)      |
| `name`    | string | yes      | new window label; len 1-64, regex `^[A-Za-z0-9_-]+$`                               |

### Output

Plain text block: `window "<session>:<target>" renamed to "<name>"`.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Missing/invalid `session`, `target`, or `name`.                    |
| `-32000` | `session` does not exist on this server, or no window matches `target`. |
| `-32603` | tmux refused the rename for an unexpected reason.                  |

### Example

```jsonc
{ "session": "demo", "target": "0", "name": "build" }
```

Pair with `list_windows` to confirm the new label is visible:

```jsonc
{ "name": "window_rename", "arguments": { "session": "demo", "target": "0", "name": "build" } }
{ "name": "list_windows",  "arguments": { "session": "demo" } }
```

---

## `window_move`

Move a window via `tmux move-window -s <src> -t <dst>`. Useful for
renumbering a window inside a session (compacting indices after a kill,
opening up a slot before an `-a`-style insert) or relocating a window
onto another session this server already manages.

### Input

| Field | Type   | Required | Notes                                                                                                                                        |
| ----- | ------ | -------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `src` | string | yes      | source target in `<session>:<window>` form (e.g. `demo:0`); session 1-64 `^[A-Za-z0-9_-]+$`, window may be a name (same regex) or numeric index |
| `dst` | string | yes      | destination target in the same form (e.g. `demo:5`). The window part may be empty (`othersession:`) to let tmux pick the next available index |

### Output

Plain text block: `window "<src>" moved to "<dst>"`.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Missing/invalid `src` or `dst`, or either side outside the regex/length policy. `src` must always carry a non-empty window part. |
| `-32000` | Source session does not exist on this server (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the move — typically because the destination index is already in use ("index in use"). |

### Examples

```jsonc
// renumber within a session
{ "src": "demo:1", "dst": "demo:5" }

// relocate onto another session, letting tmux pick the index
{ "src": "demo:1", "dst": "archive:" }
```

Pair with `list_windows` to confirm the layout after the move:

```jsonc
{ "name": "window_move",  "arguments": { "src": "demo:1", "dst": "demo:5" } }
{ "name": "list_windows", "arguments": { "session": "demo" } }
```

