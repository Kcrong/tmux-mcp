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
| `-32005` | `errs.ErrPaneActive`           | `respawn_pane` / `respawn_window` targeted a pane or window whose original command is still running and `kill` was not set; retry with `kill=true`. |
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

## `find_window`

Search for windows whose name, pane title, or visible pane content
matches a query. Functionally `tmux find-window` for a headless server
— tmux's own `find-window` requires a client attached, so the boundary
implements the same matching semantics on top of `tmux list-windows -F
… -f <filter>` and returns the matching rows directly. Useful when an
agent is juggling many sessions and wants to locate "the build window"
or "the pane currently showing an error" without enumerating every
session/window pair client-side.

By default the search runs across all three scopes (the same `-CNT`
default tmux uses); set any of the `*_only` flags to restrict, or
combine them to compose a union (matches in any selected scope are
returned). `regex` flips matching from fnmatch-style globbing to a
regular expression (`-r`). `target`, when supplied, scopes the search
to a single tmux session (`-t`); otherwise every window on the server
is considered (`-a`).

### Input

| Field          | Type    | Required | Default | Notes                                                                         |
| -------------- | ------- | -------- | ------- | ----------------------------------------------------------------------------- |
| `match`        | string  | yes      | —       | non-empty pattern. fnmatch substring by default; regex when `regex=true`.     |
| `regex`        | boolean | no       | `false` | treat `match` as a regular expression (`-r`).                                 |
| `name_only`    | boolean | no       | `false` | restrict to the window name (`-N`). Combine with the other `*_only` flags.   |
| `title_only`   | boolean | no       | `false` | restrict to the window's pane title (`-T`).                                  |
| `content_only` | boolean | no       | `false` | restrict to visible pane content (`-C`).                                     |
| `target`       | string  | no       | —       | session id; len 1-64, regex `^[A-Za-z0-9_-]+$`. Omit to search every session. |

The schema sets `additionalProperties: false`, so any field other than
the documented ones is rejected with `-32602` before tmux is consulted.

### Output

JSON text block with a flat object keyed by `matches`:

```jsonc
{
  "matches": [
    { "session": "demo",  "window_index": 1, "window_name": "build" },
    { "session": "build", "window_index": 0, "window_name": "build" }
  ]
}
```

| Field          | Type    | Notes                                                                              |
| -------------- | ------- | ---------------------------------------------------------------------------------- |
| `session`      | string  | Session the matching window lives in. Combine with `window_index` to form a `session:index` target. |
| `window_index` | integer | Window index inside its session (0-based).                                         |
| `window_name`  | string  | Whatever tmux assigned (caller-supplied `-n`, or the auto label).                  |

A query that matches zero windows returns `{"matches": []}` (an empty
array, not `null`) so callers branching on `matches.length === 0` do
not have to also handle the null shape.

### Errors

| Code     | Cause                                                                |
| -------- | -------------------------------------------------------------------- |
| `-32602` | `match` missing/empty, `target` malformed, or an unknown field sent. |
| `-32000` | `target` does not exist on this server (`errs.ErrSessionNotFound`).  |
| `-32603` | tmux failed for an unexpected reason (server crashed, IO error).     |

### Examples

```jsonc
// substring across name/title/content (default scope, every session)
{ "match": "error" }

// regex anchored to the window name, scoped to one session
{ "match": "^build_", "regex": true, "name_only": true, "target": "demo" }

// content-only search for a phrase visible on a pane
{ "match": "panic:", "content_only": true }
```

A typical chain looks like: locate the matching window, focus it, then
drive it.

```jsonc
{ "name": "find_window",   "arguments": { "match": "build", "name_only": true, "target": "demo" } }
{ "name": "window_select", "arguments": { "session": "demo", "target": "1" } }
{ "name": "capture",       "arguments": { "session": "demo" } }
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

## `send_prefix`

Deliver tmux's configured prefix key (default `C-b`, or `C-a` /
whatever the running server has bound) to a target pane via
`tmux send-prefix [-2] -t <target>`. Useful when an inner TUI (vim,
htop, weechat, …) running inside the pane has captured the prefix
chord for its own purposes and an agent needs to forward the literal
prefix keystroke through to that inner program. Set `secondary: true`
to deliver the secondary prefix (`-2`, configured via `prefix2`).

### Input

| Field       | Type    | Required | Default | Notes                                                                       |
| ----------- | ------- | -------- | ------- | --------------------------------------------------------------------------- |
| `target`    | string  | yes      | —       | pane target: `"session"`, `"session:window"`, or `"session:window.pane"` |
| `secondary` | boolean | no       | `false` | when `true`, send the secondary prefix (`-2`) instead of the primary one    |

### Output

Status text block: `ok`.

### Errors

- `-32602` — missing/malformed `target`.
- `-32000` — session or pane does not exist.
- `-32603` — tmux send-prefix failed.

### Example

```jsonc
{ "target": "demo:0.1", "secondary": false }
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

## `choose_client`

Open an interactive client-chooser via
`tmux choose-client [-N] [-Z] [-r] [-t TARGET-PANE] [-F FORMAT]
[-f FILTER] [-K KEY-FORMAT] [-O SORT-ORDER] [TEMPLATE]`. tmux draws the
chooser inside `target` (or the active pane of the active client when
omitted) and lets the attached client pick which connected tmux client
to act on. The optional flags map one-for-one onto tmux's: `no_preview`
suppresses the preview pane (`-N`), `zoom` zooms the chooser (`-Z`),
`reverse` reverses the sort order (`-r`). `format` / `filter` /
`key_format` / `sort_order` / `template` are forwarded verbatim when
non-empty so callers can re-skin the menu without rebuilding tmux.

Useful when an agent is co-driving a session with a human and wants to
hand control over to a specific connected terminal — pair with
`list_clients` to enumerate the candidates first, then drop the
chooser inside whichever pane the human can see.

### Input

| Field        | Type    | Required | Notes                                                                            |
| ------------ | ------- | -------- | -------------------------------------------------------------------------------- |
| `target`     | string  | no       | Pane target (`session`, `session:window`, `session:window.pane`, or `%N`).       |
| `format`     | string  | no       | tmux format string for each menu line (`-F`); max 4096 bytes, no newlines.       |
| `filter`     | string  | no       | tmux conditional that hides clients evaluating to false (`-f`); same bounds.     |
| `key_format` | string  | no       | tmux format for the per-row hotkey label (`-K`); same bounds.                    |
| `sort_order` | string  | no       | Column to sort the menu by (`-O`), e.g. `name`, `size`, `creation`; same bounds. |
| `template`   | string  | no       | tmux command template run against the chosen client (`TEMPLATE`); same bounds.   |
| `no_preview` | boolean | no       | When true, suppress the preview pane (`-N`). Default `false`.                    |
| `zoom`       | boolean | no       | When true, zoom the chooser pane (`-Z`). Default `false`.                        |
| `reverse`    | boolean | no       | When true, reverse the sort order (`-r`). Default `false`.                       |

The schema sets `additionalProperties: false`, so any field other than
the ones above is rejected with `-32602` (invalid params) before tmux
is consulted — a typo like `"key-format"` (dash) fails fast instead of
silently behaving like the default-flag variant.

### Output

JSON text block with a flat ack:

```jsonc
{ "opened": true }
```

The boundary deliberately does not echo the flag values back —
`choose-client` is a fire-and-forget UX trigger, and a follow-up call
(e.g. `list_clients` to confirm the chooser fired) is one tool away
when the agent wants confirmation.

### Errors

| Code     | Cause                                                                           |
| -------- | ------------------------------------------------------------------------------- |
| `-32602` | malformed `target` or a free-form format that exceeds 4096 bytes / has newlines. |
| `-32000` | `target` does not resolve to a live pane, or the server has no clients attached. |
| `-32603` | tmux failed for an unexpected reason (server crashed, IO error).                 |

The `-32000` headless case is the load-bearing failure: the chooser is
a UX affordance and cannot do anything useful without a client to draw
the menu in, so the controller refuses the call up front rather than
silently queueing a chooser nobody can see.

### Example

```jsonc
// minimal — all defaults
{}

// open a chooser inside a specific pane, with a custom row format
{
  "target": "demo:0.0",
  "format": "#{client_tty} (#{client_session})",
  "no_preview": true
}

// re-skin the chooser and run a tmux command on the picked client
{
  "sort_order": "name",
  "reverse":    true,
  "template":   "display-message -c '%%' #{client_tty}"
}
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

## `display_panes`

Briefly draw each pane's identifier overlay on a tmux client via
`tmux display-panes [-b] [-d duration] [-N] [-t CLIENT] [template]`.
This is the visual primitive that lets a human pick a pane by index;
agents use it to surface the picker on an attached terminal so a user
can choose, or to fire a templated tmux command keyed off the
selection. This is a **mutating** tool — it draws onto a live client
and can run a templated command on selection — so a `-read-only`
deployment rejects it with `-32005` (`errs.CodeReadOnly`) before the
handler runs.

### Input

| Field         | Type    | Required | Notes                                                                                                                                                                                       |
| ------------- | ------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `block`       | boolean | no       | When `true`, pass `-b` so tmux waits until the user has finished selecting before returning. Defaults to `false` (return as soon as the picker is drawn).                                   |
| `duration_ms` | integer | no       | How long to paint the overlay, in milliseconds; maps to `-d`. Range `0..600000` (10 minutes). `0` falls back to tmux's `display-panes-time` (typically 1000 ms).                            |
| `no_prefix`   | boolean | no       | When `true`, pass `-N` so the prefix key is not reserved during the picker. Defaults to `false`.                                                                                            |
| `target`      | string  | no       | tmux client name (TTY path like `/dev/pts/0`); regex `^/[A-Za-z0-9_./:-]+$`, len 1-256. Maps to `-t CLIENT`. Omit to draw on the caller's current client.                                   |
| `template`    | string  | no       | tmux command template run against the selection (e.g. `select-pane -t %%`); forwarded verbatim, len capped at 4096. Omit to leave tmux's default behaviour.                                 |

The schema sets `additionalProperties: false`, so any field other
than the five above is rejected with `-32602` before tmux is
consulted — a typo like `"duration"` (instead of `"duration_ms"`) or
`"client"` (instead of `"target"`) fails fast instead of being
silently ignored.

### Output

JSON text block with a flat object keyed by `displayed`:

```jsonc
{ "displayed": true }
```

| Field       | Type    | Notes                                                                                                                                                                                |
| ----------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `displayed` | boolean | Always `true` on success. The shape leaves room for future extensions (e.g. a count of panes drawn) without breaking callers that read only the boolean.                            |

A headless server with nothing attached returns
`{"displayed": true}` — a clean success rather than an error — so
callers can fire-and-forget without first running `list_clients` to
know whether there is a client to draw on. The boundary swallows
tmux's `no current client` stderr in this case.

### Errors

| Code     | Cause                                                                                                                                       |
| -------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `-32602` | Malformed args (bad regex on `target`, oversized `template`, `duration_ms` outside `[0..600000]`, or an unknown field on the schema).        |
| `-32000` | `target` named a client that is not currently attached (`errs.ErrSessionNotFound`).                                                          |
| `-32005` | Server is running in `-read-only` mode (this tool draws onto live clients).                                                                  |
| `-32603` | tmux failed for an unexpected reason (server crashed, IO error, malformed `template` rejected at substitution).                              |

### Examples

```jsonc
// fire-and-forget on the caller's current client; tmux's own
// display-panes-time decides how long the overlay paints
{}

// draw the picker for 2.5s on a specific terminal
{ "target": "/dev/pts/0", "duration_ms": 2500 }

// block until the user picks; useful for a user-driven flow
{ "block": true }

// run a templated select-pane against the selection — tmux
// substitutes %% with the picked pane id at execution time
{ "target": "/dev/pts/0", "template": "select-pane -t %%" }

// free the prefix key during the picker so the user can drop
// straight into a binding
{ "no_prefix": true, "target": "/dev/pts/0" }
```

Pair with `list_clients` to discover the live roster before deciding
which terminal to draw on:

```jsonc
{ "name": "list_clients",  "arguments": {} }
{ "name": "display_panes", "arguments": { "target": "/dev/pts/0" } }
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

