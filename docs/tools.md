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
| `-32005` | `errs.ErrPaneActive`           | `respawn_pane` targeted a pane whose original command is still running and `kill` was not set; retry with `kill=true`. |
| `-32010` | `errs.ErrOversizedResponse`    | Marshalled response exceeded the `-max-response-bytes` ceiling; the original payload was suppressed. The underlying call did execute. |

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

## `start_server`

Pre-spawn this controller's tmux daemon via `tmux start-server` without
creating any session. Pairs well with `session_create` — agents that
expect to issue a flurry of `session_create` calls right after startup
can warm the daemon once with `start_server` instead of paying the
spawn cost on the first `session_create`'s critical path.

Idempotent: when a server is already listening on the controller's
socket the call is a no-op and returns success. Deployment scripts can
run it unconditionally on every startup. Mutating in spirit (it spawns
a daemon process), so it is **not** allowed under `-read-only`.

### Input

No fields. Pass `{}` (or `null` — the handler accepts an empty
arguments value).

### Output

JSON text block:

```jsonc
{ "started": true }
```

The ack is identical whether the daemon was just spawned or was
already running because tmux's `start-server` itself does not
distinguish the two.

### Errors

- `-32603` — tmux failed to start the daemon (e.g. socket parent
  directory is not writable).

### Example

```jsonc
{}
```

---

## `kill_server`

Ask the controller's private tmux daemon to exit via
`tmux kill-server`. Unlike `kill_all_sessions` (which iterates over
sessions on a live daemon and leaves the server process up), this tool
tears down tmux itself. The next `session_create` therefore pays the
full re-spawn cost — there is no live daemon left to attach to. The
call is idempotent: when no daemon is running the response is still a
clean ack, so an agent looping in a recovery dance never sees a
spurious failure for a state that is already correct.

> **CAUTION — destroys ALL state on this controller's tmux server.**
> kill_server wipes EVERY session, window, and pane on this socket,
> including unrelated work belonging to other agents that share the
> same controller (`-session-prefix` does NOT scope this tool — tmux
> kill-server has no per-prefix form). Snapshot history for every
> session the daemon was carrying is forgotten. Reach for
> `kill_all_sessions` first if you only need a clean slate within your
> own prefix; reach for `kill_server` only when you actually want the
> daemon process gone.

### Input

No fields. Pass `{}`. The schema sets `additionalProperties: false`,
so any stray field (e.g. a mistakenly-passed `"session"`) is rejected
with `-32602` before tmux is consulted.

### Output

JSON text block:

```jsonc
{ "killed": true }
```

The shape is intentionally minimal. There is no per-session list to
echo back — every session on the daemon is gone — and the boolean is
there so a future addition (e.g. a `count` field for the number of
sessions reaped) can be layered on without breaking callers that read
only the field they care about.

### Errors

- `-32602` — `arguments` carried an unknown field.
- `-32603` — tmux failed to kill the server for an unexpected reason
  (the "no server running" / "error connecting" / "server exited
  unexpectedly" cases are NOT errors — they are the goal state, and
  the tool returns a clean ack).

### Example

```jsonc
{}
```

Pair with a follow-up `session_list` to confirm the daemon is gone
(the listing comes back empty because tmux is no longer accepting
connections on this socket):

```jsonc
{ "name": "kill_server",  "arguments": {} }
{ "name": "session_list", "arguments": {} }
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

## `has_session`

Report whether the named session currently exists on this server.
Wraps tmux's `has-session` primitive — strictly the cheapest path
when the caller only needs a yes/no answer (e.g. before deciding
whether to `session_create` or jump straight to `send_keys`).

A missing session is the literal answer the caller asked for, NOT
an error: only malformed args (`-32602`) or genuine tmux failures
(`-32603`) surface as JSON-RPC errors. This is the load-bearing
contract that makes the tool worth using over `session_list` or
`session_describe` — agents can ask "is X there?" without first
having to catch a `-32000`.

`has_session` is on the read-only allowlist, so a server running
with `-read-only` still exposes it.

### Input

| Field  | Type   | Required | Notes                                    |
| ------ | ------ | -------- | ---------------------------------------- |
| `name` | string | yes      | len 1-64, regex `^[A-Za-z0-9_-]+$`       |

The schema sets `additionalProperties: false`, so any field other
than `name` is rejected with `-32602` before tmux is consulted.

### Output

JSON text block:

```jsonc
{ "exists": true }
```

or

```jsonc
{ "exists": false }
```

The probe is cheap by design: a single tmux IPC and a one-bit
answer. No layout, no PID, no creation time — reach for
`session_describe` / `session_inspect` when those are needed.

### Errors

| Code     | Cause                                                                              |
| -------- | ---------------------------------------------------------------------------------- |
| `-32602` | Missing/invalid `name` (regex/length violation), or an unknown field was sent.     |
| `-32603` | tmux refused the call for a genuine reason (e.g. server crashed, IO error). A non-existent session is **not** an error here — see the "false" output above. |

### Example

```jsonc
{ "name": "demo" }
```

A typical chain looks like: probe before you act, then commit only
when the session is already there.

```jsonc
{ "name": "has_session", "arguments": { "name": "demo" } }
{ "name": "send_keys",   "arguments": { "session": "demo", "keys": ["echo hi", "Enter"] } }
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

## `display_message`

Evaluate a tmux format string via `tmux display-message -p` and return
the resolved single-line value. The canonical introspection escape
hatch for any `#{...}` variable that does not yet have a dedicated
tool — pane titles, window options, server uptime, etc.

The optional `session` / `window` / `pane` combine into a tmux target:

| Provided                | Target string passed to `tmux -t`     |
| ----------------------- | ------------------------------------- |
| `session`               | `<session>`                           |
| `session`+`window`      | `<session>:<window>`                  |
| `session`+`window`+`pane` | `<session>:<window>.<pane>`         |
| (none)                  | (no `-t`; tmux uses current/global)   |

The handler runs an explicit `has-session` probe before the
`display-message` call when `session` is set, so an unknown name
returns the typed `-32000` error instead of silently producing a blank
value (tmux's own `display-message -t <missing>` prints an empty line
without erroring).

### Input

| Field     | Type   | Required | Notes                                                                              |
| --------- | ------ | -------- | ---------------------------------------------------------------------------------- |
| `format`  | string | yes      | tmux format DSL string; max 4096 chars; must not contain literal newlines           |
| `session` | string | no       | session id; len 1-64, regex `^[A-Za-z0-9_-]+$`                                      |
| `window`  | string | no       | window name (1-64, `^[A-Za-z0-9_-]+$`) or numeric index (`\d+`)                     |
| `pane`    | string | no       | numeric pane index (`\d+`) or tmux `%N` pane id                                     |

