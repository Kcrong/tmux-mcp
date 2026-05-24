# Tool reference

The MCP tool surface tmux-mcp exposes over `tools/list` / `tools/call`.
Schemas are the canonical source of truth and live in
[`internal/server/tools.go`](../internal/server/tools.go); this page is
the human-readable companion.

Right now the page documents only the per-tool details that don't fit
into the at-a-glance table in [`README.md`](../README.md). Additional
tool sections will be added here as their schemas become public.

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

## `send_signal`

Deliver a POSIX signal to the PID of the session's currently active
pane. More precise than `send_keys "C-c"` because the signal targets
the foreground program directly — it works even when the program has
stolen the keyboard (raw-mode TUIs, daemons that swallow `Ctrl-C`).

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