To install a new binding (rather than just read the existing map),
pair `list_keys` with [`bind_key`](#bind_key) below — the two tools
read and write the same keymap and share the same `key_table` regex
so a name that round-trips through one will round-trip through the
other.

---

## `bind_key`

Register a tmux key binding via `tmux bind-key [-T TABLE] [-r] KEY COMMAND`.
The write counterpart of [`list_keys`](#list_keys), which reads the
same keymap back. Useful when an agent (or an init script the agent
drives) wants to install a custom chord — `bind_key` answers "make
F12 fire `display-message hello`" without dropping out to the shell.

### Input

| Field        | Type    | Required | Notes                                                                                                                                                                                        |
| ------------ | ------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `key`        | string  | yes      | Keysym string tmux's parser knows (e.g. `"C-Space"`, `"M-x"`, `"Up"`). len 1-256, no NUL or other ASCII control bytes (DEL allowed because tmux uses literal keysym strings).                |
| `command`    | string  | yes      | Entire tmux command line to bind to the chord; passed verbatim as a single argv element (do NOT split on whitespace — tmux parses it server-side). len 1-4096, no NUL or non-tab control bytes. |
| `key_table`  | string  | no       | Keymap name (e.g. `"prefix"`, `"root"`, `"copy-mode"`, `"copy-mode-vi"`); maps to `-T TABLE`. Same shape as `list_keys.key_table` — len 1-64, regex `^[A-Za-z0-9_-]+$`. Omit to land in tmux's default table (`"prefix"` on tmux 3.4). |
| `repeatable` | boolean | no       | When `true`, add `-r` so the binding can repeat while the prefix table stays armed (used by tmux's built-in resize / select-pane chords). Default `false`.                                  |

The schema sets `additionalProperties: false`, so any field other than
the four above is rejected with `-32602` (invalid params) before tmux
is consulted — a typo like `"table"` (instead of `"key_table"`) fails
fast instead of silently landing in the default table.

### Output

JSON text block: `{"bound": true, "key": "<key>", "table": "<key_table>"}`.
The `key` and `table` fields echo the caller's inputs verbatim so an
agent can confirm what tmux now thinks (an empty `table` means the
binding landed in the default table). On every code path that reaches
this envelope, `bound` is `true` — failures surface via the JSON-RPC
error object instead.

### Errors

| Code     | Cause                                                                                                                                            |
| -------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `-32602` | `key` or `command` missing/empty/oversized, contains a control byte, or `key_table` is malformed; or an unknown field was sent.                  |
| `-32603` | tmux refused the bind (e.g. `command` references an unknown verb, the command string has a syntax error, the named `key_table` does not exist). |

`bind-key` has no equivalent of "session not found" — it does not
look up live state, so failures are purely syntactic and surface
through the generic internal-error code rather than a typed sentinel.

### Examples

```jsonc
// Install F12 → display a message; lands in the default (prefix) table.
{ "key": "F12", "command": "display-message hello" }

// Install in copy-mode so it only fires while copy-mode is active.
{ "key": "F11", "command": "send-keys -X cancel", "key_table": "copy-mode" }

// Repeatable resize binding: hold the prefix and tap C-Right repeatedly.
{ "key": "C-Right", "command": "resize-pane -R 5", "repeatable": true }
```

Pair with [`list_keys`](#list_keys) to confirm the binding landed and
then drive it with `send_keys`:

```jsonc
{ "name": "bind_key",  "arguments": { "key": "F12", "command": "display-message hello" } }
{ "name": "list_keys", "arguments": { "key_table": "prefix" } }
{ "name": "send_keys", "arguments": { "session": "demo", "keys": ["F12"] } }
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

## `refresh_client`

Force a tmux client redraw via `tmux refresh-client [-S] [-t <client>]`.
Useful when an agent has rewritten an option that affects what the
client renders (for example `status-format`, `status-style`,
`window-status-format`) and wants the change to take effect
immediately rather than on the next tmux render tick. This is a
**mutating** tool — it changes what the client's terminal displays —
so a `-read-only` deployment rejects it with `-32005`
(`errs.CodeReadOnly`) before the handler runs.

### Input

| Field         | Type    | Required | Notes                                                                                                                              |
| ------------- | ------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| `client`      | string  | no       | tmux client name (the path-like key shown in `list_clients`, e.g. `/dev/pts/0`); regex `^/[A-Za-z0-9_./:-]+$`, len 1-256. Omit to refresh every attached client. |
| `status_only` | boolean | no       | When `true`, pass `-S` to redraw only the status line (cheaper than a full screen redraw). Defaults to `false`.                    |

The schema sets `additionalProperties: false`, so any field other than
`client` / `status_only` is rejected with `-32602` (invalid params)
before tmux is consulted — a typo like `"clinet"` fails fast instead
of silently behaving like the unscoped variant.

### Output

JSON text block with a flat object keyed by `refreshed`:

```jsonc
{ "refreshed": true }
```

| Field       | Type    | Notes                                                                          |
| ----------- | ------- | ------------------------------------------------------------------------------ |
| `refreshed` | boolean | Always `true` on success. The shape leaves room for future extensions without breaking callers that read only the boolean. |

A headless server with nothing attached returns `{"refreshed": true}`
— a clean success rather than an error — so callers can fire-and-
forget a refresh without first running `list_clients` to know whether
there is anything to refresh. The boundary swallows tmux's
`no current client` stderr in this case; any other tmux failure
surfaces as `-32603`.

### Errors

| Code     | Cause                                                                            |
| -------- | -------------------------------------------------------------------------------- |
| `-32602` | Malformed args (bad regex / over the length cap on `client`) or an unknown field on the schema. |
| `-32000` | `client` named a terminal that is not currently attached (`errs.ErrSessionNotFound`).            |
| `-32005` | Server is running in `-read-only` mode (this tool mutates client state).                         |
| `-32603` | tmux failed for an unexpected reason (server crashed, IO error).                                 |

### Examples

```jsonc
// refresh every attached client (full redraw)
{}

// status-line refresh only — fastest path after a status-format change
{ "status_only": true }

// scope to a single client (e.g. after `list_clients` returned its TTY)
{ "client": "/dev/pts/0" }

// status-line refresh, scoped to one terminal
{ "client": "/dev/pts/0", "status_only": true }
```

Pair with `set-option`-style introspection to redraw immediately after
rewriting a status variable:

```jsonc
{ "name": "display_message",  "arguments": { "format": "#{status-format[0]}" } }
// agent flips the option via shell / a future set_options tool
{ "name": "refresh_client",   "arguments": { "status_only": true } }
## `lock_session`

Lock every client attached to a session via
`tmux lock-session -t SESSION`. tmux runs the configured `lock-command`
(default `lock -np`) on each attached terminal, so the human user has
to authenticate before resuming work. Running processes inside the
session are left untouched and the session itself stays valid for
follow-up tools — only the attached clients see the lock screen.

Useful when an agent is handing a long-running session back to a human
and wants the screen secured. Headless servers (the common case for
tmux-mcp) have nothing to lock; tmux still exits 0 because the loop
over attached clients is empty, so the call is safe to make from
automation that does not know whether anyone is currently attached.

This tool mutates state (it writes the lock screen to every attached
client) and is therefore **not** in the `-read-only` allowlist — a
read-only agent that calls it sees `-32011` (`errs.ErrReadOnly`)
before any tmux command runs.

### Input

| Field     | Type   | Required | Notes                                                  |
| --------- | ------ | -------- | ------------------------------------------------------ |
| `session` | string | yes      | existing session id; len 1-64, regex `^[A-Za-z0-9_-]+$` |

The schema sets `additionalProperties: false`, so any field other than
`session` is rejected with `-32602` before tmux is consulted — a typo
like `"sesion"` fails fast.

### Output

JSON text block: `{"locked": true}`. The boundary deliberately does
not echo which clients were locked — `tmux lock-session` reports
nothing of the sort itself, and a follow-up `list_clients` is one
call away if the agent wants to inspect the affected terminals.
## `list_commands`

Enumerate every command this tmux build advertises via
`tmux list-commands`. Useful for an agent that needs to introspect
the tmux command surface before sending one through the rest of the
boundary, or to confirm a command exists on the deployed tmux
release. Each entry carries `{ name, alias, args }` — `alias` is the
short form (e.g. `lsk` for `list-keys`) and is empty when the command
has no alias; `args` is the verbatim flag/argument signature tmux
printed (empty for no-arg commands like `kill-server`).

### Input

| Field     | Type   | Required | Notes                                                                              |
| --------- | ------ | -------- | ---------------------------------------------------------------------------------- |
| `command` | string | no       | optional command name (e.g. `"list-keys"`, `"send-keys"`); maps to the trailing positional `tmux list-commands NAME`. len 1-64, regex `^[A-Za-z][A-Za-z0-9-]*$`. Omit to list every command. |

The schema sets `additionalProperties: false`, so any field other than
`command` is rejected with `-32602` (invalid params) before tmux is
consulted — a typo like `"name"` (instead of `"command"`) fails fast
instead of silently behaving like the unscoped variant.

### Output

JSON text block with a flat object keyed by `commands`:

```jsonc
{
  "commands": [
    { "name": "list-keys",    "alias": "lsk", "args": "[-1aN] [-P prefix-string] [-T key-table] [key]" },
    { "name": "kill-server",  "alias": "",    "args": "" }
  ]
}
```

| Field   | Type   | Notes                                                                                                |
| ------- | ------ | ---------------------------------------------------------------------------------------------------- |
| `name`  | string | Canonical command name (the verb you'd pass to `tmux <name>`).                                       |
| `alias` | string | tmux's short form for the command (e.g. `lsk` for `list-keys`); empty when the command has no alias. |
| `args`  | string | The verbatim flag/argument signature tmux printed; empty for no-arg commands like `kill-server`.     |

A filter that does not match a known command returns
`{"commands": []}` — a clean empty list rather than an error — so
callers can iterate the response without a separate "is this an
error" branch. (tmux 3.0–3.3 exits 1 on an unknown filter; 3.4+ exits
0 with empty stdout. The boundary collapses both into the same empty
list.)

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Missing/invalid `session`, or an unknown field was sent.           |
| `-32000` | `session` does not exist on this server (`errs.ErrSessionNotFound`). |
| `-32011` | Server started with `-read-only`; lock_session mutates state and is rejected (`errs.ErrReadOnly`). |
| `-32603` | tmux refused the lock for an unexpected reason.                    |

### Example

```jsonc
{ "session": "demo" }
```

Pair with `list_clients` to confirm exactly which terminals were
affected when handing the session back to a human:

```jsonc
{ "name": "list_clients",  "arguments": { "session": "demo" } }
{ "name": "lock_session",  "arguments": { "session": "demo" } }
| `-32602` | `command` outside the regex/length policy, or an unknown field was sent. |
| `-32603` | tmux failed for an unexpected reason (server crashed, IO error).   |

### Examples

```jsonc
// Every command tmux exposes.
{}

// Just the signature for one command.
{ "command": "list-keys" }
```

Pair with `display_message` once you've discovered a command of
interest — `list_commands` answers "does this tmux build support
verb X?" so the agent doesn't have to guess against an older release:

```jsonc
{ "name": "list_commands",   "arguments": { "command": "display-message" } }
{ "name": "display_message", "arguments": { "format": "#{session_name}", "session": "demo" } }
```

---

## `pane_select`

Make `target` the active pane of its window. Subsequent `send_keys` /
`capture` calls that name the surrounding session will then act on the
newly selected pane. Useful for multi-pane TUIs (vim+terminal split,
zellij-style layouts) where the agent needs to flip focus between
panes between commands.

For mark/unmark, last-active jumps, directional walks, input toggling,
or zoom, reach for [`select_pane`](#select_pane) instead — it accepts
the same `target` plus the full optional flag set `tmux select-pane`
understands.

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

## `select_pane`

The capable sibling of [`pane_select`](#pane_select): wraps
`tmux select-pane -t TARGET` with the full optional flag set so an
agent can mark / unmark the pane, jump back to the last-active pane,
walk one step toward a directional neighbour, toggle pane input, or
zoom the window — atomic on tmux's side. Use this when the bare
"make this pane active" semantics of `pane_select` aren't enough.

The flag pairs `mark` / `unmark` and `enable_input` / `disable_input`
are mutually exclusive; requesting both members of a pair returns
`-32602` before any tmux command runs (tmux's silent last-flag-wins
behaviour is rarely what the caller intended).

### Input

| Field            | Type    | Required | Notes                                                                              |
| ---------------- | ------- | -------- | ---------------------------------------------------------------------------------- |
| `target`         | string  | yes      | tmux pane target (`session`, `session:window`, `session:window.pane`, or `%N`)     |
| `mark`           | boolean | no       | when `true`, mark the pane (`-m`) so swap-pane / join-pane can pick it up         |
| `unmark`         | boolean | no       | when `true`, clear the marked-pane state (`-M`)                                    |
| `last`           | boolean | no       | when `true`, jump to the last-active pane (`-l`) of the target's window            |
| `direction`      | string  | no       | walk one step toward the named neighbour: `"up"` (-U), `"down"` (-D), `"left"` (-L), `"right"` (-R) |
| `enable_input`   | boolean | no       | when `true`, enable input on the pane (`-e`)                                       |
| `disable_input`  | boolean | no       | when `true`, disable input on the pane (`-d`)                                      |
| `zoom`           | boolean | no       | when `true`, also zoom the window on the target pane (`-Z`)                        |

### Output

Status text block: `ok`.

### Errors

| Code     | Cause                                                                                                                                    |
| -------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `-32602` | Missing/empty `target`, malformed pane target, unknown `direction`, or both members of `mark`/`unmark` (or `enable_input`/`disable_input`) set. |
| `-32000` | tmux could not resolve the target (`errs.ErrSessionNotFound`).                                                                           |
| `-32603` | tmux select-pane failed for any other reason.                                                                                            |

### Example

Walk one pane to the right, marking it as the implicit swap-source for
a subsequent `pane_swap`:

```jsonc
{ "target": "demo:0.0", "direction": "right", "mark": true }
```

Disable input on a side-car log pane while leaving focus on it:

```jsonc
{ "target": "demo:0.1", "disable_input": true }
```

Toggle zoom on the active pane of a window:

```jsonc
{ "target": "demo:0", "zoom": true }
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

## `clock_mode`

Put a pane into tmux's built-in clock-mode screensaver via
`tmux clock-mode [-t <target>]`. The targeted pane (or the current
pane when `target` is omitted) renders a large digital clock until the
next key arrives — the running process keeps running underneath, only
the visual takeover is added on top. Useful for "parking" a pane
visually (demo recording, status board, idle indicator) without typing
keys into the running program.

### Input

| Field    | Type   | Required | Notes                                                                                              |
| -------- | ------ | -------- | -------------------------------------------------------------------------------------------------- |
| `target` | string | no       | Optional pane target (`session`, `session:window`, `session:window.pane`, or a `%N` pane id). Omit to target the server's current pane. Length capped at 256. |

`target` (when present) must match `^[A-Za-z0-9_-]+(:[0-9]+(\.[0-9]+)?)?$`
or `^%[0-9]+$` — the same conservative shape every other pane-target
tool accepts.

### Output

JSON block: `{"clock_mode": true}`. The boundary deliberately does not
echo the resolved target — tmux clock-mode is fire-and-forget, and a
follow-up `display_message` with `format="#{pane_mode}"` is one call
away if the agent wants to confirm the pane actually entered
clock-mode.

### Errors

| Code     | Cause                                                                                              |
| -------- | -------------------------------------------------------------------------------------------------- |
| `-32602` | Malformed `target` (regex/length policy) or unknown property in arguments (`additionalProperties: false`). |
| `-32000` | `target` does not resolve on this server, or the controller is headless (no tmux server running). Both surface `errs.ErrSessionNotFound`. |
| `-32603` | tmux refused the call for any other reason (e.g. internal tmux failure).                           |

### Example

```jsonc
{ "target": "demo:0.1" }
```

Park a pane on the clock and confirm with `display_message`:

```jsonc
{ "name": "clock_mode",      "arguments": { "target": "demo" } }
{ "name": "display_message", "arguments": { "session": "demo", "format": "#{pane_mode}" } }
// → { "value": "clock-mode" }
```

---

## `pipe_pane`

Pipe a pane's output through a shell command via
`tmux pipe-pane [-IO] -t <target> [shell-command]`. The canonical way to
log pane output to a file:
`{"target": "demo:0", "shell_command": "cat > /tmp/demo.log"}` flushes
every byte tmux writes to the pty into `/tmp/demo.log` until a
follow-up call clears the pipe. Calling `pipe_pane` again with
`shell_command` empty/omitted sends a bare `pipe-pane` to tmux, which
tears down any existing pipe on that pane (the documented "stop
logging" form).

> **CAUTION** — `pipe_pane` spawns a shell pipeline on the tmux server.
> tmux runs `shell_command` via `/bin/sh -c`; **the command itself is
> not sandboxed by this server**, so an agent that can call this tool
> can run arbitrary shell on the server. Operators must trust the agents
> reaching for `pipe_pane` — gate the surface away from untrusted
> clients with the `-allowlist` flag (see [`docs/flags.md`](flags.md))
> or remove the tool from the registry entirely.
>
> Mutating in spirit (it spawns a shell pipeline), so `pipe_pane` is
> **not** allowed under `-read-only`.

### Input

| Field           | Type    | Required | Default | Notes                                                                                       |
| --------------- | ------- | -------- | ------- | ------------------------------------------------------------------------------------------- |
| `target`        | string  | yes      | —       | Pane target (`session`, `session:window`, or `session:window.pane`).                         |
| `shell_command` | string  | no       | `""`    | Shell pipeline tmux runs via `/bin/sh -c`. Empty/omitted **stops** an existing pipe.        |
| `output_only`   | boolean | no       | `false` | When true, adds `-O`: only output written by tmux is piped, not input typed at the pane.    |
| `also_input`    | boolean | no       | `false` | When true, adds `-I`: also pipe input. Combine with `output_only` to mirror both directions.|

`shell_command` is bounded at 4096 bytes; NUL bytes and other ASCII
control characters (newline, ESC, DEL, …) are rejected up front. Tab
(0x09) is allowed for spacing.

### Output

JSON block: `{"piped": true}`. The boundary deliberately does not echo
the resolved argv because tmux gives no useful confirmation back — a
follow-up read of the operator's log file is the natural way to
confirm the pipe is flowing.

### Errors

| Code     | Cause                                                                                               |
| -------- | --------------------------------------------------------------------------------------------------- |
| `-32602` | Missing/empty `target`, malformed `target` (regex/length policy), or `shell_command` violating the length / control-char policy. |
| `-32000` | `target` does not resolve on this server (`errs.ErrSessionNotFound`).                               |
| `-32603` | tmux refused the pipe for any other reason (e.g. internal tmux failure).                            |

### Example

Start logging the visible pane to a file:

```jsonc
{ "target": "demo:0", "shell_command": "cat > /tmp/demo.log" }
```

Stop the pipe (no `shell_command`):

```jsonc
{ "target": "demo:0" }
```

Mirror both directions through `tee`:

```jsonc
{ "target": "demo:0", "shell_command": "tee /tmp/demo.log",
  "output_only": true, "also_input": true }
## `confirm_before`

Stage a tmux command behind an interactive y/n prompt via
`tmux confirm-before [-p prompt] [-t target-client] command`. tmux pops
a confirmation prompt up in the matching client and only runs `command`
if the user accepts — the controller surfaces this as a single
fire-and-forget call so an agent can stage destructive ops without
making the tmux UI silently auto-execute.

The boundary is deliberately NOT idempotent on a headless server: with
no client attached there is no terminal to display the prompt on, so
the call surfaces `-32000` (`CodeSessionNotFound`) rather than a silent
success. Returning a successful no-op there would let an agent believe
a destructive command was queued behind a confirmation when in fact
nobody ever saw the prompt — exactly the auto-execute behaviour the
tool exists to prevent.

### Input

| Field     | Type   | Required | Default | Notes                                                                                          |
| --------- | ------ | -------- | ------- | ---------------------------------------------------------------------------------------------- |
| `command` | string | yes      | —       | tmux command to run if the user accepts; len 1-4096                                            |
| `prompt`  | string | no       | tmux's `Confirm 'CMD'? (y/n)` | y/n prompt text; len 0-128                                            |
| `target`  | string | no       | caller's current client | tmux client target (typically a TTY path like `/dev/pts/0`); regex `^[A-Za-z0-9/_.\-]+$`, len 0-128 |

### Output

JSON block: `{"ack": true, "prompt": "<text>"}`. The `prompt` field
echoes whatever the caller passed (empty when omitted) so an agent can
log exactly which confirmation it queued.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Missing/empty `command`, oversized field, or malformed `target` (regex/length policy). |
| `-32000` | No client attached (headless server) or named `target` does not resolve (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the call for any other reason. |

### Example

```jsonc
{ "command": "kill-session -t demo", "prompt": "really kill demo?" }
```

Stage a destructive kill behind an explicit confirmation, leaving the
final say with the human at the terminal. With a specific
`target` the prompt is routed at one attached client rather than
"whoever tmux thinks is current":

```jsonc
{ "command": "kill-session -t demo", "target": "/dev/pts/0" }
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

## `last_pane`

Switch the active pane of a window back to whichever pane was
previously active via `tmux last-pane`. Useful for an LLM agent that
just split a pane, drove the new one, and wants to flip back to the
original without having to track the pane id explicitly. Pairs with
`pane_split` (which surfaces the new pane's id) and `list_panes`
(which exposes the current `active` flag) for layout-aware retargeting.

### Input

| Field           | Type    | Required | Notes                                                                                   |
| --------------- | ------- | -------- | --------------------------------------------------------------------------------------- |
| `target_window` | string  | no       | tmux window target like `mysession:0`; session 1-64 `[A-Za-z0-9_-]`, window name (1-64) or numeric index. Omit to use tmux's current window. |
| `disable_input` | boolean | no       | When true, disable input on the newly-selected pane (`-d`). Mutually exclusive with `enable_input`. Default `false`. |
| `enable_input`  | boolean | no       | When true, re-enable input on the newly-selected pane (`-e`). Mutually exclusive with `disable_input`. Default `false`. |
| `zoom_toggle`   | boolean | no       | When true, also toggle the pane's zoom state (`-Z`). Default `false`.                   |

The schema sets `additionalProperties: false`, so any unknown field is
rejected with `-32602` before tmux is consulted. `disable_input` and
`enable_input` are also mutually exclusive — setting both is rejected
with `-32602` rather than letting tmux silently honour one of them.

### Output

Plain text block: `ok`.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Malformed `target_window`, both `disable_input` and `enable_input` set, or an unknown field was sent. |
| `-32000` | `target_window` does not resolve on this server (`errs.ErrSessionNotFound`), or the window has no "previously active" pane to flip back to. |
| `-32603` | tmux refused the toggle for an unexpected reason.                  |

### Example

```jsonc
{ "target_window": "demo:0", "zoom_toggle": true }
```

A typical chain after splitting and driving a side-car pane:

```jsonc
{ "name": "pane_split", "arguments": { "session": "demo", "direction": "horizontal" } }
{ "name": "send_keys",  "arguments": { "session": "demo", "keys": ["tail -F log", "Enter"] } }
{ "name": "last_pane",  "arguments": { "target_window": "demo:0" } }
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

## `run_shell`

Execute a one-shot shell command on the tmux server host via
`tmux run-shell [-b] [-c <start_dir>] [-t <target>] <command>` and
return the captured stdout. Distinct from
[`pipe_pane`](#pipe_pane) (which hooks pane I/O to a long-running
shell pipeline) and [`send_keys`](#send_keys) (which types into a
pane and lets the running process see the input): `run_shell` runs
**outside** any pane and surfaces the command's stdout back through
the JSON response.

The implementation redirects the command's stdout/stderr into a
private temp file (path under `os.TempDir`, removed before the call
returns) because tmux's own `run-shell` writes output to view-mode in
the active pane rather than back to the calling client; the
controller wraps the user payload as `{ <command>; } >'<tmpfile>' 2>&1`
so the bytes round-trip cleanly. With `background=true` the wrapper
is bypassed: tmux runs the command detached, returns immediately, and
the response carries an empty `stdout`.

> **CAUTION** — `run_shell` runs ARBITRARY shell commands on the
> tmux-mcp host. tmux executes `command` via `/bin/sh`; **the command
> itself is not sandboxed by this server**, so an agent that can call
> this tool can run any program the server's uid is allowed to run.
> Operators must trust the agents reaching for `run_shell` — gate the
> surface away from untrusted clients with the `-allowlist` flag (see
> [`docs/flags.md`](flags.md)) or run the server with `-read-only`,
> which excludes `run_shell` (and every other mutating tool) from the
> dispatcher entirely.
>
> Mutating in spirit (it executes shell side effects), so `run_shell`
> is **not** allowed under `-read-only`.

### Input

| Field        | Type    | Required | Default | Notes                                                                                          |
| ------------ | ------- | -------- | ------- | ---------------------------------------------------------------------------------------------- |
| `command`    | string  | yes      | —       | Shell pipeline tmux runs via `/bin/sh`. Capped at 4096 bytes; NUL and other ASCII control bytes (except tab) are rejected. |
| `start_dir`  | string  | no       | `""`    | When set, tmux chdir's into this directory before exec'ing /bin/sh (`-c <start-dir>`). Must be an absolute path. |
| `target`     | string  | no       | `""`    | Pane/session target (`session`, `session:window`, or `session:window.pane`) tmux uses for format-string evaluation (`-t`). |
| `background` | boolean | no       | `false` | When true, run the command detached (`-b`); tmux returns immediately and stdout is discarded. |

`command` must be valid UTF-8; the boundary rejects NUL bytes and
ASCII control characters (newline, ESC, DEL, …). Tab (0x09) is
allowed for spacing. `target`, when supplied, must match the same
conservative pane-target regex used elsewhere on the surface.

### Output

JSON block: `{"stdout": "<captured>"}`. `stdout` is the raw bytes
the shell wrote to fd 1+2 inside the wrapper (tmux's own `run-shell`
output discipline does not reach the caller, so what you get back
is the brace group's combined stdout/stderr). With `background=true`
`stdout` is always the empty string — tmux discards the detached
command's output and the controller has nothing to return.

### Errors

| Code     | Cause                                                                                                                |
| -------- | -------------------------------------------------------------------------------------------------------------------- |
| `-32602` | Empty/oversize/non-UTF-8/control-character `command`, malformed `target`, or non-absolute `start_dir`.                |
| `-32000` | `target` does not resolve on this server (`errs.ErrSessionNotFound`). The handler runs an up-front `has-session` probe because tmux's `run-shell -t <missing>` would otherwise silently fall back to the current/global context. |
| `-32603` | The wrapped shell command exited non-zero, or tmux refused the call for any other reason. The shell's own diagnostics land in the captured `stdout` (combined fd 1+2), so the caller can read them after handling the error. |

### Example

Capture a command's output:

```jsonc
{ "command": "git rev-parse HEAD" }
```

Run a build in a specific working directory:

```jsonc
{ "command": "make build", "start_dir": "/srv/projects/demo" }
```

Fire-and-forget a notification (no output captured, returns
immediately):

```jsonc
{ "command": "curl -fsS https://hooks.example/ping &", "background": true }
```

Pin format-string evaluation to a specific session (tmux substitutes
`#{}` against that session's context before exec'ing /bin/sh):

```jsonc
{ "command": "echo target=#{session_name}", "target": "demo" }
```

---

## `copy_mode`

Enter (or leave) tmux's copy-mode in a target pane via
`tmux copy-mode [-Hu] [-q] [-M] [-s SRC_PANE] [-t TARGET_PANE]`.
copy-mode puts the pane into scrollback / selection mode so a
subsequent [`send_keys`](#send_keys) call can drive copy-mode key
bindings (cursor motion, search, copy-selection, …); pass
`exit=true` to leave copy-mode and return the pane to its normal "type
commands at the shell" state. Distinct from
[`capture`](#capture) (which reads scrollback into a string without
touching pane state) and [`send_keys`](#send_keys) (which types into
whatever mode the pane is already in): `copy_mode` is the verb that
flips the mode state itself, so an agent can pre-position the pane
before sending the copy-mode key bindings that drive selection / yank.

### Input

| Field         | Type    | Required | Notes                                                                                       |
| ------------- | ------- | -------- | ------------------------------------------------------------------------------------------- |
| `target`      | string  | yes      | Pane target (`session`, `session:window`, `session:window.pane`, or `%N`).                  |
| `src_pane`    | string  | no       | Optional source pane whose scrollback is cloned into the target before entry (`-s`).        |
| `exit`        | boolean | no       | When true, quit copy-mode immediately if the target is in it (`-q`); default false.         |
| `scroll_down` | boolean | no       | When true, anchor the cursor at the bottom of the visible region (`-u`); default false.     |
| `mouse`       | boolean | no       | When true, start copy-mode in mouse-drag selection (`-M`); default false.                   |
| `drag_mode`   | boolean | no       | When true, enter HALFLINE drag-mode (`-H`, equivalent of pressing `H` interactively).       |

`target` and `src_pane` must match
`^[A-Za-z0-9_-]+(:[0-9]+(\.[0-9]+)?)?$` (or a tmux `%N` pane id) — the
same conservative shape the other pane tools accept. tmux's `-e`
("exit when status-bar drag finishes") is intentionally not surfaced;
add it later if a concrete need shows up.

### Output

JSON block: `{"ok": true, "target": "<target>", "exit": <bool>}`. When
`src_pane` was supplied the echo also carries it as `"src_pane":
"<src>"`. The echoed `target` / `src_pane` are the logical
(caller-supplied) values, so a `-session-prefix` deployment never
leaks the prefixed identity back to the caller.

### Errors

| Code     | Cause                                                                                                                |
| -------- | -------------------------------------------------------------------------------------------------------------------- |
| `-32602` | Missing/empty `target`, or a `target` / `src_pane` that does not match the pane regex.                               |
| `-32000` | The target session/window/pane does not exist on this server (`errs.ErrSessionNotFound`).                            |
| `-32603` | tmux refused the copy-mode call for any other reason.                                                                |

### Example

```jsonc
// Enter copy-mode on the active pane of session "demo" so a follow-up
// send_keys can drive Up / PageUp / "?pattern" / Enter / "y" bindings.
{ "name": "copy_mode", "arguments": { "target": "demo:0.0" } }

// Clone another pane's scrollback before entering copy-mode (useful
// for inspecting a sibling pane's history without making it active).
{ "name": "copy_mode", "arguments": { "target": "demo:0.0", "src_pane": "demo:0.1" } }

// Done inspecting — leave copy-mode and return to the shell.
{ "name": "copy_mode", "arguments": { "target": "demo:0.0", "exit": true } }
```

Pair with `send_keys` to drive the copy-mode bindings, and with
`capture` (or `display_message #{?pane_in_mode,1,0}`) to confirm the
pane really is in copy-mode before issuing the key sequence.

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

When the failed process owned the *whole* window (a build watcher in a
single-pane window, an interactive TUI that filled the window) reach
for [`respawn_window`](#respawn_window) instead — it re-runs the command
at window scope and shares the same `-32005` recovery contract.

---

## `respawn_window`

Restart the command in an existing window via
`tmux respawn-window [-k] [-c <cwd>] -t <session>:<window> [command]`.
Window-scoped sibling of [`respawn_pane`](#respawn_pane): where
`respawn_pane` re-runs a single pane's command, `respawn_window`
re-runs the whole window. Reach for it when a window-level workflow
(a build watcher that owned its window's only pane, a REPL inside a
single-pane window, a long-running daemon) has exited and the agent
wants to bring it back without recreating the window and reshuffling
the surrounding session layout. The `#{window_id}` and the window's
slot in the session are preserved — only the foreground process
changes.

### Input

| Field     | Type    | Required | Notes                                                                              |
| --------- | ------- | -------- | ---------------------------------------------------------------------------------- |
| `session` | string  | yes      | session id; len 1-64, regex `^[A-Za-z0-9_-]+$`                                     |
| `window`  | string  | yes      | window name (`^[A-Za-z0-9_-]+$`, len 1-64) or numeric index (`^[0-9]+$`)           |
| `command` | string  | no       | optional command (≤ 4096 bytes, no newlines); empty re-runs the window's original  |
| `cwd`     | string  | no       | optional starting directory; absolute path required if set (tmux `-c`)             |
| `kill`    | boolean | no       | when `true`, tmux SIGKILLs the running process before respawning (`-k`); default `false` |

`command` is forwarded to tmux as a single trailing argv and run via
`/bin/sh -c` on the tmux side. Newlines (`\n` / `\r`) are rejected
up front — they would otherwise break the "single command" contract
when tmux hands the string to the shell. The `cwd` field uses the
same absolute-path policy as `session_create`.

### Output

JSON block: `{"respawned": true}`. The window keeps its tmux
`#{window_id}` and its slot in the session — only the foreground
process is replaced. Follow up with `capture` / `wait_for_text` if
you need to observe the restarted command's output.

### Errors

| Code     | Cause                                                                              |
| -------- | ---------------------------------------------------------------------------------- |
| `-32602` | Missing/malformed `session` / `window`, relative `cwd`, or `command` with newline / over 4096 bytes. |
| `-32000` | `session:window` does not resolve on this server (`errs.ErrSessionNotFound`).      |
| `-32005` | Window is still running its original command and `kill` was not set (`errs.ErrPaneActive`). Retry with `kill=true`. The same code [`respawn_pane`](#respawn_pane) emits — clients can branch on it once and reuse the recovery path for both tools. |
| `-32603` | tmux refused the respawn for any other reason (e.g. internal tmux failure).        |

### Example

Bring a crashed build watcher back to life at window scope without
disturbing other windows in the session. The first attempt without
`kill` will trip `-32005` if the old process is still running; the
typed code lets the agent recover deterministically:

```jsonc
{ "name": "respawn_window",
  "arguments": { "session": "demo", "window": "build",
                 "command": "npm run watch" } }

// If the previous command is still active, retry with kill=true:
{ "name": "respawn_window",
  "arguments": { "session": "demo", "window": "build",
                 "command": "npm run watch", "kill": true } }
```

Reuse the original starting command but switch to a fresh working
directory (e.g. after `git worktree remove` left the old path
dangling):

```jsonc
{ "name": "respawn_window",
  "arguments": { "session": "demo", "window": "0",
                 "cwd": "/srv/build/v2", "kill": true } }
```

For pane-level recovery use [`respawn_pane`](#respawn_pane) — it shares
the same `kill` semantics and `-32005` recovery contract.

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

## `last_window`

Switch the named session back to its previously-active window via
`tmux last-window -t <target>`. tmux remembers the last active window
per session and toggles between the "current" and the "last" slot —
the equivalent of the interactive `prefix + l` (or the customary
`Alt-a`) hot key, which agents reach for to flip between two related
contexts (editor / build, code / repl) without having to remember the
destination's index or name. Pairs with `window_select` for explicit
targets and `window_create` / `window_kill` for lifecycle.

### Input

| Field    | Type   | Required | Notes                                                   |
| -------- | ------ | -------- | ------------------------------------------------------- |
| `target` | string | yes      | existing session id; len 1-64, regex `^[A-Za-z0-9_-]+$` |

The schema sets `additionalProperties: false`, so a typo like `session`
(the natural reflex from `window_select`) is rejected with `-32602`
before tmux is consulted, rather than silently behaving like a no-op
against the default target.

### Output

Plain text block: `ok`.

### Errors

| Code     | Cause                                                                                                                |
| -------- | -------------------------------------------------------------------------------------------------------------------- |
| `-32602` | Missing/invalid `target`, or an unknown field was sent.                                                              |
| `-32000` | `target` does not exist on this server (`errs.ErrSessionNotFound`).                                                  |
| `-32603` | tmux refused the toggle for an unexpected reason — most commonly "no last window" when the session has only ever had one window. |

### Example

```jsonc
{ "target": "demo" }
```

A typical "flip back-and-forth" chain looks like:

```jsonc
{ "name": "window_select", "arguments": { "session": "demo", "target": "editor" } }
{ "name": "window_select", "arguments": { "session": "demo", "target": "build" } }
{ "name": "last_window",   "arguments": { "target": "demo" } }   // ➔ back to "editor"
{ "name": "last_window",   "arguments": { "target": "demo" } }   // ➔ back to "build"
```

`last_window` mutates the active-window pointer, so it is **not**
included in the `-read-only` allowlist.

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

## `set_option`

Set or clear a tmux option via `tmux set-option`. Mirrors the read-side
`show_options` tool so an agent that wants to flip a runtime knob
(`status-interval`, `automatic-rename`, `remain-on-exit`, …) does not
have to spawn a subshell. **Mutates tmux state**, so this tool is
excluded from the read-only allowlist.

Scopes mirror `show_options` and tmux's own flag set:

- `server` — server-wide options (`tmux set-option -s NAME VALUE`). The
  `target` argument is ignored — server options have no
  session/window/pane qualifier.
- `session` — per-session override on the named session
  (`tmux set-option -t SESSION NAME VALUE`). This is the default scope.
- `window` — per-window override on `SESSION:WINDOW`
  (`tmux set-option -w -t SESSION:WINDOW NAME VALUE`).
- `pane` — per-pane override on a specific pane (`tmux set-option -p -t
  PANE NAME VALUE`). Pane options are a tmux 3.4+ concept; older builds
  reject `-p` and the call surfaces as `-32603`.

Pass `unset: true` to clear the override (`tmux set-option -u`); the
`value` field is ignored on this path and may be omitted. tmux's own
contract for `set-option -u` is "if no override exists, do nothing and
exit 0", so an over-eager unset is a no-op rather than an error —
matching the behaviour an agent would see from the CLI.

### Input

| Field    | Type    | Required                                  | Default     | Notes                                                                                                                                |
| -------- | ------- | ----------------------------------------- | ----------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| `name`   | string  | yes                                       | —           | option name; len 1-128, regex `^[A-Za-z0-9_-]+$`                                                                                     |
| `value`  | string  | yes when `unset` is `false`               | —           | option value, capped at 4096 bytes. Empty string is a legitimate value tmux will store verbatim                                      |
| `scope`  | string  | no                                        | `"session"` | one of `server`, `session`, `window`, `pane`                                                                                         |
| `target` | string  | yes when `scope` is `session`/`window`/`pane` | —       | session name (`scope=session`); `SESSION:WINDOW` (`scope=window`); `SESSION:WINDOW.PANE` or `%N` (`scope=pane`)                      |
| `unset`  | boolean | no                                        | `false`     | when true, clear the override (`-u`) instead of setting a value                                                                      |

The schema sets `additionalProperties: false`, so any unknown field is
rejected up front.

### Output

JSON block echoing the resolved scope and the branch taken so a caller
inspecting the response can tell which side of the set/unset switch
landed:

```jsonc
{
  "set":   true,
  "unset": false,
  "name":  "status-interval",
  "scope": "session"
}
```

When `unset=true`, the envelope flips to `{"set": false, "unset": true,
…}`.

### Errors

| Code     | Cause                                                                                                                                                |
| -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-32602` | Missing/invalid `name`; missing `target` for session/window/pane scope; unknown `scope`; `value` over the 4 KiB cap; or an unknown field was sent.   |
| `-32000` | Referenced session does not exist on this server (`errs.ErrSessionNotFound`).                                                                        |
| `-32603` | tmux refused the call (unknown option name, version mismatch on `scope=pane`, etc.).                                                                 |

### Examples

```jsonc
// Server-wide: bump the maximum buffer count.
{ "name": "buffer-limit", "value": "75", "scope": "server" }

// Session-scoped (default): faster status-line refresh on the "demo" session.
{ "name": "status-interval", "value": "1", "target": "demo" }

// Window-scoped: turn off automatic renaming for the first window.
{ "name": "automatic-rename", "value": "off", "scope": "window", "target": "demo:0" }

// Pane-scoped (tmux 3.4+): keep a pane open after its command exits.
{ "name": "remain-on-exit", "value": "on", "scope": "pane", "target": "demo:0.0" }

// Unset: drop the per-session override and revert to the default.
{ "name": "status-interval", "scope": "session", "target": "demo", "unset": true }
```

Pair with `show_options` to confirm the override actually landed:

```jsonc
{ "name": "set_option",   "arguments": { "name": "status-interval", "value": "5", "target": "demo" } }
{ "name": "show_options", "arguments": { "scope": "session", "session": "demo" } }
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

## `show_window_options`

Read the resolved tmux **window-options** table for a target window.
Wraps `tmux show-window-options [-g] [-t TARGET] [OPTION]` and returns
an ordered list of `{ name, value }` pairs in the same alphabetical
order tmux itself prints. Sister of the (write-side) `set_window_option`
tool, and a finer-grained complement to `show_options` (which carries a
`scope` discriminator). Useful when an LLM agent needs to introspect a
window's per-window flags — `synchronize-panes`, `automatic-rename`,
`mode-keys` — before deciding whether to flip them.

This tool is on the **`-read-only` allowlist**: it never mutates tmux
state, so a server started in inspection-only mode still serves it.

### Input

| Field    | Type    | Required | Default | Notes                                                                                                            |
| -------- | ------- | -------- | ------- | ---------------------------------------------------------------------------------------------------------------- |
| `target` | string  | no       | —       | Window target. `<session>` or `<session>:<window>`; len 1-129 (session ≤64, window ≤64). Empty = tmux's current. |
| `name`   | string  | no       | —       | Single option to fetch (e.g. `synchronize-panes`, `mode-keys`); ≤64 chars. Empty queries every option.           |
| `global` | boolean | no       | `false` | When true, query the global window-options defaults (`-g`) instead of the per-window override map.               |

The schema sets `additionalProperties: false`, so a typo in a field
name surfaces as a fast `-32602` rejection rather than silently doing
the wrong thing.

### Output

JSON block:

```jsonc
{
  "options": [
    { "name": "automatic-rename", "value": "on" },
    { "name": "mode-keys", "value": "emacs" },
    { "name": "synchronize-panes", "value": "on" }
  ]
}
```

Values are returned verbatim — including any quoting tmux emits for
strings with embedded specials. An empty result (`{"options": []}`)
means either no per-window overrides are set on the target (the common
case for a freshly created window) or the requested `name` is currently
unset; it is never an error condition.

### Errors

| Code     | Cause                                                                                                                                        |
| -------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `-32602` | Bound violation on `target` / `name`, or an unknown field was sent.                                                                          |
| `-32000` | Referenced session/window does not exist on this server (`errs.ErrSessionNotFound`; tmux 3.4 surfaces "no such window" for this branch).     |
| `-32603` | tmux refused the call for any other reason.                                                                                                  |

### Examples

```jsonc
// Every per-window override on the first window of "demo".
{ "target": "demo:0" }

// Just one flag, scoped to a specific window.
{ "target": "demo:build", "name": "synchronize-panes" }

// Global window-options defaults (always populated).
{ "target": "demo:0", "global": true }
```

Pair with `set_window_option` when the agent needs to flip a flag and
verify the live value:

```jsonc
{ "name": "set_window_option",  "arguments": { "target": "demo:0", "name": "synchronize-panes", "value": "on" } }
{ "name": "show_window_options", "arguments": { "target": "demo:0", "name": "synchronize-panes" } }
```

---

## `show_environment`

Inspect the environment future panes will inherit, via
`tmux show-environment`. Read-only counterpart of
[`set_environment`](#set_environment): use `set_environment` to
mutate the table, `show_environment` to read it back. Works against
either the per-session table (the default) or the server-wide global
table. Pass `name` to narrow the response to a single variable, or
omit it to dump the full scope.

Scopes mirror tmux's own flag set:

- `session` (default) — read the named target session's environment
  table (`tmux show-environment -t TARGET`). `target` is required.
  Future panes spawned inside that session inherit the entries
  reported here; existing panes keep whatever environment they
  already have.
- `global` — read the server-wide table
  (`tmux show-environment -g`). New sessions inherit these entries on
  creation; `target` is ignored.

tmux marks variables that have been *explicitly removed* in a scope
(typically because the session has hidden an inherited global) with a
leading dash on the `show-environment` line (e.g. `-FOO`). The tool
surfaces these as `present=false` so a caller can tell "tmux has a
record that this variable is unset" apart from "tmux has no record
of this variable at all" — the latter, queried via the single-`name`
form, also comes back with `present=false` so an agent's "is FOO
set?" check is always a single read.

### Input

| Field    | Type   | Required                  | Default     | Notes                                                                                |
| -------- | ------ | ------------------------- | ----------- | ------------------------------------------------------------------------------------ |
| `name`   | string | no                        | —           | Variable name to narrow the response to a single entry. Len 1-128, regex `^[A-Za-z_][A-Za-z0-9_]*$`. |
| `scope`  | string | no                        | `"session"` | One of `session`, `global`.                                                          |
| `target` | string | yes when `scope=session`  | —           | Target session id; len 1-64, regex `^[A-Za-z0-9_-]+$`. Ignored for `scope=global`.   |

`additionalProperties: false` — typo'd field names are rejected at
the boundary with `-32602`.

### Output

Two response shapes, picked by whether `name` was supplied.

**Whole-table form (`name` omitted):**

```jsonc
{
  "vars": {
    "EDITOR": "vim",
    "MCP_FOO": "bar",
    "EMPTY":   ""        // legal "set to empty" — distinct from removed
  },
  "removed": ["LEGACY_VAR"]   // entries tmux reported with a leading dash
}
```

`vars` carries every entry tmux currently reports as present (the
common case — an agent asking "what env will future panes see?"
needs a single key lookup). `removed` is a flat list of names tmux
emitted with a leading `-` (i.e. explicitly hidden on top of the
inherited scope); the slice is empty when no removals exist.

**Single-name form (`name` supplied):**

```jsonc
{
  "name":    "MCP_FOO",
  "value":   "bar",
  "present": true
}
```

`present=false` covers two related cases on purpose:

- tmux has no record of the variable in this scope (the
  never-set case). tmux's `unknown variable: NAME` stderr is
  translated into `present=false`, not a wire error, so an agent's
  "is FOO set?" probe is always a single call.
- tmux has a record but it is the explicit `-NAME` removal form.

If you need to distinguish those two — auditing whether someone
actively cleared a global default vs. whether the variable simply
was never assigned — fall back to the whole-table form and look for
the name in the `removed` slice.

### Errors

| Code     | Cause                                                                                                          |
| -------- | -------------------------------------------------------------------------------------------------------------- |
| `-32602` | Unknown `scope`; `target` missing for `scope=session`; bad `name` (regex/length); unknown field on `arguments`. |
| `-32000` | Referenced session does not exist on this server (`errs.ErrSessionNotFound`).                                  |
| `-32603` | tmux refused the call for any other reason.                                                                    |

### Examples

```jsonc
// Whole table for a single session.
{ "scope": "session", "target": "demo" }

// Default scope is "session" — same call, terser.
{ "target": "demo" }

// Single variable on a session.
{ "name": "MCP_FOO", "target": "demo" }

// Whole server-wide global table.
{ "scope": "global" }

// Single variable from the global table.
{ "name": "EDITOR", "scope": "global" }
```

Common chain: write a value with `set_environment`, then confirm it
landed where future panes will see it.

```jsonc
{ "name": "set_environment",  "arguments": { "name": "MCP_FOO", "value": "bar", "target": "demo" } }
{ "name": "show_environment", "arguments": { "name": "MCP_FOO", "target": "demo" } }
```

This is read-only and on the `-read-only` allowlist alongside other
inspection tools.

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

## `next_window`

Advance the session's active window pointer to the next window via
`tmux next-window -t <target>`. tmux walks the session's window list in
index order and wraps around at the end, so calling this on the last
window lands on the first one. Pairs with `window_select` (jump to a
specific target) by offering the "step forward" idiom an agent reaches
for when it does not know the concrete next index up front.

This tool **mutates** session state: it changes which window is active.
It is therefore **not** included in the `-read-only` allowlist — a
server armed with `-read-only` rejects `next_window` calls with
`CodeReadOnly` (`-32011`) before any handler runs.
## `previous_window`

Move the targeted session's active window pointer one slot backward
via `tmux previous-window -t <target>`. tmux wraps from index 0 to
the highest-numbered window so a session sitting on its first window
does not refuse the call — it lands on the last one instead. Useful
for an agent stepping backward through a sequence of sibling windows
without having to enumerate them via `list_windows` first.

Sibling of `next_window`; the two tools are deliberately symmetric so
an agent that drives one does not need to relearn the schema for the
other. Mutates state (it shifts the session's active-window pointer)
so it is **not** allowed under `-read-only`.

### Input

| Field        | Type    | Required | Default | Notes                                                                              |
| ------------ | ------- | -------- | ------- | ---------------------------------------------------------------------------------- |
| `target`     | string  | yes      | —       | existing session id; len 1-64, regex `^[A-Za-z0-9_-]+$`                            |
| `with_alert` | boolean | no       | `false` | when `true`, skip to the next window with a monitor-activity / monitor-bell alert (tmux's `-a` flag) |

The schema sets `additionalProperties: false`, so any unknown field is
rejected with `-32602` before tmux is consulted.

`with_alert=true` is the load-bearing setting for an agent watching a
long-lived session for whatever raised an alert: without it a session
with many idle windows is stepped through one-by-one, with it the
pointer hops directly to whichever window has new activity. This is
the same semantics tmux's interactive `next-window -a` keybinding
produces.

### Output

Plain text block: `ok`.

The boundary deliberately does not echo the post-step layout — a
follow-up `list_windows` is one call away if the caller wants to
confirm which window the pointer landed on.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | Missing/invalid `target`, or an unknown field was sent.            |
| `-32000` | `target` does not exist on this server (`errs.ErrSessionNotFound`). |
| `-32011` | Server is armed with `-read-only`; `next_window` is not on the inspection allowlist. |
| `-32603` | tmux refused the step for an unexpected reason.                    |
| `with_alert` | boolean | no       | `false` | when `true`, tmux skips windows that are not alert-flagged and lands on the previous one that *is* (`-a`) |

The schema sets `additionalProperties: false`, so any field other than
`target` / `with_alert` is rejected with `-32602` (invalid params)
before tmux is consulted.
## `unlink_window`

Remove a window reference from a session via
`tmux unlink-window -t <session>:<window>`. The inverse of
`link_window`: where link-window grafts a window's `#{window_id}` into
a second session's slot, unlink-window detaches the named slot from
that session — leaving the window itself alive in any other sessions
still referencing the same id. Pairs with `link_window` (graft) and
`window_move` (relocate, removing the source).

### Input

| Field    | Type    | Required | Default | Notes                                                                                |
| -------- | ------- | -------- | ------- | ------------------------------------------------------------------------------------ |
| `target` | string  | yes      | —       | window reference like `mysession:0`; session 1-64 [A-Za-z0-9_-], window name (same regex/length policy) or numeric index (`\d+`) |
| `kill`   | boolean | no       | `false` | when `true`, unlink even the last reference (destroys the window) — tmux's `-k` flag |

The schema sets `additionalProperties: false`, so any unknown field is
rejected up front (a typo like `"targets"` fails fast instead of
silently producing a missing-target rejection).

`kill=false` (the default) is the right setting when the window lives
in another session that should keep it: tmux refuses to unlink the
last reference because doing so would also reap the underlying window,
and the refusal is surfaced as `-32603` so the caller can react. Set
`kill=true` once no session needs the linked window any longer — that
flag tells tmux to proceed even on the last reference, destroying the
window in the process. The boundary does not pre-flight the reference
count: letting tmux refuse yields the same answer with one fewer
round-trip and avoids racing a concurrent link/unlink.
## `rotate_window`

Cycle the panes inside a window through the existing layout slots via
`tmux rotate-window [-U|-D] -t <target>`. tmux keeps the layout shape
(even-horizontal, main-vertical, tiled, …) intact and only rotates
which pane occupies which slot — a three-pane row `A B C` becomes
`B C A` under the default `-U`, and `C A B` under `-D`.

This is **distinct** from a future `next_layout` / `previous_layout`
pair (which would cycle through the preset layout templates):
`rotate_window` leaves the active layout in place and only shuffles
the panes within it. It also pairs with `swap_window` (which trades
two *windows* between session slots) and `pane_swap` (which trades
two *panes* in place without rotating the rest of the window).

### Input

| Field      | Type    | Required | Default | Notes                                                                                                  |
| ---------- | ------- | -------- | ------- | ------------------------------------------------------------------------------------------------------ |
| `target`   | string  | yes      | —       | Window target. Bare session name (`demo`) rotates the active window; `<session>:<window>` (`demo:0`) pins a specific window. Session 1-64, `^[A-Za-z0-9_-]+$`; window 1-64 (`^[A-Za-z0-9_-]+$`) or numeric (`\d+`). |
| `downward` | boolean | no       | `false` | When `true`, rotate the other way (tmux's `-D`). Default `false` emits the tmux-default `-U`.          |

The schema sets `additionalProperties: false`, so any unknown field is
rejected up front. Empty `target` is rejected with `-32602` rather
than letting tmux fall back to "the current window of the current
client" — almost never what an agent meant.

### Output

JSON text block:

```jsonc
{ "moved": true }
```

The boundary deliberately does not echo which window now carries the
active flag — a follow-up `list_windows` is one call away if the
caller wants to confirm the new slot.
{ "unlinked": true }
```

The boundary deliberately does not echo the post-unlink layout — a
follow-up `list_windows` is one call away if the caller wants to
confirm the destination dropped the slot.
{ "rotated": true }
```

The boundary deliberately does not echo the post-rotation pane order
— a follow-up `list_panes` is one call away if the caller wants to
confirm the new slot ordering.

### Errors

| Code     | Cause                                                                              |
| -------- | ---------------------------------------------------------------------------------- |
| `-32602` | Missing/invalid `target`, or an unknown field was sent.                            |
| `-32000` | `target` does not name a session on this server (`errs.ErrSessionNotFound`).       |
| `-32603` | tmux refused the call for an unexpected reason.                                    |
| `-32602` | Missing/invalid `target` (empty, no `:`, empty session/window half, regex/length violation); or an unknown field was sent. |
| `-32000` | A referenced session does not exist on this server, or the target does not match a window (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the unlink (e.g. `kill=false` against the only reference, or other unexpected tmux failure). |
| `-32000` | `target` does not match an existing session/window (`errs.ErrSessionNotFound`).    |
| `-32603` | tmux refused the rotation for an unexpected reason (e.g. a single-pane window).    |

### Example

```jsonc
{ "target": "demo" }
```

A typical chain looks like: snapshot the layout, step forward, drive
whichever window the pointer landed on.

```jsonc
{ "name": "list_windows", "arguments": { "session": "demo" } }
{ "name": "next_window",  "arguments": { "target": "demo", "with_alert": true } }
{ "name": "capture",      "arguments": { "session": "demo" } }
Pair with `list_windows` (before and after) when you need to confirm
the active flag actually flipped:

```jsonc
{ "name": "list_windows",    "arguments": { "session": "demo" } }
{ "name": "previous_window", "arguments": { "target": "demo" } }
{ "name": "list_windows",    "arguments": { "session": "demo" } }
{ "target": "monitor:1" }
```

Pair with `link_window` to undo a share once the monitor session no
longer needs the linked window — without `kill` the source session
keeps its copy untouched:

```jsonc
{ "name": "link_window",   "arguments": { "src_session": "build", "src_window": "watch", "dst_session": "monitor", "dst_window": "1" } }
{ "name": "unlink_window", "arguments": { "target": "monitor:1" } }
```

Or use `kill=true` to remove the last reference and destroy the
underlying window in one call:

```jsonc
{ "name": "unlink_window", "arguments": { "target": "build:watch", "kill": true } }
{ "target": "demo", "downward": false }
```

Pair with `list_panes` (before and after) when you need to confirm the
slot ordering actually shifted:

```jsonc
{ "name": "list_panes",     "arguments": { "session": "demo" } }
{ "name": "rotate_window",  "arguments": { "target": "demo" } }
{ "name": "list_panes",     "arguments": { "session": "demo" } }
```
## `next_layout`

Cycle the targeted window onto the next preset layout via
`tmux next-layout -t <target>`. Walks tmux's ordered preset ring
(`even-horizontal` → `even-vertical` → `main-horizontal` →
`main-vertical` → `tiled`) and wraps to the first preset after the
last. tmux applies the cycle to the targeted session's active window —
the agent does not need to know which window is current to use this.

Pairs with `select_layout` (which takes a SPECIFIC layout name) by
offering the "give me the next preset" affordance an agent reaches for
when it doesn't care which layout, just wants to rotate. Use
`next_layout` when the goal is "try a different arrangement"; use
`select_layout` when the goal is "land on this exact preset".

### Input

| Field    | Type   | Required | Default | Notes                                                                   |
| -------- | ------ | -------- | ------- | ----------------------------------------------------------------------- |
| `target` | string | yes      | —       | existing session id; len 1-64, regex `^[A-Za-z0-9_-]+$`. tmux applies the cycle to the session's active window. |

The schema sets `additionalProperties: false`, so any unknown field is
rejected up front with `-32602`.

### Output

A small text block containing the literal string `ok`. tmux's
`next-layout` itself produces no useful stdout; chain
`display_message` against `#{window_layout}` (or `list_windows`) if
you need to confirm the new arrangement.

### Errors

| Code     | Cause                                                                                           |
| -------- | ----------------------------------------------------------------------------------------------- |
| `-32602` | Missing or malformed `target` (regex / length policy violation), or an unknown field was sent.  |
| `-32000` | `target` does not match any session on this server (`errs.ErrSessionNotFound`).                 |
| `-32603` | tmux refused the cycle for an unexpected reason (e.g. a single-pane window that has no layout). |

### Example

Rotate the active window of `demo` onto its next preset:

```jsonc
{ "name": "next_layout", "arguments": { "target": "demo" } }
```

Drive a "try another arrangement" loop until something looks right:

```jsonc
{ "name": "display_message", "arguments": { "session": "demo", "format": "#{window_layout}" } }
{ "name": "next_layout",     "arguments": { "target": "demo" } }
{ "name": "display_message", "arguments": { "session": "demo", "format": "#{window_layout}" } }
```

`next_layout` mutates tmux state (it changes the active window's pane
arrangement), so it is rejected with `-32011` (`CodeReadOnly`) when
the server runs with `-read-only`.

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

## `link_window`

Share a window across sessions in place via
`tmux link-window -s <src_session>:<src_window> -t <dst_session>:<dst_window>`.
Unlike `window_move` (which relocates and removes the source) and
`swap_window` (which trades two windows of the same session),
`link_window` leaves the source intact: the same `#{window_id}` is
reachable from both sessions, so a long-running build window can be
exposed in a "monitor" session without losing the foreground in the
working session.

### Input

| Field         | Type    | Required | Default | Notes                                                                              |
| ------------- | ------- | -------- | ------- | ---------------------------------------------------------------------------------- |
| `src_session` | string  | yes      | —       | source session name; len 1-64, regex `^[A-Za-z0-9_-]+$`                            |
| `src_window`  | string  | yes      | —       | source window name (1-64, `^[A-Za-z0-9_-]+$`) or numeric index (`\d+`)             |
| `dst_session` | string  | yes      | —       | destination session name; same regex/length policy as `src_session`                |
| `dst_window`  | string  | yes      | —       | destination window name (same regex/length policy as `src_window`) or numeric index |
| `kill`        | boolean | no       | `false` | when `true`, overwrite an existing dst window instead of erroring (tmux's `-k` flag) |

The `(src_session, src_window)` and `(dst_session, dst_window)` pairs
must differ — passing the same `<session>:<window>` rejects with
`-32602` ("src and dst must differ") before tmux is consulted, so the
boundary surfaces a more informative error than tmux's own no-op
refusal. The schema sets `additionalProperties: false`, so any unknown
field is rejected up front (a typo like `"src_win"` fails fast instead
of silently producing a partial target).

`kill=true` is the right setting when an agent wants to repeatedly
expose the latest build into a fixed monitor slot — without it tmux
refuses the call with "index in use" once the slot is occupied.
`kill=false` (the default) is safer for one-shot links because it
prevents an accidental overwrite of a window the user might still be
attached to.
## `delete_buffer`

Drop a single named tmux paste buffer via `tmux delete-buffer -b NAME`.
Useful for an agent that stashed a snippet via `set_buffer` and wants
to release the storage once the value has been consumed — buffers
persist on the tmux server until explicitly deleted (or until tmux's
`buffer-limit` rotates them out), so a long-running agent that writes
many buffers should clean up the ones it no longer needs.

The boundary always requires `name`. tmux's bare `delete-buffer` (no
`-b`) drops the most-recently-added buffer, but exposing that
"delete the last thing you stored" path through a programmatic agent
invites accidental destruction of buffers another caller just minted.
Forcing the name keeps the operation deterministic from the caller's
point of view.

### Input

| Field  | Type   | Required | Notes                                                                                            |
| ------ | ------ | -------- | ------------------------------------------------------------------------------------------------ |
| `name` | string | yes      | buffer name to drop; len 1-128, regex `^[A-Za-z0-9_-]+$`. Empty string is rejected with -32602.  |

### Output

JSON text block:

```jsonc
{ "linked": true, "dst": "monitor:1" }
```

`dst` echoes the destination handle in the form the caller can hand
straight to `list_windows` / `send_keys` / `window_select` next, so a
follow-up step does not have to reconstruct the colon-joined target
itself. The src is omitted because the caller already supplied it.

### Errors

| Code     | Cause                                                                              |
| -------- | ---------------------------------------------------------------------------------- |
| `-32602` | Missing/invalid `src_session`, `src_window`, `dst_session`, or `dst_window`; both pairs equal; or an unknown field was sent. |
| `-32000` | A referenced session does not exist on this server, or one of the targets does not match a window (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the link (e.g. `kill=false` with the dst slot already in use, or other unexpected tmux failure). |
{
  "deleted": true,
  "name":    "pinned"   // echoed back so the caller can correlate the response with the request.
}
```

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | `name` missing/empty, outside the regex/length policy, or an unknown field on `arguments`. |
| `-32000` | The named buffer does not exist on this server (`errs.ErrSessionNotFound`); the same stable wire code `show_buffer` uses for the same conceptual outcome. |
| `-32603` | tmux refused the delete for any other reason (e.g. internal tmux failure). |

### Example

```jsonc
{
  "src_session": "build", "src_window": "watch",
  "dst_session": "monitor", "dst_window": "1",
  "kill": true
}
```

Pair with `list_windows` against the destination session to confirm
the linked window landed in the expected slot:

```jsonc
{ "name": "link_window",  "arguments": { "src_session": "build", "src_window": "watch", "dst_session": "monitor", "dst_window": "1" } }
{ "name": "list_windows", "arguments": { "session": "monitor" } }
{ "name": "pinned" }
```

A typical chain looks like: stash a snippet under a known name, read
it back once, then drop it so the buffer table stays small.

```jsonc
{ "name": "set_buffer",    "arguments": { "data": "the value", "name": "shared" } }
{ "name": "show_buffer",   "arguments": { "name": "shared" } }
{ "name": "delete_buffer", "arguments": { "name": "shared" } }
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

## `load_buffer`

Inject `data` into a tmux paste buffer via `tmux load-buffer -b NAME -`,
streaming the payload over the child's stdin. Behaviourally identical
to `set_buffer` — same `name` / `append` semantics, same `bufferN`
auto-naming when `name` is omitted, same 1 MiB cap — but the bytes
travel through the child's stdin pipe instead of as a positional argv
argument so very large payloads do not run into the OS argv length cap
(`ARG_MAX`, ~128 KiB on Linux, ~256 KiB on macOS) before tmux's own
buffer ceiling is reached.

Reach for `load_buffer` when you have a kilobyte-sized blob (a captured
logfile slice, a screenful of source code, a binary-safe snippet) and
want a discrete tool call to land it in a buffer; reach for
`set_buffer` when the payload is small and the single-argv shape is
preferable.

### Input

| Field    | Type    | Required | Notes                                                                                                                                |
| -------- | ------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| `data`   | string  | yes      | buffer payload, streamed verbatim over stdin. Empty string is allowed (creates an empty buffer; tmux 3.4 may drop empty buffers). Capped at 1 MiB (1 048 576 bytes). |
| `name`   | string  | no       | optional buffer name to pin; len 1-128, regex `^[A-Za-z0-9_-]+$`. When omitted, tmux assigns the next `bufferN`.                     |
| `append` | boolean | no       | when true, concatenate onto an existing buffer (`-a`) instead of replacing it. Defaults to false.                                     |
## `save_buffer`

Return the raw text content of a tmux paste buffer via
`tmux save-buffer - [-b NAME]` — semantically equivalent to
`show_buffer` but signals "this is the canonical save-path read".
The headline difference is `error_on_truncation`: when true (the
default), the handler returns a typed `-32010 oversized response`
directly if the marshalled body would exceed the server's configured
`-max-response-bytes` cap, so a caller cannot silently receive a
truncated payload. Pair with `list_buffers` to discover the names a
server is currently holding.

### Input

| Field                 | Type    | Required | Notes                                                                                                                                                                                                                                            |
| --------------------- | ------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `name`                | string  | no       | optional buffer name; len 1-128, regex `^[A-Za-z0-9_-]+$`. Empty / omitted → most-recent buffer.                                                                                                                                                  |
| `error_on_truncation` | boolean | no       | when true (the default), the handler refuses oversize bodies up front with `-32010` instead of letting the framing-level guard rewrite them after the fact. Pass `false` to ship the payload verbatim and let the dispatcher's cap fire if needed. |

### Output

JSON text block:

```jsonc
{
  "loaded": true,
  "name":   "buffer3"   // resolved buffer name; equal to the input `name` when one was pinned, otherwise the auto-assigned bufferN.
}
```

The resolved `name` is recovered the same way `set_buffer` does:
running `tmux list-buffers -F '#{buffer_created} #{buffer_name}'`
after the load and picking the most-recently-created entry. When the
caller pinned a `name`, that lookup is skipped — tmux honours
`-b NAME` verbatim.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | `data` missing or > 1 MiB; `name` outside the regex/length policy; or an unknown field on `arguments`. |
| `-32603` | tmux's load-buffer or follow-up list-buffers failed (rare; typically a fork/exec error). |

### Examples

```jsonc
// Stream a large snippet into tmux's auto-named buffer slot.
{ "data": "<5KB blob...>" }

// Pin a stable name so a follow-up show_buffer doesn't have to
// round-trip through list_buffers.
{ "data": "<payload>", "name": "snippet" }

// Append to an existing buffer (or create it under that name).
{ "data": "...continued", "name": "snippet", "append": true }
```

Pair with `show_buffer` to round-trip a snippet:

```jsonc
{ "name": "load_buffer", "arguments": { "data": "the value", "name": "shared" } }
{ "name": "show_buffer", "arguments": { "name": "shared" } }
  "name": "pinned",         // echoed back when the caller pinned a name; empty otherwise.
  "data": "the quick brown fox"
}
```

`data` is the buffer body verbatim — tmux does not append a trailing
newline, so an agent that expects one should add it explicitly.

### Errors

| Code     | Cause                                                                                                                                                                |
| -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-32602` | `name` outside the regex/length policy, or an unknown field on `arguments`.                                                                                          |
| `-32000` | The named buffer does not exist on this server (`errs.ErrSessionNotFound`).                                                                                          |
| `-32010` | `error_on_truncation=true` and the marshalled body would exceed the server's configured `-max-response-bytes` cap. Retry with a smaller scope or pass `false` to allow the dispatcher's framing-level handling. |
| `-32603` | tmux refused the save for an unexpected reason.                                                                                                                      |

### Example

```jsonc
// Strict read: error out if the canonical payload would not fit.
{ "name": "pinned" }

// Permissive read: ship whatever fits and let the dispatcher's
// framing-level cap rewrite the response if it gets too big.
{ "name": "pinned", "error_on_truncation": false }
```

A typical chain looks like: stash a snippet, list to discover the
assigned name, then read it back via the canonical save-path.

```jsonc
{ "name": "list_buffers", "arguments": {} }
{ "name": "save_buffer",  "arguments": { "name": "buffer0" } }
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

## `switch_client`

Redirect a tmux client between sessions on the same server via
`tmux switch-client [-c <client>] [-t <target>] [-l|-n|-p] [-r]`.
Use this to bounce an attached terminal from one session to another
without detaching: pass `target` to land on a specific session, or
set exactly one of `last` / `next` / `prev` to walk the session
list. `client` (the path-like name shown in `list_clients`,
e.g. `/dev/pts/0`) scopes the redirect to one terminal; omit it to
redirect the caller's current client. `read_only=true` toggles the
client's read-only / ignore-size flags (the same `-r` semantics as
`attach-session`).

`switch_client` is **mutating** (it changes which session a client
is bound to) so it is NOT on the read-only allowlist; a server
running with `-read-only` rejects it before any handler runs.

Headless servers with nothing attached are a successful no-op — the
boundary swallows tmux's `no current client` stderr so callers can
fire-and-forget without a separate `list_clients` round-trip.

### Input

| Field       | Type    | Required | Notes                                                                                                                                          |
| ----------- | ------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `client`    | string  | no       | Optional tmux client name (typically a TTY path like `/dev/pts/0`); regex `^/[A-Za-z0-9_./:-]+$`, max 256 bytes. Omit for the caller's client. |
| `target`    | string  | mostly   | Target session name (or `session:window[.pane]` / `%pane`). Required unless exactly one of `last` / `next` / `prev` is true.                   |
| `last`      | boolean | no       | Walk to the last (most-recently-visited) session via `-l`.                                                                                     |
| `next`      | boolean | no       | Walk forward to the next session via `-n`.                                                                                                     |
| `prev`      | boolean | no       | Walk backward to the previous session via `-p`.                                                                                                |
| `read_only` | boolean | no       | Toggle the client's read-only / ignore-size flags via `-r` on top of the directional choice.                                                   |

Exactly one of `target`, `last`, `next`, `prev` must be set; passing
zero or more than one is rejected with `-32602`. `read_only` rides
alongside any of those.

### Output

JSON text block:

```jsonc
{
  "switched": true
}
```

The boolean is always `true` on the success path, including the
headless no-op. Future fields (e.g. an echoed client name or a
resolved target) will land alongside without breaking callers that
only read `switched`.

### Errors

| Code     | Cause                                                                                                       |
| -------- | ----------------------------------------------------------------------------------------------------------- |
| `-32602` | malformed args, unknown field, or zero/multiple of `target` / `last` / `next` / `prev`.                     |
| `-32000` | named `client` is not attached, or named `target` session does not exist (`errs.ErrSessionNotFound`).       |
| `-32603` | tmux refused the switch for an unexpected reason.                                                           |

### Examples

Land an attached client on a specific session:

```jsonc
{
  "name": "switch_client",
  "arguments": { "client": "/dev/pts/0", "target": "build" }
}
```

Walk every client to the previous session and toggle read-only:

```jsonc
{
  "name": "switch_client",
  "arguments": { "prev": true, "read_only": true }
}
```

---

## `display_popup`

Render an overlay popup on the attached tmux client via
`tmux display-popup`. The popup is a rectangular overlay drawn on top
of any panes; pane contents are not refreshed while the popup is
visible. Sizing knobs (`width` / `height`) accept either a percentage
("80%") or a number of cells; omit them to let tmux centre a
half-the-terminal popup. `shell_command` runs inside the popup; omit it
to launch the user's default shell. Pair `close_on_exit` (`-C`) or
`close_on_zero_exit` (`-E`) to make the popup self-dismiss on exit.

The tool **mutates client UI state** and is therefore refused under
`-read-only` (`-32011`).

### Input

| Field                | Type             | tmux flag | Notes                                                                                          |
| -------------------- | ---------------- | --------- | ---------------------------------------------------------------------------------------------- |
| `target`             | string           | `-t`      | Optional pane target (`session`, `session:window`, `session:window.pane`, or `%N`).            |
| `title`              | string           | `-T`      | tmux format string for the popup title; max 4096 chars; no newlines.                           |
| `border_style`       | string           | `-S`      | tmux style spec applied to the border (e.g. `fg=red`); max 4096 chars; no newlines.            |
| `border_lines`       | string           | `-b`      | Border glyph set (`single`, `double`, `heavy`, `simple`, `rounded`, `padded`, `none`).         |
| `start_directory`    | string           | `-d`      | Absolute path used as the popup shell-command's cwd. Relative paths are rejected up front.     |
| `env`                | object           | `-e`      | Map of `KEY=VALUE` overrides; POSIX env names; values max 4096 chars; max 64 entries.          |
| `width`              | string           | `-w`      | Cells (`60`) or percentage (`60%`); regex `^[0-9]+%?$`; max 32 chars.                          |
| `height`             | string           | `-h`      | Same shape as `width`.                                                                         |
| `x`                  | string           | `-x`      | Same shape as `width`.                                                                         |
| `y`                  | string           | `-y`      | Same shape as `width`.                                                                         |
| `shell_command`      | string           |           | Trailing positional command tmux runs inside the popup; max 4096 chars; no newlines.            |
| `no_border`          | bool             | `-B`      | Suppresses the popup border. Overrides `border_lines` / `border_style` on tmux's side.         |
| `close_on_exit`      | bool             | `-C`      | Close the popup as soon as `shell_command` exits.                                              |
| `close_on_zero_exit` | bool             | `-E`      | Close the popup only when `shell_command` exits 0.                                             |
| `centered`           | bool             | `-r`      | Centre the popup on the active client. Requires tmux >= 3.5; older daemons reject the flag.    |

The schema sets `additionalProperties: false`, so a typo in any field
name is refused with `-32602` before tmux is consulted.

### Output

JSON text block:

```jsonc
{ "opened": true }
```

`opened` is a flat ack — display-popup is fire-and-forget at this
layer, so the boundary deliberately does not echo the supplied flags
back. Inspect the popup state with `display_message` /
`list_clients` if confirmation is needed.

### Errors

| Code     | Cause                                                                                                        |
| -------- | ------------------------------------------------------------------------------------------------------------ |
| `-32602` | Bad `target` shape, oversized free-form arg, relative `start_directory`, malformed env key, or unknown field. |
| `-32000` | `target` names a session / window / pane that does not exist (`errs.ErrSessionNotFound`).                     |
| `-32011` | Server is running with `-read-only`; `display_popup` mutates client UI state and is refused.                  |
| `-32603` | tmux refused the call for any other reason (no current client on a headless server, unknown flag on an older tmux, etc.). |

### Example

Open a centred status popup running a quick command:

```jsonc
{
  "name": "display_popup",
  "arguments": {
    "target": "demo:0.1",
    "title":  "review #{session_name}",
    "width":  "60%",
    "height": "40%",
    "border_lines": "rounded",
    "border_style": "fg=cyan",
    "shell_command": "git status",
    "close_on_exit": true,
    "env": { "PAGER": "cat" }
  }
}
```

A bare call lets tmux apply every default — half-the-terminal centred
popup, default border, the user's shell:

```jsonc
{ "name": "display_popup", "arguments": {} }
```

---

## `lock_server`

Lock the entire tmux server via `tmux lock-server` (alias `lock`). tmux
iterates every attached client on this controller's private daemon and
runs the configured `lock-command` (default `lock -np`) against each
one. Distinct from a session-scoped lock (which would target every
client attached to one named session) and the single-client variant
(one specific TTY): `lock_server` covers every screen on every session
this daemon is hosting in a single call.

The simplest of the three lock primitives — tmux's `lock-server` takes
no flags at all, so the boundary policy mirrors that exactly.

Headless servers with nothing attached are a successful no-op: tmux
still exits 0 because the iteration over attached clients is empty.
Mutating in spirit (it changes what every attached client's terminal
displays — the lock screen replaces the live session view across every
session on this daemon), so it is **not** allowed under `-read-only`.

### Input

No fields. Pass `{}` (or `null` — the handler accepts an empty
arguments value). The schema sets `additionalProperties: false`, so
any stray field (e.g. a `"session"` borrowed from `lock_session`, or
a `"client"` from `lock_client`) is rejected with `-32602` before tmux
is consulted.
## `set_environment`

Set or remove an environment variable that future panes will inherit,
via `tmux set-environment`. `scope=session` (the default) updates the
named session's environment table (`-t SESSION`); `scope=global`
updates the server-wide table (`-g`) so subsequently-created sessions
inherit it. Pass `value` to set the variable; omit `value` to remove
it (`-u NAME`).

Existing panes keep whatever environment they already have — only
newly spawned panes (e.g. via `new_window`, `pane_split`, or a fresh
`session_create` for `scope=global`) pick up the change. This mirrors
the underlying tmux semantics; the boundary does not invent an
"reload-running-shells" affordance.

### Input

| Field     | Type   | Required                  | Notes                                                                                                                                                                |
| --------- | ------ | ------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `name`    | string | yes                       | environment variable name; len 1-128, regex `^[A-Za-z_][A-Za-z0-9_]*$` (POSIX shape — leading digit, dashes, dots and shell metachars are rejected at -32602).  |
| `value`   | string | no                        | variable value. Omit to remove the variable (`-u NAME`); an explicit empty string `""` is a legal "set to empty", distinct from omission.                            |
| `scope`   | string | no (default `"session"`)  | one of `session` (`-t SESSION`) or `global` (`-g`). Anything else is rejected at -32602.                                                                              |
| `session` | string | yes when `scope=session`  | target session for `scope=session`; ignored when `scope=global`. Standard session-ref policy: len 1-64, regex `^[A-Za-z0-9_-]+$`. Routed through `-session-prefix` when configured. |

The schema sets `additionalProperties: false`, and the handler enforces
it at runtime — a typo'd field (e.g. `val` instead of `value`) is
rejected with `-32602` before any tmux command runs, rather than
silently behaving like the remove form.
## `paste_buffer`

Inject the contents of a tmux paste buffer into the targeted pane via
`tmux paste-buffer [-d] [-p] [-b NAME] -t TARGET`. Useful when an
agent has staged a snippet via `set_buffer` and now wants to deliver
it into a running shell or TUI without paying the per-keystroke cost
of `send_keys` on a long payload — tmux forwards the stored bytes
through its paste machinery, exactly as if the user had hit the
configured paste key. Pair with `set_buffer` upstream and (optionally)
`delete_after=true` for a one-shot snippet that does not linger in
the server's buffer table.

### Input

| Field          | Type    | Required | Notes                                                                                                                                |
| -------------- | ------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| `target`       | string  | yes      | tmux pane target (`"session"`, `"session:window"`, or `"session:window.pane"`). Same regex / length rules as elsewhere on the boundary. |
| `buffer_name`  | string  | no       | optional buffer to paste; len 1-128, regex `^[A-Za-z0-9_-]+$`. When omitted, the most-recently-added buffer is pasted (the bare `paste-buffer` CLI default). |
| `delete_after` | boolean | no       | when true, drop the buffer from tmux's list after the paste lands (`-d`). Defaults to false.                                          |
| `bracketed`    | boolean | no       | when true, wrap the paste in bracketed-paste escape sequences (`-p`) for applications that opt into bracketed-paste mode. Defaults to false. |
## `source_file`

Re-source a tmux config file via `tmux source-file PATH`. Useful for
hot-reloading tweaks (status bar, key bindings, options) without
restarting the tmux server. `path` must be an absolute filesystem
path; the boundary rejects relative paths, control characters, NUL
bytes, and `..` traversal segments before tmux is consulted. Set
`quiet=true` to map to `-q`, which tells tmux to suppress non-fatal
errors so a partially-incompatible config still reloads as far as it
can — the same flag agents reach for when they want a best-effort
reload that does not blow up on missing or out-of-version directives.

### Input

| Field   | Type    | Required | Notes                                                                                                                                |
| ------- | ------- | -------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| `path`  | string  | yes      | absolute filesystem path to the tmux.conf. Max 4096 bytes; rejects NUL, control characters, and `..` traversal segments.             |
| `quiet` | boolean | no       | when true, pass `-q` so tmux suppresses non-fatal errors (unknown options, missing file). Defaults to false.                         |
## `set_hook`

Bind or unbind a tmux command to a server / session-scoped event via
`tmux set-hook`. Hooks let an agent react to tmux's own lifecycle —
pane death, client attach, session creation — without polling, so
long-running supervisors can install once and forget.

`name` is the event (e.g. `pane-died`, `client-attached`,
`session-created`); `command` is the tmux command line tmux will run
when the event fires (e.g. `display-message "x"`,
`run-shell ./on-pane-died.sh`). Set `unset=true` to clear an existing
hook (`-u`); on the unset path `command` is ignored. Set
`global=true` to bind on the server-wide options table (`-g`) so
every current and future session inherits the hook; otherwise
`target` names the session whose options table the hook lands on
(`-t TARGET`). Mutating: hooks change the daemon's behaviour for
every subsequent event, so this tool is **not** allowed under
## `previous_layout`

Cycle the targeted window's pane arrangement one step BACKWARD through
tmux's preset ring via `tmux previous-layout -t <target>`. The five
presets tmux ships (`even-horizontal`, `even-vertical`,
`main-horizontal`, `main-vertical`, `tiled`) walk in reverse — wrapping
from the first preset to the last so the call never refuses on an
edge. Sibling of `next_layout`; pair with `select_layout` when you want
to jump to a specific preset or stored layout dump rather than step
through the ring.

`previous_layout` MUTATES tmux state (it changes a window's pane
arrangement) so it is intentionally NOT part of the read-only
allowlist and is rejected when the operator runs the server with
`-read-only`.

### Input

| Field     | Type    | Required | Notes                                                                                                            |
| --------- | ------- | -------- | ---------------------------------------------------------------------------------------------------------------- |
| `name`    | string  | yes      | hook event name; len 1-128, regex `^[A-Za-z0-9_-]+$`.                                                            |
| `command` | string  | conditional | required when `unset=false`; len 0-4096, no NUL or other ASCII control bytes (tab is allowed). Ignored when `unset=true`. |
| `unset`   | boolean | no       | when true, clear the hook (`-u`) instead of binding it. Defaults to false.                                       |
| `global`  | boolean | no       | when true, bind on the server-wide options table (`-g`); `target` is ignored. Defaults to false.                  |
| `target`  | string  | conditional | required when `global=false`; same regex/length policy as session names (`^[A-Za-z0-9_-]+$`, len 1-64).         |
| Field    | Type   | Required | Notes                                                                              |
| -------- | ------ | -------- | ---------------------------------------------------------------------------------- |
| `target` | string | yes      | window in `<session>:<window>` form; session 1-64 `^[A-Za-z0-9_-]+$`, window may be a name (same regex) or numeric index (`\d+`). |

The schema sets `additionalProperties: false`, so any unknown field is
rejected by spec-compliant clients before tmux is consulted.

### Output

JSON text block:
## `lock_client`

Lock a single attached tmux client via `tmux lock-client [-t <client>]`.
Distinct from a session-scoped lock (which would target every client
attached to a named session): this tool either targets one specific
attached client by its TTY-path name (the value `list_clients` reports
as `tty`, e.g. `/dev/pts/0`) or, with `client` omitted, asks tmux to
lock the caller's current client. This is a **mutating** tool — the
lock screen replaces the live session view on the targeted terminal —
so a `-read-only` deployment rejects it with `-32011`
(`errs.CodeReadOnly`) before the handler runs.

### Input

| Field    | Type   | Required | Notes                                                                                                                                                |
| -------- | ------ | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| `client` | string | no       | tmux client name (the path-like key shown in `list_clients`, e.g. `/dev/pts/0`); regex `^/[A-Za-z0-9_./:-]+$`, len 1-256. Omit to lock the current client. |

The schema sets `additionalProperties: false`, so any field other than
`client` is rejected with `-32602` (invalid params) before tmux is
consulted — a typo like `"clinet"` fails fast instead of silently
behaving like the unscoped variant.

### Output

JSON text block with a flat object keyed by `locked`:

```jsonc
{ "locked": true }
```

The ack is identical whether tmux iterated zero or many attached
clients because tmux's `lock-server` itself does not distinguish the
two — and surfacing that detail would push every caller to write a
"how many clients did we lock?" branch they do not need.

### Errors

| Code     | Cause                                                                                                              |
| -------- | ------------------------------------------------------------------------------------------------------------------ |
| `-32602` | An unknown field was supplied on `arguments` (the schema is closed).                                               |
| `-32000` | No daemon is running on this controller's socket (`errs.ErrSessionNotFound`). Reuses the standard "named target does not exist" code shared with `lock_session` / `lock_client` / `list_clients` / `session_kill`. |
| `-32603` | tmux refused the lock for an unexpected reason.                                                                    |
{
  "pasted": true
}
```

The pasted bytes themselves do not appear in the response — they land
in the pane's pty and are observable via `capture` once the receiving
process has rendered them.

```jsonc
{
  "sourced": true
}
```

The reload is observable via `show_options` — for instance, sourcing a
`tmux.conf` that sets `set -g escape-time 17` flips the server-wide
`escape-time` value to `"17"` immediately, so an agent that wants to
confirm the reload took effect can chain into `show_options`.

```jsonc
{
  "set":    true,
  "unset":  false,    // mirrors the input flag so a caller can branch on the resolved mode.
  "global": false,    // mirrors the input flag.
  "name":   "pane-died" // echoes the hook name back verbatim.
}
```

The ack shape is identical on the bind and unset paths — only the
`unset` flag distinguishes the two. Re-running the unset path against
an already-cleared hook is a no-op (the wrapper preserves tmux's
idempotent `-u` semantics) so deployment scripts can teardown
unconditionally.

```jsonc
{ "cycled": true }
```

`previous-layout` itself produces no useful stdout; a follow-up
`display_message` against `#{window_layout}` is one call away if the
caller wants to confirm the actual dump that landed.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | `target` missing / outside the pane-target regex; `buffer_name` outside the regex/length policy; or an unknown field on `arguments`. |
| `-32000` | The named buffer (or the targeted session/pane) does not exist on this server (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the paste for an unexpected reason.                   |
| `-32602` | Missing/invalid `target` (empty, no `:`, bad regex on either half). |
| `-32000` | The targeted session/window does not exist (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the cycle for an unexpected reason.                   |

### Example

```jsonc
{}
```

Pairs naturally with a long-running multi-session deployment: when an
operator hands the server back to a human and wants every attached
terminal to require authentication before resuming work, `lock_server`
is the single call that does it without disturbing any running
process.
{
  "set":     true,
  "name":    "FOO",      // echoed back verbatim from the input.
  "removed": false       // true when `value` was omitted (the `-u NAME` path), false on a set.
}
```

---

## `if_shell`

Conditional dispatch via
`tmux if-shell [-bF] SHELL_COMMAND TMUX_COMMAND [ELSE_TMUX_COMMAND]`.
tmux runs `shell_command` through `/bin/sh -c` (or evaluates it as a
`#{format}` expression when `format_expand=true`). On success — exit
code 0, or a non-empty / non-zero / non-`"0"` expansion — tmux
dispatches `then_command`. On failure (any other exit code, or an
empty/zero expansion) tmux dispatches `else_command` when set;
otherwise the failure branch is a no-op. The canonical agent pattern
is "if a process is running, do X; else Y" — e.g.
`pgrep -x build-watch && wc -l build.log` deciding between
`display-message running` and `display-message stopped`.

`if_shell` is the conditional sibling of `run_shell` (the unconditional
"run a /bin/sh command on the controller host" surface). Reach for
`run_shell` when you just need stdout; reach for `if_shell` when the
agent's next action depends on the shell command's exit code.

> **CAUTION** — `if_shell` runs `shell_command` on the controller host
> via `/bin/sh -c`. **The command itself is not sandboxed by this
> server**, so an agent that can call this tool can run arbitrary shell
> on the host. Operators must trust the agents reaching for `if_shell`
> — gate the surface away from untrusted clients with the `-allowlist`
> flag (see [`docs/flags.md`](flags.md)) or remove the tool from the
> registry entirely.
>
> Mutating in spirit (it spawns a shell pipeline AND dispatches a tmux
> command), so `if_shell` is **not** allowed under `-read-only`.

### Input

| Field           | Type    | Required | Default | Notes                                                                                                          |
| --------------- | ------- | -------- | ------- | -------------------------------------------------------------------------------------------------------------- |
| `shell_command` | string  | yes      | —       | Shell pipeline tmux runs via `/bin/sh -c` (or a `#{format}` expression when `format_expand=true`).             |
| `then_command`  | string  | yes      | —       | tmux command line dispatched on success (exit 0 / non-empty expansion).                                        |
| `else_command`  | string  | no       | `""`    | tmux command line dispatched on failure. Omit / empty → no-op on the failure branch.                          |
| `background`    | boolean | no       | `false` | When true, runs `shell_command` detached (`-b`); the call returns immediately and the branch fires later.       |
| `format_expand` | boolean | no       | `false` | When true, treat `shell_command` as a tmux `#{format}` expression (`-F`) instead of running it through /bin/sh. |

All three command strings are bounded at 4096 bytes; NUL bytes and
other ASCII control characters (newline, ESC, DEL, …) are rejected up
front. Tab (0x09) is allowed for spacing.

### Output

JSON block: `{"dispatched": true}`. The boundary deliberately does not
echo the resolved argv because tmux gives no useful confirmation back —
a follow-up `display_message` / `capture` against the targeted session
is the natural way to confirm the chosen branch ran.

### Errors

| Code     | Cause                                                                                                            |
| -------- | ---------------------------------------------------------------------------------------------------------------- |
| `-32602` | `name` missing or outside the regex/length policy; `scope=session` without `session`; unknown `scope`; unknown field on `arguments`. |
| `-32000` | `scope=session` and the named session does not exist on this server (`errs.ErrSessionNotFound`).                 |
| `-32603` | tmux refused the `set-environment` for any other reason.                                                         |
| Field    | Type    | Notes                                                                          |
| -------- | ------- | ------------------------------------------------------------------------------ |
| `locked` | boolean | Always `true` on success. The shape leaves room for future extensions without breaking callers that read only the boolean. |

A headless server with nothing attached returns `{"locked": true}` —
a clean success rather than an error — so callers can fire-and-forget
a lock without first running `list_clients` to know whether there is
anything to lock. The boundary swallows tmux's `no current client`
stderr in this case; any other tmux failure surfaces as `-32603`.

### Errors

| Code     | Cause                                                                                            |
| -------- | ------------------------------------------------------------------------------------------------ |
| `-32602` | Malformed args (bad regex / over the length cap on `client`) or an unknown field on the schema.  |
| `-32000` | `client` named a terminal that is not currently attached (`errs.ErrSessionNotFound`).            |
| `-32011` | Server is running in `-read-only` mode (this tool mutates client state).                         |
| `-32603` | tmux failed for an unexpected reason (server crashed, IO error).                                 |
| `-32602` | `path` missing, longer than 4096 bytes, relative, contains a control character / NUL byte, contains a `..` segment, or an unknown field on `arguments`. |
| `-32000` | `path` does not exist on the filesystem (`errs.ErrSessionNotFound`); only when `quiet=false`. With `quiet=true` tmux silently swallows the missing-file case. |
| `-32603` | tmux's source-file failed for an unexpected reason (rare; typically a fork/exec error or syntactically-broken config under quiet=false). |
| `-32602` | `name` missing / outside regex/length policy; `command` missing on bind path or > 4 KiB / contains NUL or other control bytes; `target` missing on the per-session bind path or outside the session-name policy; unknown field on `arguments`. |
| `-32000` | Target session does not exist (`errs.ErrSessionNotFound`); also covers tmux's "no such window" / "invalid option" stderr shapes for an unknown hook name on the unset path. |
| `-32603` | tmux refused the set-hook for an unexpected reason (rare; typically a fork/exec error). |

### Examples

```jsonc
// Set a per-session variable. Future panes spawned inside `mysession`
// inherit it; existing panes keep their current environment.
{ "name": "FOO", "value": "bar", "scope": "session", "session": "mysession" }

// scope is optional — the documented default is "session".
{ "name": "FOO", "value": "bar", "session": "mysession" }

// Set to empty string explicitly. Distinct from omitting `value`,
// which would remove the variable instead.
{ "name": "FOO", "value": "", "session": "mysession" }

// Remove a per-session variable (`tmux set-environment -t mysession -u FOO`).
{ "name": "FOO", "scope": "session", "session": "mysession" }

// Set a server-wide variable that subsequently created sessions
// inherit. `session` is ignored for scope=global.
{ "name": "PATH", "value": "/usr/local/bin:/usr/bin", "scope": "global" }

// Remove a global variable (`tmux set-environment -g -u PATH`).
{ "name": "PATH", "scope": "global" }
```

`set_environment` is a mutating tool and is therefore rejected when
the server is armed with `-read-only` (CodeReadOnly, `-32011`). Use
the read-only tool surface (`show_options`, `capture`, `session_list`,
`list_buffers`, …) for inspection-only access.
// Stash a snippet, then paste it into the session's active pane.
{ "name": "set_buffer",
  "arguments": { "data": "echo HELLO_FROM_PASTE\n", "name": "snippet" } }
{ "name": "paste_buffer",
  "arguments": { "target": "demo:0.0", "buffer_name": "snippet" } }

// Default-name path: paste the most-recently-added buffer.
{ "name": "paste_buffer",
  "arguments": { "target": "demo:0.0" } }

// One-shot snippet: paste then delete so the buffer does not linger.
{ "name": "paste_buffer",
  "arguments": { "target": "demo:0.0", "buffer_name": "snippet", "delete_after": true } }
// lock the caller's current client (no-op on a headless server)
{}

// lock a specific attached terminal by TTY path
{ "client": "/dev/pts/0" }
```

Pair with `list_clients` to discover an attached terminal's TTY
before locking it:

```jsonc
{ "name": "list_clients", "arguments": {} }
{ "name": "lock_client",  "arguments": { "client": "/dev/pts/0" } }
// Strict reload: fail loudly if the path is wrong.
{ "path": "/etc/tmux-mcp/tmux.conf" }

// Best-effort reload: ignore missing files / unknown options.
{ "path": "/home/agent/.tmux.conf", "quiet": true }
```

Pair with `show_options` to confirm the reload took effect:

```jsonc
{ "name": "source_file",  "arguments": { "path": "/etc/tmux-mcp/tmux.conf" } }
{ "name": "show_options", "arguments": { "scope": "server" } }
// Bind a hook on a single session.
{ "name": "pane-died", "command": "display-message \"a pane just died\"", "target": "demo" }

// Bind globally so every session inherits the hook.
{ "name": "client-attached", "command": "run-shell ./on-attach.sh", "global": true }

// Clear a hook a previous run installed.
{ "name": "pane-died", "target": "demo", "unset": true }

// Clear the global variant.
{ "name": "client-attached", "global": true, "unset": true }
```

A typical install-then-clear chain:

```jsonc
{ "name": "set_hook", "arguments": { "name": "pane-died", "command": "display-message x", "target": "demo" } }
{ "name": "set_hook", "arguments": { "name": "pane-died", "target": "demo", "unset": true } }
{ "target": "demo:0" }
```

A typical chain looks like: split the window to gain panes, anchor on
a known preset, then step backward to reshape.

```jsonc
{ "name": "pane_split",      "arguments": { "session": "demo", "direction": "vertical", "detach": true } }
{ "name": "select_layout",   "arguments": { "target": "demo:0", "layout": "tiled" } }
{ "name": "previous_layout", "arguments": { "target": "demo:0" } }
```


---

## `command_prompt`

Open the targeted client's interactive command-prompt UI via
`tmux command-prompt [-1iIN] [-p PROMPTS] [-I INPUTS] [-t TARGET] [TEMPLATE]`.
Useful for an agent that wants to programmatically launch a preset
prompt dialog (e.g. a rename-window flow whose template is
`rename-window %%`). On a headless server (no client attached, no
`client` pinned) the call is a successful no-op — the prompt has
nowhere to render — and the response still echoes back the caller's
arguments with `opened: true` so a chained workflow stays
deterministic.

### Input

| Field         | Type    | Required | Notes                                                                          |
| ------------- | ------- | -------- | ------------------------------------------------------------------------------ |
| `client`      | string  | no       | Optional target client TTY path (e.g. `/dev/pts/3`); maps to `-t TARGET`.       |
| `prompts`     | string  | no       | Optional comma-separated prompt strings (`-p PROMPTS`); one per `%%` placeholder. |
| `inputs`      | string  | no       | Optional comma-separated default inputs (`-I INPUTS`); aligned positionally with `prompts`. |
| `template`    | string  | no       | Optional tmux command tmux runs once the prompt is filled. `%%` → user input.   |
| `one_key`     | boolean | no       | When `true`, accept a single keypress without Enter (`-1`).                     |
| `incremental` | boolean | no       | When `true`, run the command on every keystroke (`-i`).                         |
| `multi_line`  | boolean | no       | When `true`, open the multi-line editor instead of single-line (`-N`). Rare.    |

Every field is optional — tmux itself accepts a bare `command-prompt`
invocation. In practice an agent will set at least one of `template` /
`one_key` / `incremental` / `multi_line` to make the call do useful
work; the schema does not enforce it. Each free-form string field is
capped at 4096 bytes, must be valid UTF-8, must not contain NUL bytes,
and must not contain control characters other than tab. Newlines /
carriage returns inside `template` / `prompts` / `inputs` are
explicitly rejected because tmux's command-prompt is single-shot per
call.
## `show_hooks`

Enumerate every hook binding the tmux server currently holds. Wraps
`tmux show-options -H` / `-wH` (the `-H` flag exposes hook entries
that are otherwise hidden from `show-options` output). Sister of
[`set_hook`](#set_hook), the mutating verb that installs / removes
the bindings this tool surfaces — pair them in a "ensure / inspect"
loop to confirm a hook landed where the agent expected it to.

When `target` is omitted, the response covers the **server-global**
hook tables (`-gH` for server/session-class events like
`client-attached`, `-gwH` for window-class events like `pane-died`)
**and** every existing session's hook tables (the controller iterates
the live session list and probes each one). When `target` names a
session, only that session's hook tables are scanned, so the response
is scoped to bindings the named session actually carries — useful for
"show me only what's on this tenant" inspections.

Read-only: this tool issues nothing but `show-options -H` invocations
under the hood. It is allowed under `-read-only`.

### Input

| Field    | Type   | Required | Notes                                                                                                                                                                |
| -------- | ------ | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `target` | string | no       | optional session name; same regex/length policy as session names (`^[A-Za-z0-9_-]+$`, len 1-64). When set, scope the scan to this session. When omitted, scan everything. |

### Output

JSON text block:

```jsonc
{
  "opened":      true,
  "client":      "",
  "prompts":     "name:",
  "inputs":      "",
  "template":    "rename-window %%",
  "one_key":     false,
  "incremental": false,
  "multi_line":  false
}
```

`opened` is `true` whenever the controller succeeded — that covers
both the "actually rendered to a client" branch and the headless
no-op. The remaining fields echo the caller's logical arguments back
so a chained workflow can correlate the response with the original
invocation.
  "hooks": [
    {
      "name":    "pane-died",                  // hook event (no [idx] suffix; multi-binding hooks fan out into multiple rows).
      "command": "display-message \"x\"",      // tmux command bound to the event, verbatim from show-options.
      "target":  ""                             // empty for server-global bindings; the session name for per-session bindings.
    },
    {
      "name":    "alert-bell",
      "command": "display-message \"bell\"",
      "target":  "demo"
    }
  ]
}
```

`hooks` is **always** a non-nil array — an empty server (no hooks set
yet) returns `{"hooks": []}` rather than `null`, so a caller iterating
the array never has to special-case the "no bindings" path.

When tmux normalises a command's quoting on store (e.g. it strips
non-essential surrounding quotes around a single arg without
whitespace), the `command` field reflects the **stored** form, not
the original input the caller passed to `set_hook`. Bodies with
embedded whitespace round-trip verbatim because tmux preserves the
surrounding quotes; bodies without whitespace may come back unquoted.

### Errors

| Code     | Cause                                                              |
| -------- | ------------------------------------------------------------------ |
| `-32602` | A free-form string field exceeded 4096 bytes, contained a control byte, was not UTF-8, or carried a newline. |
| `-32000` | An explicit `client` did not match any attached TTY (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the call for any other reason.                        |

### Example

```jsonc
// Preset rename-window dialog. Headless server: returns a successful
// no-op; attached client: opens the prompt with "name: " label and
// the current window name as the default input.
{
  "template": "rename-window %%",
  "prompts":  "name:",
  "inputs":   ""
}
```

`command_prompt` is **not** in the read-only allowlist: even a
template like `rename-window %%` mutates server state once the user
fills the prompt, and a more aggressive template (`kill-server`) would
mutate it directly. Operators running with `-read-only` will see
`-32011` (`CodeReadOnly`) for any `command_prompt` call.
| `-32602` | Missing/empty `shell_command` or `then_command`, or any of the three commands violating the length / control-char policy. |
| `-32603` | tmux refused the dispatch for any other reason — typically a syntax error or unknown command in `then_command` / `else_command`. |

`if_shell` does not take `-t`, so there is no `-32000`
`session_not_found` mapping here. A bad target inside
`then_command` / `else_command` surfaces as the generic tmux error
text wrapped in `-32603`.

### Examples

If a build watcher is running, log a "still up" message; otherwise log
"stopped":

```jsonc
{
  "shell_command": "pgrep -x build-watch >/dev/null",
  "then_command":  "display-message 'build-watch up'",
  "else_command":  "display-message 'build-watch stopped'"
}
```

Use a `#{format}` expression (no fork+exec) to branch on the active
session name:

```jsonc
{
  "shell_command": "#{==:#{session_name},build}",
  "then_command":  "display-message 'on build session'",
  "else_command":  "display-message 'on a different session'",
  "format_expand": true
}
```

Fire-and-forget (the call returns before the shell command exits):

```jsonc
{
  "shell_command": "sleep 5; pgrep -x my-daemon",
  "then_command":  "display-message 'daemon recovered'",
  "background":    true
}
```
| `-32602` | `target` outside the regex/length policy, or an unknown field on `arguments`. |
| `-32000` | `target` names a session that does not exist on this server (`errs.ErrSessionNotFound`). |
| `-32603` | tmux refused the show for an unexpected reason (rare; typically a fork/exec error). |

### Examples

```jsonc
// List every binding the server currently holds (global + every session).
{ "name": "show_hooks", "arguments": {} }

// Scope the scan to a single session.
{ "name": "show_hooks", "arguments": { "target": "demo" } }
```

A typical install-then-verify chain pairs with
[`set_hook`](#set_hook):

```jsonc
{ "name": "set_hook",  "arguments": { "name": "pane-died", "command": "display-message \"x\"", "target": "demo" } }
{ "name": "show_hooks", "arguments": { "target": "demo" } }
```