The schema sets `additionalProperties: false`, so any field other than
the four listed above is rejected with `-32602` before tmux is
consulted.

### Output

JSON text block:

```jsonc
{ "value": "demo 0 %0 /home/user/repo" }
```

`value` is whatever tmux returned for the format, with the trailing
newline that `display-message` always appends stripped. Leading /
trailing whitespace inside the format (e.g. from `#{=10:pane_title}`
padding) is preserved verbatim.

### Errors

| Code     | Cause                                                                              |
| -------- | ---------------------------------------------------------------------------------- |
| `-32602` | Missing `format`, format contains a literal newline, format too long, or any of `session` / `window` / `pane` outside the regex/length policy. |
| `-32000` | `session` does not exist on this server (`errs.ErrSessionNotFound`).               |
| `-32603` | tmux refused the call for an unexpected reason (e.g. malformed format string).     |

### Example

```jsonc
{ "format": "#{session_name} #{window_index} #{pane_id} #{pane_current_path}", "session": "demo" }
```

Pair with `list_panes` / `list_windows` when the agent needs to first
discover a target before reading its format-only fields.

```jsonc
{ "name": "list_panes",      "arguments": { "session": "demo" } }
{ "name": "display_message", "arguments": { "format": "#{pane_title}", "session": "demo", "window": "0", "pane": "1" } }
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

## `show_messages`

Read-only. Returns tmux's per-client message log via
`tmux show-messages [-JT] [-t CLIENT]`. This is the buffer tmux
prints into the bottom status bar — useful for an agent that wants to
introspect what tmux has been telling clients without having to
attach. Each line tmux emitted is one element in the response slice;
trailing `\n` is stripped per entry.

### Input

| Field              | Type    | Required | Notes                                                                                          |
| ------------------ | ------- | -------- | ---------------------------------------------------------------------------------------------- |
| `client`           | string  | no       | client id (TTY path, e.g. `/dev/pts/3`); maps to `-t CLIENT`. Omit on a headless server.       |
| `include_jobs`     | boolean | no       | When `true`, append the job log (`-J`). Defaults to `false`.                                   |
| `include_terminal` | boolean | no       | When `true`, append the terminal log (`-T`). Defaults to `false`.                              |

The schema sets `additionalProperties: false`, so any field other than
the three above is rejected with `-32602` (invalid params) before
tmux is consulted — a typo like `"clinet"` fails fast instead of
silently behaving like the no-arg variant.

### Output

JSON text block with a flat object keyed by `messages`:

```jsonc
{
  "messages": [
    "2025-01-02T03:04:05Z client opened: /dev/pts/3",
    "2025-01-02T03:04:09Z session created: demo"
  ]
}
```

| Field      | Type             | Notes                                                                       |
| ---------- | ---------------- | --------------------------------------------------------------------------- |
| `messages` | array of strings | One entry per line tmux emitted. Empty list when nothing is buffered.       |

A server with no current client (the load-bearing case for the
headless tmux daemons tmux-mcp owns) returns `{"messages": []}` —
a clean empty list rather than an error — so callers can iterate the
response without a separate "is this an error" branch. The same is
true before the daemon has been spun up at all: zero messages exist
by definition, so the call is safe to issue at any point.

### Errors

| Code     | Cause                                                                |
| -------- | -------------------------------------------------------------------- |
| `-32602` | `client` present but malformed, or an unknown field was sent.        |
| `-32000` | `client` does not exist on this server (`errs.ErrSessionNotFound`).  |
| `-32603` | tmux failed for an unexpected reason (server crashed, IO error).     |

### Examples

```jsonc
// no-arg: read the current-client message log (empty on a headless server)
{}

// scope to a specific client and append the job log
{ "client": "/dev/pts/3", "include_jobs": true }

// both -J and -T
{ "include_jobs": true, "include_terminal": true }
```

---

## `detach_client`

Cleanly end one or more tmux client connections via
`tmux detach-client [-a] [-s SESSION] [-t CLIENT]` so the backing
terminal is released. Distinct from `kill_server` (which tears down
the whole daemon) and the future `lock_client` (which holds the
client but keeps the connection): `detach_client` severs the
client/server bond on a per-target basis. This is a **mutating** tool
— it changes the server's client roster — so a `-read-only`
deployment rejects it with `-32005` (`errs.CodeReadOnly`) before the
handler runs.

### Input

| Field     | Type    | Required | Notes                                                                                                                                                              |
| --------- | ------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `client`  | string  | no       | tmux client name (the path-like key shown in `list_clients`, e.g. `/dev/pts/0`); regex `^/[A-Za-z0-9_./:-]+$`, len 1-256. Maps to `-t CLIENT`.                     |
| `session` | string  | no       | tmux session name; detaches every client attached to that session via `-s SESSION`. Same conservative regex as the rest of the surface (alnum/underscore/dash).    |
| `all`     | boolean | no       | When `true`, pass `-a` to detach every OTHER client. Meaningful only with `client` (inverts the selection to "everyone except CLIENT"). Defaults to `false`.       |

At least one of `client` / `session` / `all` must be set — a bare
`{}` is rejected with `-32602` (invalid params) up front rather than
dispatched as `tmux detach-client` (which would target the caller's
"current" client, a concept that does not apply to the headless tmux
servers tmux-mcp owns). The combination `client + all` is valid and
means "detach every other client except CLIENT".

The schema sets `additionalProperties: false`, so any field other
than the three above is rejected with `-32602` before tmux is
consulted — a typo like `"tty"` (instead of `"client"`) fails fast
instead of being silently ignored.

### Output

JSON text block with a flat object keyed by `detached`:

```jsonc
{ "detached": true }
```

| Field      | Type    | Notes                                                                                                                          |
| ---------- | ------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `detached` | boolean | Always `true` on success. The shape leaves room for future extensions (e.g. a count of detached terminals) without breaking callers that read only the boolean. |

A headless server with nothing attached returns `{"detached": true}`
— a clean success rather than an error — so callers can fire-and-
forget a detach without first running `list_clients` to know whether
there is anything to detach. The boundary swallows tmux's
`no current client` stderr in this case.

### Errors

| Code     | Cause                                                                                                                       |
| -------- | --------------------------------------------------------------------------------------------------------------------------- |
| `-32602` | Malformed args (bad regex / over the length cap / all three fields empty) or an unknown field on the schema.                |
| `-32000` | `client` named a terminal that is not currently attached (`errs.ErrSessionNotFound`).                                       |
| `-32005` | Server is running in `-read-only` mode (this tool mutates client state).                                                    |
| `-32603` | tmux failed for an unexpected reason (server crashed, IO error).                                                            |

A non-existent **session** does NOT surface as `-32000`: tmux folds
"no such session" into "no current client" for `detach-client`
specifically, and the boundary cannot distinguish that case from the
legitimate-empty case (session exists, has zero attached clients).
Both fall through to `{"detached": true}`. Callers that need strict
missing-session semantics should pre-flight `has_session`.

### Examples

```jsonc
// detach every client attached to session "demo"
{ "session": "demo" }

