# Tool reference

The MCP tool surface tmux-mcp exposes over `tools/list` / `tools/call`.
Schemas are the canonical source of truth and live in
[`internal/server/tools.go`](../internal/server/tools.go); this page is
the human-readable companion.

Right now the page documents only the per-tool details that don't fit
into the at-a-glance table in [`README.md`](../README.md). Additional
tool sections will be added here as their schemas become public.

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