// detach a single client by TTY (e.g. found via list_clients)
{ "client": "/dev/pts/0" }

// detach every OTHER client; -a alone reads server-wide
{ "all": true }

// detach every client EXCEPT this one — useful for a "kick everyone
// else out so I have exclusive control" idiom
{ "client": "/dev/pts/0", "all": true }
```

Pair with `list_clients` to discover the live roster before deciding
which terminals to release:

```jsonc
{ "name": "list_clients",  "arguments": { "session": "demo" } }
{ "name": "detach_client", "arguments": { "client": "/dev/pts/0" } }
```

---

## `list_keys`

Enumerate the key bindings on this controller's tmux server via
`tmux list-keys`. Useful for an agent that needs to introspect what a
key chord does before sending it through `send_keys`, or to confirm a
custom binding installed by an init script took effect. Each entry
carries `{ table, key, command }`.

### Input

| Field        | Type    | Required | Notes                                                                                                                                |
| ------------ | ------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| `key_table`  | string  | no       | Keymap name (e.g. `"prefix"`, `"root"`, `"copy-mode"`, `"copy-mode-vi"`); maps to `-T TABLE`. len 1-64, regex `^[A-Za-z0-9_-]+$`. Omit to list every table. |
| `notes_only` | boolean | no       | When true, restrict the listing to bindings annotated with a `bind-key -N` note (`-N`). Default `false`.                             |
| `prefix`     | string  | no       | Optional render-time prefix prepended to every rendered key chord (`-P PREFIX`); only meaningful in notes-only mode. len 1-64.       |

The schema sets `additionalProperties: false`, so any field other than
the three above is rejected with `-32602` (invalid params) before tmux
is consulted — a typo like `"table"` (instead of `"key_table"`) fails
fast instead of silently behaving like the unscoped variant.

### Output

JSON text block with a flat object keyed by `keys`:

```jsonc
{
  "keys": [
    {
      "table":   "prefix",
      "key":     "C-b",
      "command": "send-prefix"
    },
    {
      "table":   "prefix",
      "key":     "?",
      "command": "List key bindings"
    }
  ]
}
```

| Field     | Type   | Notes                                                                                                                                                     |
| --------- | ------ | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `table`   | string | The keymap the binding lives in. Empty only in `notes_only` mode without a `key_table` filter (tmux drops the column from its `-N` output in that case). |
| `key`     | string | The key chord (e.g. `C-a`, `M-{`, `Space`, `Enter`). When `prefix` is set, the chord is rendered with that prefix prepended verbatim.                    |
| `command` | string | The action the binding triggers. In the default rendering this is a tmux command line; in `notes_only` mode it is the binding's `-N` note text instead.  |

A listing with no matching bindings (common for narrow filters)
returns `{"keys": []}` — a clean empty list rather than an error — so
callers can iterate the response without a separate "is this an error"
branch.

### Errors

| Code     | Cause                                                                                                                |
| -------- | -------------------------------------------------------------------------------------------------------------------- |
| `-32602` | `key_table` or `prefix` malformed / out of range, or an unknown field was sent.                                      |
| `-32603` | tmux failed for an unexpected reason (e.g. `key_table` names a table that does not exist, server crashed, IO error). |

### Examples

```jsonc
// Every binding, default rendering.
{}

// Just the prefix table.
{ "key_table": "prefix" }

// Annotated bindings only, with a leading "C-b " in the rendered key.
{ "notes_only": true, "prefix": "C-b " }
```

Pair with `send_keys` once you've discovered which chord drives a
given action — list_keys answers "what does C-b ? do?" so the agent
doesn't have to memorise the default tmux key map:

```jsonc
{ "name": "list_keys", "arguments": { "key_table": "prefix" } }
{ "name": "send_keys", "arguments": { "session": "demo", "keys": ["C-b", "?"] } }
```

---

## `unbind_key`

Remove a tmux key binding via `tmux unbind-key [-a] [-T TABLE] [KEY]`.
Sister of `bind_key` and `list_keys`: pass `key` to remove a single
chord, or set `all=true` to wipe every binding in the targeted table
(`-a`). Useful for an agent tearing down a custom binding set installed
earlier in the session, or for a recovery loop that wants to flush a
custom keymap before re-registering it from scratch.

### Input

| Field       | Type    | Required | Notes                                                                                                                              |
| ----------- | ------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| `key`       | string  | no\*     | Key chord to remove (e.g. `"C-a"`, `"F12"`, `"M-{"`). Mutually exclusive with `all=true`. len 1-256, no NUL or ASCII control bytes. |
| `key_table` | string  | no       | Keymap name (e.g. `"prefix"`, `"root"`, `"copy-mode"`); maps to `-T TABLE`. len 1-64, regex `^[A-Za-z0-9_-]+$`. Omit to use tmux's default table for the operation. |
| `all`       | boolean | no\*     | When true, remove every binding in the targeted table (`-a`). Mutually exclusive with `key`. Default `false`.                       |

\* Exactly one of `{key set, all=true}` is required: both empty would
silently no-op on tmux (a buggy caller's unbind never lands), and both
set contradict each other (tmux silently swallows the KEY when `-a` is
present). The handler refuses both shapes with `-32602` (invalid params).

The schema sets `additionalProperties: false`, so any field other than
the three above is rejected with `-32602` (invalid params) before tmux
is consulted — a typo like `"table"` (instead of `"key_table"`) fails
fast instead of silently behaving like the unscoped variant.

### Output

JSON text block with a flat ack object:

```jsonc
{ "unbound": true }
```

Idempotent by design. tmux's `unbind-key` itself emits `table TABLE
doesn't exist` (when the targeted custom table has been wiped) and
`unknown key: KEY` (when the chord was never bound) for the
"already-gone" shapes; the boundary swallows both so a recovery loop
re-issuing the same teardown frame does not see a spurious failure on
the second iteration. `unbound: true` is returned uniformly whether the
binding existed or not.

### Errors

| Code     | Cause                                                                                                                                 |
| -------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| `-32602` | Neither `key` nor `all=true` set; or both set; or `key`/`key_table` malformed (length, regex, control bytes); or an unknown field was sent. |
| `-32603` | tmux failed for an unexpected reason (server crashed, IO error, etc.). Idempotent shapes (`table doesn't exist`, `unknown key`) are NOT errors. |

### Examples

```jsonc
// Remove a single binding from a custom table.
{ "key": "F12", "key_table": "agent-table" }

// Wipe every binding in a custom table.
{ "key_table": "agent-table", "all": true }

// Remove a chord from tmux's default key table for the operation
// (no `-T` flag, no `-a`).
{ "key": "C-a" }
```

Pair with `bind_key` to install bindings, and `list_keys` to confirm
the teardown landed:

```jsonc
{ "name": "bind_key",   "arguments": { "key": "F12", "key_table": "agent", "command": "display-message hi" } }
{ "name": "unbind_key", "arguments": { "key": "F12", "key_table": "agent" } }
{ "name": "list_keys",  "arguments": { "key_table": "agent" } } // empty after teardown
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

## `move_pane`

Relocate a single pane to a different slot, window, or session via
`tmux move-pane -s <src> -t <dst>` (with `-h` / `-b` / `-d` selected by
the boolean knobs). Distinct from
[`pane_swap`](#pane_swap) (which trades two existing panes in place,
leaving counts unchanged) and
[`pane_break`](#pane_break) (which detaches a pane into its own
brand-new window): `move_pane` takes one source pane and re-homes it
next to the destination, splitting the destination to make room. The
source pane keeps its `#{pane_id}`, contents, and running process —
only the layout slot changes — so follow-up `pane_select` /
`send_keys` calls against the moved pane see the new placement
immediately.

When the donor window has no remaining panes after the move, tmux
reaps it: a `list_windows` call after the move may return one fewer
window than it did before.

### Input

| Field        | Type    | Required | Notes                                                                          |
| ------------ | ------- | -------- | ------------------------------------------------------------------------------ |
| `src`        | string  | yes      | Source pane target (`session`, `session:window`, `session:window.pane`, or `%N`) |
| `dst`        | string  | yes      | Destination pane target (same target forms as `src`)                           |
| `horizontal` | boolean | no       | When true, split the destination left/right (`-h`); default is top/bottom.     |
| `before`     | boolean | no       | When true, insert the moved pane before the destination (`-b`); default is after. |
| `no_focus`   | boolean | no       | When true, do not change the active pane after the move (`-d`).                |

Both targets must match `^[A-Za-z0-9_-]+(:[0-9]+(\.[0-9]+)?)?$` (or a
tmux `%N` pane id) — the same conservative shape the other pane tools
accept.

### Output

JSON block: `{"moved": true, "src": "<src>", "dst": "<dst>"}`. The
echoed `src` / `dst` are the logical (caller-supplied) values, so a
`-session-prefix` deployment never leaks the prefixed identity back to
the caller.

### Errors

| Code     | Cause                                                                                              |
| -------- | -------------------------------------------------------------------------------------------------- |
| `-32602` | Missing/empty `src` or `dst`, or a target that does not match the pane regex.                      |
| `-32000` | Either target points at a session/window/pane this server does not know about (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the move for any other reason.                                                        |

### Example

```jsonc
// Default: move into the destination window with a top/bottom split,
// inserted after the destination, focus follows.
{ "src": "demo:1.0", "dst": "demo:0.0" }

// Horizontal split, place moved pane before destination, leave focus alone.
{ "src": "demo:1.0", "dst": "demo:0.0", "horizontal": true, "before": true, "no_focus": true }
```

Pair with `list_windows` (before and after) when you need to confirm
the donor window was reaped after the move:

```jsonc
{ "name": "list_windows", "arguments": { "session": "demo" } }
{ "name": "move_pane",    "arguments": { "src": "demo:1.0", "dst": "demo:0.0", "no_focus": true } }
{ "name": "list_windows", "arguments": { "session": "demo" } }
```

---

## `respawn_pane`

Restart the command running in an existing pane via
`tmux respawn-pane [-k] -t <session>:<window>.<pane> [command]`. The
pane id, layout slot, and surrounding window are all preserved — only
the foreground process changes. Useful when a long-running command
inside a pane has exited (e.g. a build watcher crashed, a REPL
quit) and the agent wants to bring it back without recreating the
pane and re-shuffling the layout. When `command` is omitted tmux
re-runs whatever was used to start the pane originally; when it is
set, tmux executes it via `/bin/sh -c` exactly as it would for
`session_create` / `pane_split`.

### Input

| Field     | Type    | Required | Notes                                                                              |
| --------- | ------- | -------- | ---------------------------------------------------------------------------------- |
| `session` | string  | yes      | session id; len 1-64, regex `^[A-Za-z0-9_-]+$`                                     |
| `window`  | string  | yes      | window name (`^[A-Za-z0-9_-]+$`, len 1-64) or numeric index (`^[0-9]+$`)           |
| `pane`    | string  | yes      | pane index (`^[0-9]+$`) or tmux `%N` pane id (`^%[0-9]+$`)                         |
| `command` | string  | no       | optional command (≤ 4096 bytes, no newlines); empty re-runs the pane's original    |
| `kill`    | boolean | no       | when `true`, tmux SIGKILLs the running process before respawning (`-k`); default `false` |

`command` is forwarded to tmux as a single trailing argv and run via
`/bin/sh -c` on the tmux side. Newlines (`\n` / `\r`) are rejected
up front — they would otherwise break the "single command" contract
when tmux hands the string to the shell.

### Output

JSON block: `{"respawned": true}`. The pane keeps its tmux pane id
(`%N`), layout, and surrounding window — only the foreground process
is replaced. Follow up with `capture` / `wait_for_text` if you need to
observe the restarted command's output.

### Errors

| Code     | Cause                                                                              |
| -------- | ---------------------------------------------------------------------------------- |
| `-32602` | Missing/malformed `session` / `window` / `pane`, or `command` with newline / over 4096 bytes. |
| `-32000` | `session:window.pane` does not resolve on this server (`errs.ErrSessionNotFound`). |
| `-32005` | Pane is still running its original command and `kill` was not set (`errs.ErrPaneActive`). Retry with `kill=true`. |
| `-32603` | tmux refused the respawn for any other reason (e.g. internal tmux failure).        |

### Example

Bring a crashed build watcher back to life without reshuffling the
layout. The first attempt without `kill` will trip `-32005` if the
old process is still running; the typed code lets the agent recover
deterministically:

```jsonc
{ "name": "respawn_pane",
  "arguments": { "session": "demo", "window": "0", "pane": "1",
                 "command": "npm run watch" } }

// If the previous command is still active, retry with kill=true:
{ "name": "respawn_pane",
  "arguments": { "session": "demo", "window": "0", "pane": "1",
                 "command": "npm run watch", "kill": true } }
```

Omit `command` to re-run whatever the pane was originally started
with (typically the user's default shell):

```jsonc
{ "name": "respawn_pane",
  "arguments": { "session": "demo", "window": "0", "pane": "1" } }
```

---

## `select_layout`

Apply a preset or stored pane layout to a window via
`tmux select-layout -t <session>:<window> [-n] [-p] [-E] [layout]`.
`layout` accepts either one of the five preset names tmux ships out of
the box (`even-horizontal`, `even-vertical`, `main-horizontal`,
`main-vertical`, `tiled`) or a stored layout dump string previously
read from `#{window_layout}` (the value `display_message` /
`list-windows` surfaces). Optional `next` (-n) and `previous` (-p)
cycle through the preset ring; optional `spread` (-E) spreads the
current pane and its neighbours out evenly. Pairs with `pane_split`
to populate the panes a layout will reshape and with `display_message`
(`#{window_layout}`) to dump the post-call layout for later restore.

### Input

| Field      | Type    | Required | Default | Notes                                                                                          |
| ---------- | ------- | -------- | ------- | ---------------------------------------------------------------------------------------------- |
| `target`   | string  | yes      | —       | window target in `<session>:<window>` form (e.g. `demo:0`); session 1-64 `^[A-Za-z0-9_-]+$`, window may be a name (same regex) or numeric index. |
| `layout`   | string  | yes      | —       | preset name or stored layout dump; len ≤ 4096, newlines refused.                               |
| `next`     | boolean | no       | `false` | when true, cycle to the next preset layout (`-n`).                                             |
| `previous` | boolean | no       | `false` | when true, cycle to the previous preset layout (`-p`).                                         |
| `spread`   | boolean | no       | `false` | when true, spread the current pane and its neighbours out evenly (`-E`).                       |

`next` and `previous` are mutually exclusive — passing both rejects
with `-32602` ("next and previous are mutually exclusive") before tmux
is consulted. The schema sets `additionalProperties: false`, so any
unknown field is rejected up front.

`select_layout` mutates tmux state and is therefore NOT in the
read-only allowlist: a server started with `-read-only` will reject
the call with `-32011` (`errs.ErrReadOnly`).

### Output

JSON text block:

```jsonc
{ "selected": true }
```

The boundary deliberately does not echo the resulting layout dump — a
follow-up `display_message` against `#{window_layout}` is one call
away if the caller wants to capture the dump for later restore.

### Errors

| Code     | Cause                                                                              |
| -------- | ---------------------------------------------------------------------------------- |
| `-32602` | Missing/invalid `target` or `layout`; `target` not in `<session>:<window>` form; `layout` empty / over 4096 bytes / contains newlines; `next` and `previous` both true; or an unknown field was sent. |
| `-32000` | `session` does not exist on this server, or the targeted window does not match (`errs.ErrSessionNotFound`). |
| `-32011` | Server is running with `-read-only` and refuses mutating tools (`errs.ErrReadOnly`). |
| `-32603` | tmux refused the layout for an unexpected reason (e.g. a stored dump that does not match the current pane count). |

### Examples

```jsonc
// Apply the "tiled" preset to the first window of the session.
{ "name": "select_layout",
  "arguments": { "target": "demo:0", "layout": "tiled" } }

// Cycle to the next preset (anchor on a known preset first if the
// ring's starting position matters).
{ "name": "select_layout",
  "arguments": { "target": "demo:0", "layout": "tiled", "next": true } }

// Restore a layout previously dumped via `#{window_layout}`.
{ "name": "select_layout",
  "arguments": { "target": "demo:0",
                 "layout": "bb62,159x48,0,0{79x48,0,0,79x48,80,0}" } }
```

A typical capture-and-restore chain:

```jsonc
{ "name": "display_message",
  "arguments": { "session": "demo", "window": "0",
                 "format": "#{window_layout}" } }
// ... later, after experimenting with other presets ...
{ "name": "select_layout",
  "arguments": { "target": "demo:0", "layout": "<saved value>" } }
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

## `new_window`

Create a new window inside an existing session via `tmux new-window`
and return the structured identity tmux assigned. Use when an agent
wants the freshly created window's stable id (`@N`) so a follow-up
tool can address it without needing a separate `list_windows` call.
For comparison, `window_create` covers the same flag mapping but
returns a human-readable text block — pick whichever response shape
your agent prefers.

### Input

| Field         | Type    | Required | Notes                                                                                                       |
| ------------- | ------- | -------- | ----------------------------------------------------------------------------------------------------------- |
| `session`     | string  | yes      | existing session id; len 1-64, regex `^[A-Za-z0-9_-]+$`                                                     |
| `name`        | string  | no       | optional window label (`-n`); len 1-64, regex `^[A-Za-z0-9_-]+$`. Tmux auto-names when omitted.             |
| `command`     | string  | no       | optional initial command. Defaults to the user's shell when blank. Newlines (`\n` / `\r`) are refused up front so tmux's command parser cannot silently split it. |
| `after_index` | integer | no       | when set, insert the new window after this existing window index (`-t <session>:<after_index>`); omit to let tmux append at the next free slot. Must be `>= 0`. |
| `select`      | boolean | no       | when `true` (default) tmux focuses the new window; `false` creates it in the background (`-d`).             |

The schema sets `additionalProperties: false`, so unknown fields are
rejected with `-32602` before any tmux call runs.

### Output

JSON text block:

```jsonc
{ "session": "demo", "window_index": 2, "window_id": "@7", "window_name": "build" }
```

`window_id` is tmux's `#{window_id}` value (e.g. `@7`) — stable
across renames and renumberings, so prefer it for long-lived
references. `window_index` is the numeric `<session>:<index>` slot.
`window_name` echoes the explicit `name` you supplied or the label
tmux auto-assigned from the command's basename.

### Errors

| Code     | Cause                                                                                            |
| -------- | ------------------------------------------------------------------------------------------------ |
| `-32602` | Missing/invalid `session`, `name` outside the regex/length policy, `command` containing newlines, or negative `after_index`. |
| `-32000` | `session` does not exist on this server (`errs.ErrSessionNotFound`).                             |
| `-32603` | tmux refused to create the window (e.g. command not found in PATH, `after_index` slot conflicts).|

### Example

```jsonc
{ "session": "demo", "name": "build", "command": "make watch", "after_index": 0, "select": false }
```

Chain the structured response into another tool call by reaching for
the stable `window_id` rather than the numeric index — that way a
later `window_move` or rename does not invalidate the reference:

```jsonc
{ "name": "new_window",   "arguments": { "session": "demo", "name": "build" } }
{ "name": "send_keys",    "arguments": { "session": "demo", "keys": ["echo built", "Enter"] } }
```

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

## `kill_window`

Destroy a single window via `tmux kill-window -t <session>:<window>`,
honouring tmux's natural cascade: when the targeted window is the only
window left in the session, killing it also reaps the session and the
response surfaces that fact via `session_killed: true` instead of
rejecting the call. Pairs with `pane_kill` and `kill_all_sessions` to
round out the kill-X surface.

Distinct from the older `window_kill` tool: `window_kill` refuses to
destroy the only window of a session (returns `-32602` with a
"`session_kill` instead" hint), while `kill_window` lets the cascade
fire and reports it explicitly. Both tools coexist so callers can pick
the contract that suits them — UI-style flows that want to keep
session and window lifecycles strictly separate stick with
`window_kill`; bulk-cleanup loops that want a single tool to "make
this window go away whatever the consequences" use `kill_window`.

### Input

| Field     | Type   | Required | Notes                                                                              |
| --------- | ------ | -------- | ---------------------------------------------------------------------------------- |
| `session` | string | yes      | existing session id; len 1-64, regex `^[A-Za-z0-9_-]+$`                            |
| `window`  | string | yes      | window name (1-64, `^[A-Za-z0-9_-]+$`) or numeric tmux index (`\d+`)               |

The schema sets `additionalProperties: false`, so any field other than
`session` / `window` is rejected with `-32602` (invalid params) before
tmux is consulted.

### Output

JSON block. The common case (window goes away, session lives on):

```jsonc
{ "killed": true }
```

The cascade case (window was the last one, session reaped along with
it):

```jsonc
{ "killed": true, "session_killed": true }
```

`session_killed` is **only** present when the cascade fires, so
agents that branch on its presence don't have to filter out a noisy
`false` in the common case. Snapshot history kept for the reaped
session is dropped automatically, mirroring the `session_kill`
cleanup contract — the next `capture` against a fresh session of the
same name seeds a new entry.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Missing/invalid `session` or `window`, or an unknown field was sent. |
| `-32000` | `session` does not exist on this server (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the kill (e.g. window not found in the session).      |

### Example

```jsonc
{ "session": "demo", "window": "build" }
```

A bulk-cleanup loop that walks `list_windows` output and asks tmux to
remove each entry — without caring whether the final pass also ends
the session — looks like:

```jsonc
{ "name": "list_windows", "arguments": { "session": "demo" } }
{ "name": "kill_window",  "arguments": { "session": "demo", "window": "build" } }
{ "name": "kill_window",  "arguments": { "session": "demo", "window": "test" } }
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

---

## `show_options`

Return the resolved tmux option set at a given scope. Wraps
`tmux show-options` and parses its line-oriented output into a flat
`key → value` map. Useful for confirming the runtime configuration an
agent is operating against (status-line format, default-shell,
escape-time, …) without forcing the caller to spawn a subshell.

Scopes mirror tmux's own flag set:

- `server` — server-wide options (`tmux show-options -s`). The session
  and window arguments are ignored — server options are global to the
  tmux server process and have no session/window qualifier.
- `session` — per-session options for the named session
  (`tmux show-options -t SESSION`). Set `global: true` to fall back to
  the session-option defaults (`-g`); without it, only overrides set
  on this specific session are returned (which is often empty on a
  freshly created session).
- `window` — per-window options for `SESSION:WINDOW`
  (`tmux show-options -w -t SESSION:WINDOW`). `global: true` again
  returns the window-option defaults.

### Input

| Field     | Type    | Required                           | Default | Notes                                                                                            |
| --------- | ------- | ---------------------------------- | ------- | ------------------------------------------------------------------------------------------------ |
| `scope`   | string  | yes                                | —       | one of `server`, `session`, `window`                                                             |
| `session` | string  | yes when `scope` is `session`/`window` | —       | session id; len 1-64, regex `^[A-Za-z0-9_-]+$`                                                   |
| `window`  | string  | yes when `scope` is `window`       | —       | window name (same regex as `session`) or numeric index                                           |
| `global`  | boolean | no                                 | `false` | when true, query tmux's `-g` defaults instead of the override map. Ignored for `scope=server`. |

### Output

JSON block:

```jsonc
{
  "options": {
    "buffer-limit": "50",
    "default-terminal": "tmux-256color",
    "command-alias[2]": "\"server-info=show-messages -JT\"",
    "history-file": "''"
  }
}
```

Values are returned verbatim — including any quoting tmux emits for
strings with embedded spaces or specials — so callers wanting a
normalised representation can do that on top of the raw map. Lines
without a value (rare, but possible for empty array-style options) are
recorded with an empty-string value so an option name is never
silently dropped.

### Errors

| Code     | Cause                                                                                                                |
| -------- | -------------------------------------------------------------------------------------------------------------------- |
| `-32602` | Missing/unknown `scope`; `session` missing for `scope=session`/`window`; `window` missing for `scope=window`; bound violation on `session`/`window`. |
| `-32000` | Referenced session (or session for the window target) does not exist on this server (`errs.ErrSessionNotFound`).     |
| `-32603` | tmux refused the call for any other reason.                                                                          |

### Examples

```jsonc
// Server-wide options.
{ "scope": "server" }

// Session overrides for the "demo" session (often empty without prior set-option).
{ "scope": "session", "session": "demo" }

// Session-option defaults (always populated).
{ "scope": "session", "session": "demo", "global": true }

// Window-option defaults for the first window of "demo".
{ "scope": "window", "session": "demo", "window": "0", "global": true }
```

Pair with `session_describe` when triaging an unexpected behaviour:
describe reports the layout, `show_options` reports the configuration
that produced it.

```jsonc
{ "name": "session_describe", "arguments": { "name": "demo" } }
{ "name": "show_options",     "arguments": { "scope": "session", "session": "demo", "global": true } }
```

---

## `set_window_option`

Set or clear a tmux **window** option via
`tmux set-window-option [-aFgoqu] [-t TARGET] OPTION VALUE`. Window
options live on tmux's per-window table — `synchronize-panes`,
`automatic-rename`, `mode-keys`, `pane-border-format`, etc. — distinct
from server / session option scopes. This tool is the write counterpart
to `show_options` with `scope=window`: read with one, write with the
other.

### Input

| Field           | Type    | Required                                       | Default | Notes                                                                                                  |
| --------------- | ------- | ---------------------------------------------- | ------- | ------------------------------------------------------------------------------------------------------ |
| `name`          | string  | yes                                            | —       | option name; len 1-64, regex `^[A-Za-z][A-Za-z0-9_-]*$`                                                |
| `value`         | string  | yes when `unset` is `false`                    | —       | option value; len 0-4096, no NUL, no C0 controls except `\t` / `\n` / `\r`                             |
| `target`        | string  | yes when `global` is `false`                   | —       | window target, typically `SESSION:WINDOW`; same regex/length policy as other window-targeting tools     |
| `append`        | boolean | no                                             | `false` | when `true`, append to a string-list option (`-a`); ignored otherwise                                  |
| `format_expand` | boolean | no                                             | `false` | when `true`, run the value through tmux's `#{format}` substitution before storing (`-F`)               |
| `global`        | boolean | no                                             | `false` | when `true`, modify the global window-options table (`-g`); `target` is then optional                  |
| `allow_missing` | boolean | no                                             | `false` | when `true`, suppress unknown-option diagnostics (`-q`); useful for forward/backward compatible config |
| `unset`         | boolean | no                                             | `false` | when `true`, clear the override (`-u`); `value` is ignored and may be omitted                          |

The schema sets `additionalProperties: false`, so any unknown field is
rejected at the schema layer.

### Output

JSON text block:

```jsonc
// Normal set:
{ "set": true, "unset": false, "name": "synchronize-panes" }

// Unset (clearing an override):
{ "set": false, "unset": true, "name": "synchronize-panes" }
```

The boundary deliberately does not echo the resolved value back —
follow up with `show_options` (`scope=window`) when you need to confirm
what tmux actually stored.

### Errors

| Code     | Cause                                                                                                                                          |
| -------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `-32602` | Missing/invalid `name`; missing `value` when `unset=false`; oversized value; NUL/control character in value; missing `target` when `global=false`. |
| `-32000` | Referenced session/window does not exist on this server (`errs.ErrSessionNotFound`).                                                           |
| `-32603` | tmux refused the call for any other reason (e.g. unknown option without `allow_missing=true`, version mismatch).                               |

### Examples

```jsonc
// Enable synchronize-panes for the active window of session "demo".
{ "target": "demo:0", "name": "synchronize-panes", "value": "on" }

// Append to the pane-border-format string-list option.
{ "target": "demo:0", "name": "pane-border-format", "value": "+EXTRA", "append": true }

// Modify the global window-options table (no target needed under global=true).
{ "name": "mode-keys", "value": "vi", "global": true }

// Clear a per-window override.
{ "target": "demo:0", "name": "synchronize-panes", "unset": true }
```

Pair with `show_options` (`scope=window`) before and after a write
when you want to confirm the option actually landed:

```jsonc
{ "name": "set_window_option", "arguments": { "target": "demo:0", "name": "synchronize-panes", "value": "on" } }
{ "name": "show_options",      "arguments": { "scope": "window", "session": "demo", "window": "0" } }
```

---

## `swap_window`

Exchange two windows of the same session in place via
`tmux swap-window -s <session>:<src> -t <session>:<dst>`. tmux trades
the layout slots: each window keeps its `#{window_id}`, contents,
panes, and running processes while the position indices/names trade.
Pairs with `pane_swap` (panes inside a window) and `window_move`
(relocation across sessions or to a new index).

### Input

| Field       | Type    | Required | Default | Notes                                                                              |
| ----------- | ------- | -------- | ------- | ---------------------------------------------------------------------------------- |
| `session`   | string  | yes      | —       | existing session id; len 1-64, regex `^[A-Za-z0-9_-]+$`                            |
| `src`       | string  | yes      | —       | source window name (1-64, `^[A-Za-z0-9_-]+$`) or numeric index (`\d+`)             |
| `dst`       | string  | yes      | —       | destination window name (same regex/length policy as `src`) or numeric index       |
| `no_select` | boolean | no       | `false` | when `true`, do not change the active window after the swap (tmux's `-d` flag)     |

`src` and `dst` must differ — passing the same value rejects with
`-32602` ("src and dst must differ") before tmux is consulted, so the
boundary surfaces a more informative error than tmux's own no-op
refusal. The schema sets `additionalProperties: false`, so any unknown
field is rejected up front.

`no_select=true` is the most useful setting for autonomous agents: it
prevents tmux from shifting the active window pointer to follow the
swap, keeping a chained `send_keys` / `capture` deterministic.

### Output

JSON text block:

```jsonc
{ "swapped": true }
```

The boundary deliberately does not echo the post-swap layout — a
follow-up `list_windows` is one call away if the caller wants to
confirm the new index/name pairing.

### Errors

| Code     | Cause                                                                              |
| -------- | ---------------------------------------------------------------------------------- |
| `-32602` | Missing/invalid `session`, `src`, or `dst`; `src == dst`; or an unknown field was sent. |
| `-32000` | `session` does not exist on this server, or one of the targets does not match a window (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the swap for an unexpected reason.                                    |

### Example

```jsonc
{ "session": "demo", "src": "0", "dst": "1", "no_select": true }
```

Pair with `list_windows` (before and after) when you need to confirm
the layout actually flipped:

```jsonc
{ "name": "list_windows", "arguments": { "session": "demo" } }
{ "name": "swap_window",  "arguments": { "session": "demo", "src": "0", "dst": "1", "no_select": true } }
{ "name": "list_windows", "arguments": { "session": "demo" } }
```

---

## `choose_tree`

Snapshot the (session, window) tree this server's tmux holds, in the
shape `tmux choose-tree` produces in its non-interactive form. Useful
for an LLM agent that needs to "see the whole topology" of the server
in one call without iterating `list_sessions` × `list_windows`. The
interactive picker is intentionally not reachable: this tool always
returns a structured snapshot.

Under the hood the tool runs `tmux list-windows -F ...` with the
appropriate `-a` / `-t` filter — `tmux choose-tree` itself is
interactive-only on tmux 3.4 (it opens a picker inside an attached
client), so the snapshot we expose to agents wraps `list-windows`
instead, against the same headless tmux servers the rest of the
surface targets.

### Input

| Field     | Type   | Required                          | Notes                                                                                          |
| --------- | ------ | --------------------------------- | ---------------------------------------------------------------------------------------------- |
| `scope`   | string | no (defaults to `"all"`)          | one of `"all"`, `"session"`, `"window"`. `"all"` walks every window on the server.             |
| `session` | string | when `scope` is `session`/`window`| len 1-64, regex `^[A-Za-z0-9_-]+$`.                                                            |
| `window`  | string | when `scope` is `window`          | window name (len 1-64, `^[A-Za-z0-9_-]+$`) or numeric index (`\d+`).                           |

The schema sets `additionalProperties: false`, so any field other than
`scope` / `session` / `window` is rejected with `-32602` before tmux
is consulted.

`scope="all"` does not accept `session` or `window`: passing them
alongside the default scope is rejected with `-32602` so a caller who
meant `scope="session"` but forgot to flip the field gets a fast,
pointed error rather than a silent server-wide listing.

### Output

JSON text block with a flat object keyed by `rows`:

```jsonc
{
  "rows": [
    {
      "session":      "demo",
      "window_index": 0,
      "window_name":  "shell",
      "pane_count":   1,
      "active":       true
    }
  ]
}
```

| Field          | Type    | Notes                                                                          |
| -------------- | ------- | ------------------------------------------------------------------------------ |
| `session`      | string  | tmux session name this row belongs to. Equal to the value used as a target.   |
| `window_index` | integer | Numeric window index within the session (0-based).                            |
| `window_name`  | string  | Human-readable window label tmux assigned.                                    |
| `pane_count`   | integer | Number of panes currently in the window.                                      |
| `active`       | boolean | `true` when this window is the currently focused one of its session.          |

A server with no windows visible (e.g. a session-scoped call right
after the last window of that session was killed) returns
`{"rows": []}` — a clean empty list rather than an error — so callers
can iterate the response without a separate "is this an error" branch.

### Errors

| Code     | Cause                                                                                  |
| -------- | -------------------------------------------------------------------------------------- |
| `-32602` | `scope` invalid; `session` / `window` malformed; required field missing for the chosen scope; or an unknown field on `arguments`. |
| `-32000` | `session` does not exist on this server (`errs.ErrSessionNotFound`).                   |
| `-32603` | tmux failed for an unexpected reason (server crashed, IO error).                       |

### Examples

```jsonc
// Default scope: snapshot every window on the server.
{}

// Scope to a single session.
{ "scope": "session", "session": "demo" }

// Drill into a single window.
{ "scope": "window", "session": "demo", "window": "build" }
```

Pair with `session_list` to find live sessions, then walk the tree:

```jsonc
{ "name": "session_list", "arguments": {} }
{ "name": "choose_tree",  "arguments": { "scope": "session", "session": "demo" } }
```

---

## `set_buffer`

Write `data` into a tmux paste buffer via `tmux set-buffer`. Buffers
live on the tmux server (not on a session), so a caller can later read
the value back from any session — useful for stashing large
clipboard-style snippets between tool calls without serialising them
through repeated `send_keys` frames. Pass an optional `name` to pin a
stable buffer name (`-b NAME`); omit it to let tmux auto-assign
`bufferN`. Set `append=true` to concatenate onto an existing buffer
(`-a`); when the named buffer does not exist tmux silently creates it,
matching the underlying CLI semantics.

### Input

| Field    | Type    | Required | Notes                                                                                                                                |
| -------- | ------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| `data`   | string  | yes      | buffer payload, stored verbatim. Empty string is allowed but tmux 3.4 silently drops empty buffers. Capped at 1 MiB (1 048 576 bytes). |
| `name`   | string  | no       | optional buffer name to pin; len 1-128, regex `^[A-Za-z0-9_-]+$`. When omitted, tmux assigns the next `bufferN`.                     |
| `append` | boolean | no       | when true, concatenate onto an existing buffer (`-a`) instead of replacing it. Defaults to false.                                     |

### Output

JSON text block:

```jsonc
{
  "set":  true,
  "name": "buffer3"   // resolved buffer name; equal to the input `name` when one was pinned, otherwise the auto-assigned bufferN.
}
```

The resolved `name` is recovered by running
`tmux list-buffers -F '#{buffer_created} #{buffer_name}'` after the
set and picking the most-recently-created entry. When the caller
pinned a `name`, that lookup is skipped — tmux honours `-b NAME`
verbatim and the resolved name is exactly what was passed.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | `data` missing or > 1 MiB; `name` outside the regex/length policy; or an unknown field on `arguments`. |
| `-32603` | tmux's set-buffer or follow-up list-buffers failed (rare; typically a fork/exec error). |

### Examples

```jsonc
// Stash a snippet under tmux's auto-naming. The response carries
// the buffer name a follow-up tool can target.
{ "data": "hello world" }

// Pin a stable name so a follow-up show_buffer doesn't have to
// round-trip through list_buffers.
{ "data": "the quick brown fox", "name": "snippet" }

// Append to an existing buffer (or create it under that name).
{ "data": "...continued", "name": "snippet", "append": true }
```

Pair with `show_buffer` to round-trip a snippet:

```jsonc
{ "name": "set_buffer",  "arguments": { "data": "the value", "name": "shared" } }
{ "name": "show_buffer", "arguments": { "name": "shared" } }
```

---

## `list_buffers`

Enumerate the paste buffers tmux is currently holding on this server.
Useful when an agent has stashed multiple snippets via `set-buffer`
and needs to discover the assigned names before fetching contents
with `show_buffer`. Buffers live on the tmux server (not on a
session), so a bare list call returns every buffer regardless of
which session originally created it.

### Input

No fields. Pass `{}`. The schema sets `additionalProperties: false`,
so any unknown field is rejected with `-32602` before tmux is
consulted.

### Output

JSON text block:

```jsonc
{
  "buffers": [
    { "name": "buffer0", "size": 11,  "created_at": "2026-05-24T22:51:31Z" },
    { "name": "pinned",  "size": 7,   "created_at": "2026-05-24T22:51:32Z" }
  ]
}
```

| Field        | Type    | Notes                                                                |
| ------------ | ------- | -------------------------------------------------------------------- |
| `name`       | string  | tmux buffer name; either auto-assigned (`bufferN`) or pinned via `set-buffer -b NAME`. |
| `size`       | integer | byte length of the buffer's contents.                                |
| `created_at` | string  | RFC3339 (UTC) timestamp tmux first stored the buffer.                |

Returns `{"buffers": []}` (not an error) when no buffers exist —
including against a freshly-spawned controller whose tmux server has
not even started yet.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Unknown field on `arguments`.                                      |
| `-32603` | tmux returned an unparseable list-buffers response.                |

### Example

```jsonc
{}
```

Pair with `show_buffer` to fetch a specific buffer's contents:

```jsonc
{ "name": "list_buffers", "arguments": {} }
{ "name": "show_buffer",  "arguments": { "name": "pinned" } }
```

---

## `show_buffer`

Return the raw text content of a tmux paste buffer. Omit `name` (or
pass an empty string) to dump the most-recently-added buffer,
matching the tmux CLI default — the common case after a fresh
`set-buffer`. When `name` is supplied, `tmux show-buffer -b <name>`
runs and the value round-trips verbatim.

### Input

| Field  | Type   | Required | Notes                                                                                            |
| ------ | ------ | -------- | ------------------------------------------------------------------------------------------------ |
| `name` | string | no       | optional buffer name; len 1-128, regex `^[A-Za-z0-9_-]+$`. Empty / omitted → most-recent buffer. |

### Output

JSON text block:

```jsonc
{
  "name": "pinned",         // echoed back when the caller pinned a name; empty otherwise.
  "data": "the quick brown fox"
}
```

`data` is the buffer body verbatim — tmux does not append a trailing
newline, so an agent that expects one should add it explicitly.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | `name` outside the regex/length policy, or an unknown field on `arguments`. |
| `-32000` | The named buffer does not exist on this server (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the show for an unexpected reason.                    |

### Example

```jsonc
{ "name": "pinned" }
```

A typical chain looks like: stash a snippet, list to discover the
assigned name, then read it back.

```jsonc
{ "name": "list_buffers", "arguments": {} }
{ "name": "show_buffer",  "arguments": { "name": "buffer0" } }
```

