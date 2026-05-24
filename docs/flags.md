# CLI flags & environment variables

Every flag the `tmux-mcp` binary accepts, together with its environment
variable equivalent (when one exists). Flags take precedence over env
vars; pass `tmux-mcp -help` to print the canonical usage block.

## Flags

| Flag                       | Default                         | Env var equivalent  | Description                                                                                                       |
| -------------------------- | ------------------------------- | ------------------- | ----------------------------------------------------------------------------------------------------------------- |
| `-help`                    | —                               | —                   | Print the usage block (the `Flags:` table from `cmd/tmux-mcp/main.go`) and exit 0.                                |
| `-version`                 | —                               | —                   | Print `tmux-mcp <version>` and exit 0. Version is set via `-ldflags="-X main.version=…"` and falls back to `dev`. |
| `-version-json`            | —                               | —                   | Print machine-readable build metadata (`{version, go, commit, date}`) on stdout and exit 0.                       |
| `-probe`                   | —                               | —                   | Run a startup health check (verifies tmux on `$PATH` and version floor) and exit. See **Probe** below.            |
| `-dry-run`                 | —                               | —                   | Perform the full startup (flag parsing, tmux controller init, audit sink open, tool-surface build) then exit before reading stdin. Prints `dry-run ok\ttmux=<v>\ttmux-mcp=<v>` on success. See **Dry run** below.    |
| `-log-level`               | `info`                          | —                   | Log verbosity. One of `error`, `warn`, `info`, `debug`. Logs go to stderr; stdout stays JSON-RPC.                 |
| `-log-format`              | `text` (auto `json` at `debug`) | —                   | slog output format: `text` or `json`. Unset + `-log-level=debug` auto-promotes to `json`; passing the flag pins the chosen value. |
| `-log-source`              | `false`                         | —                   | Include file/line of the call site in each log record (slight perf cost). JSON records gain a `source` object, text records a `source=…` key. |
| `-log-output`              | `stderr`                        | —                   | Destination for slog output: `stderr` (default), `stdout` (DANGER — corrupts JSON-RPC frames if combined with serving), or a file path (opened append-only at mode `0600`). The file is closed cleanly on shutdown. By default tmux-mcp does not rotate the file — pair it with `logrotate(8)` or pass `-log-rotate-size`. |
| `-log-rotate-size`         | `0` (disabled)                  | —                   | Enable size-based rotation on `-log-output`. When the next Write would push the file past N bytes, tmux-mcp renames the live file to `<path>.<unix-ns>` and reopens a fresh `<path>` in place. Counted in bytes — e.g. `10485760` for 10MB. `0` preserves the legacy "open once, never rotate" behaviour for deployments paired with `logrotate(8)`. |
| `-log-rotate-keep`         | `5`                             | —                   | Maximum count of rotated archive files retained on disk alongside `-log-output`. After a rollover, the oldest archives (by mtime) are deleted once the count exceeds K. Ignored when `-log-rotate-size=0`. |
| `-socket`                  | fresh tempdir under `$TMPDIR`   | `TMUX_MCP_SOCKET`   | Absolute path for the private tmux socket. Parent directory must already exist. Flag wins over env var.           |
| `-tmux-bin`                | `""` (resolve `tmux` from `$PATH`) | `TMUX_MCP_TMUX_BIN` | Absolute path to the tmux executable. Empty default keeps the legacy PATH lookup. When set the path must be absolute and point at an existing executable file; startup fails with `tmux binary "<path>" not executable: …` otherwise. Flag wins over env var. See **`-tmux-bin`** below. |
| `-tmux-config-path`        | `""` (tmux's built-in defaults)    | `TMUX_MCP_TMUX_CONFIG_PATH` | Absolute path to a `tmux.conf` file passed to every tmux invocation via `-f <path>`, so options the file declares (e.g. `set -g escape-time 17`) take effect for every session this server creates. Empty default keeps the legacy "tmux uses its built-in defaults plus `~/.tmux.conf`" behaviour. When set the path must be absolute and point at an existing regular file; startup fails with `tmux config path "<path>" …` otherwise — a single clean diagnostic instead of poisoning every later tmux call. Flag wins over env var. See **`-tmux-config-path`** below. |
| `-max-concurrent-calls`    | `64`                            | —                   | Cap simultaneously-executing `tools/call` frames. Excess callers wait (back-pressure rather than failure). `0` disables the cap (unbounded goroutines). |
| `-max-response-bytes`      | `0` (disabled)                  | —                   | Hard ceiling on the marshalled JSON-RPC response body in bytes. When a reply exceeds the cap, the server replaces it with a typed JSON-RPC error (code `-32010`) so a misbehaving tool (e.g. `capture_pane` on a 10MB scrollback) cannot dump a multi-megabyte frame onto an MCP client whose reader can't tolerate it. Clients see the error, not a truncated payload. The audit + metrics records still fire with the oversize sentinel. `0` disables the ceiling (the historical behaviour). |
| `-audit-log`               | disabled                        | —                   | Path for JSONL audit records. `stderr` shares the slog stream; any other value is opened append-only at mode 0600. Records carry `args_size_bytes` only — never argument *content*. |
| `-snapshot-ttl`            | `1h`                            | —                   | Maximum idle time a session's snapshot history may sit in memory before it is pruned. `0` disables cleanup (history released only when the session is killed). Accepts any Go duration: `30s`, `5m`, `2h`. |
| `-shutdown-timeout`        | `5s`                            | —                   | On `SIGTERM`/`SIGINT`, wait up to this duration for in-flight `tools/call` handlers to finish writing their JSON-RPC responses before exiting. `0` disables the drain (immediate exit). On timeout the binary exits non-zero so supervisors can flag a forced shutdown. |
| `-session-idle-timeout`    | `0` (disabled)                  | —                   | Auto-kill any session that has had no `tools/call` activity for at least this duration. Activity is any `tools/call` referencing the session by name; `session_list` and `kill_all_sessions` are explicitly excluded. Negative values are rejected at startup (exit 2). |
| `-allowlist`               | `""` (no filter)                | —                   | Comma-separated tool names. When set, only those names appear in `tools/list` and are dispatchable via `tools/call`; every other tool is rejected with `-32601` (methodNotFound). Unknown names abort startup with `unknown tools in -allowlist: …`. Useful for least-privilege deployments — see **`-allowlist`** below. |
| `-session-prefix`          | `""` (no prefix)                | —                   | When set, every session this server creates lands on tmux as `<prefix><name>`, and every other session-bearing tool resolves the bare name back transparently. `session_list` / `kill_all_sessions` are scoped to the prefix and strip it from the response so co-tenant agents stay invisible. Must match `[A-Za-z0-9_-]+`, may not end with `-`, and must leave room for at least one byte of session name (combined length ≤ 64). See **`-session-prefix`** below. |
| `-read-only`               | `false`                         | —                   | Reject every `tools/call` whose tool would mutate tmux state. Only inspection tools (`capture`, `wait_for_text`, `session_list`, `list_panes`, `list_windows`, `list_clients`, `list_keys`, `display_message`, `session_describe`, `session_inspect`, plus the spec-named aliases `capture_pane` / `list_sessions` / `list_buffers` / `show_buffer` / `show_options` / `show_message`) dispatch; everything else is rejected with a typed JSON-RPC error (code `-32011`, message `tool 'X' is rejected: server in read-only mode`). `tools/list` still returns the full surface so a constrained agent can enumerate it. See **`-read-only`** below. |

## `-version-json` output

Stable, lowercase JSON — safe to consume from CI / dashboards:

```json
{ "version": "v0.4.0", "go": "go1.24.1", "commit": "abc1234", "date": "2026-01-15T12:34:56Z" }
```

`commit` and `date` come from `runtime/debug.ReadBuildInfo` (populated
when the binary is built with `-buildvcs=true`, which is the default).
They are empty strings on builds where VCS info was stripped.

## `-probe` semantics

`tmux-mcp -probe` exists for orchestrators that just want to confirm
the binary is functional (k8s liveness, systemd `ExecStartPre=`,
Docker `HEALTHCHECK`):

- Looks up `tmux` on `$PATH`, runs `tmux -V`, checks the version floor.
- On success: writes one line to stdout — `ok\ttmux=<v>\ttmux-mcp=<v>` — and exits 0.
- On failure: writes a `probe failed: …` diagnostic to stderr, leaves
  stdout untouched, and exits non-zero. Parsers can therefore rely on
  stdout being either empty or a valid `ok\t…` line.
- Bounded by an internal 5s timeout so a wedged binary on a misconfigured
  PATH cannot hang the liveness check forever.

## `-dry-run` semantics

`tmux-mcp -dry-run` is a strictly stronger probe than `-probe`: it
runs every bootstrap side-effect short of reading stdin so you can
catch flag/path/tmux-version errors before swapping a binary into a
live agent.

Steps the dry run executes (in order):

1. Parse flags, validate `-log-format` / `-session-idle-timeout`.
2. Open the slog destination (`-log-output`) — fails if a file path
   is supplied and the path is not writable at mode `0600`.
3. Initialise the tmux controller (`tmuxctl.NewWithSocket`) — fails if
   `tmux` is missing on `$PATH`, the version is below the floor, or the
   `-socket` parent directory is wrong.
4. Open the audit sink (`-audit-log`) — fails if the path is not
   writable at mode `0600`.
5. Build the in-memory tool surface (`server.NewTools` + options) and
   apply `-allowlist` if set — fails if any name in the list is not a
   registered tool.
6. Print `dry-run ok\ttmux=<tmux-ver>\ttmux-mcp=<binary-ver>` to stdout
   and exit 0.

On any failure the diagnostic goes to stderr (no stdout output) and
the process exits non-zero. The signature distinguishes a dry run
from a normal probe so an orchestrator can tell whether the heavier
checks ran.

Use it in CI smoke tests, systemd `ExecStartPre=`, or before swapping
a Claude Desktop config to a new socket path / audit log location.

## `-socket` rules

- The path **must be absolute**. Relative paths are rejected up front
  with a clear error.
- The **parent directory must already exist** — `tmux-mcp` will not
  create `/run/tmux-mcp` for you. That keeps a typo from silently
  writing to the wrong place. Use `RuntimeDirectory=` (systemd) or
  `RUN mkdir` (Dockerfile) to provision it.
- On shutdown the socket file is removed but the parent directory is
  left intact, so unit restarts stay idempotent.
- When neither the flag nor the env var is set, the legacy behaviour
  applies: a fresh tempdir under `$TMPDIR` holds the socket.

## `-tmux-bin`

- Empty default keeps the legacy behaviour: tmux-mcp resolves `tmux`
  via `exec.LookPath` and uses whatever the deployment's `$PATH`
  points at. Existing deployments see no change.
- When set, the value must be an **absolute path** and must point at
  an **existing executable file**. Validation runs at startup so a
  bogus path surfaces a single clean diagnostic
  (`tmux binary "<path>" not executable: …`) before any tmux command
  is dispatched, rather than an obscure `fork/exec` failure once the
  JSON-RPC loop is already serving requests.
- The same validated path is used everywhere the binary is exec'd:
  the controller's tmux invocations, the `-probe` health check, the
  `-dry-run` bootstrap, and the `/healthz` background probe. So
  `-probe` reflects exactly the binary the runtime would otherwise
  drive.
- The version floor (`tmux 3.0+`) applies to the override too — a
  pinned tmux that's older than the floor is rejected with the same
  upgrade-hint diagnostic the default path emits.
- Useful for:
  - **Nix / Homebrew**: pin a specific tmux store path so the binary
    can't drift under tmux-mcp when the system PATH shifts.
  - **Containers**: select between multiple tmux versions installed
    side-by-side without rewriting PATH.
  - **Sandboxes / static builds**: point at a vendored tmux that
    lives outside any standard search path.
  - **Testing**: drive integration tests against a known-good tmux
    version regardless of what's on the developer's PATH.

## `-tmux-config-path`

- Empty default keeps the legacy behaviour: the controller invokes
  tmux without `-f`, so tmux uses its built-in defaults plus the
  user's `~/.tmux.conf`. Existing deployments see no change.
- When set, the controller injects `-f <path>` into every tmux
  invocation (server-flag position, before the subcommand verb), so
  every session this server creates loads the supplied `tmux.conf`.
  Options the file declares (`set -g escape-time 17`,
  `set -g history-limit 50000`, …) take effect for every session
  without bleeding into the user's interactive shell config.
- The path must be an **absolute path** and must resolve to an
  **existing regular file** (not a directory or other irregular
  type). Validation runs at server start so a misconfiguration
  surfaces a single clean diagnostic
  (`tmux config path "<path>" …`) before any tmux command is
  dispatched — without it, every later run would fail with the same
  error per session.
- Useful for:
  - **Agent-friendly defaults**: ship a `tmux.conf` alongside the
    binary that lowers `escape-time`, raises `history-limit`,
    disables status-line redraws agents don't need, etc.
  - **Vendored / sandboxed deployments**: keep the agent's tmux
    behaviour reproducible regardless of which user account runs
    tmux-mcp (no surprise overrides from a host's `~/.tmux.conf`).
  - **Multi-tenant hosts**: run several tmux-mcp instances with
    different option sets on the same machine without forking the
    user's tmux config.
  - **Testing**: drive integration tests against a known-good
    `tmux.conf` regardless of what the developer has at `~`.

## `-log-format` & `-log-source`

- Default is `text`; passing `-log-format=json` switches to a
  newline-delimited JSON handler suitable for log aggregators.
- When `-log-format` is **not** passed and `-log-level=debug`, the
  server auto-promotes to `json` so structured fields stay
  machine-readable during deep debugging. Pass `-log-format=text`
  explicitly to override that auto-switch.
- `-log-source` is off by default — `slog`'s `AddSource` walks
  `runtime.Callers` on every record, so leaving it off keeps the
  zero-cost path. Enable it ad-hoc when you need to grep a log line
  back to the exact `slog.*` call that produced it.

## `-log-output`

- Default is `stderr`, which preserves the legacy behaviour of
  routing structured logs to the inherited stderr stream. Useful
  when the launcher (`systemd`, a container runtime, …) already
  captures stderr.
- `stdout` is honoured as a magic value for ad-hoc debugging in
  tandem with `-dry-run` / `-version`. **DANGER:** using it while
  the server is actually serving stdio interleaves slog records
  with JSON-RPC frames and corrupts the protocol — only useful for
  the one-shot paths.
- Any other value is treated as a filesystem path opened
  append-only with mode `0600` (same shape as `-audit-log`). The
  file is closed cleanly on shutdown so the last record is
  flushed.
- By default tmux-mcp does **not** rotate the log file. Pair it with
  `logrotate(8)` (or equivalent) on long-lived hosts, or pass
  `-log-rotate-size` for the in-process size-based rotator
  documented below.

## `-log-rotate-size` & `-log-rotate-keep`

- Default `-log-rotate-size=0` is **disabled** — the writer is a
  plain `*os.File` opened once and never rotated. Existing
  deployments paired with `logrotate(8)` see no behavioural drift.
- Pass `-log-rotate-size=N` (in bytes) to enable the in-process
  rotator. On every `Write`, the rotator checks whether `current
  size + len(p)` would exceed N and, if so, renames the live file
  to `<path>.<unix-ns>` and reopens a fresh `<path>` in place
  (mode `0600`, same as the live file). The size counter is held
  in-memory and seeded from `os.File.Stat()` at open time, so an
  `O_APPEND` reopen of an existing file resumes accounting from
  the right offset without paying for an `fstat(2)` per record.
- `-log-rotate-keep K` (default `5`) bounds the number of archive
  files retained on disk. After every rollover, the rotator
  enumerates `<path>.*` siblings, sorts them by `mtime`, and
  deletes the oldest until at most K archives remain. Pass `0` to
  retain every archive (useful for forensic / compliance reasons).
- The rotation runs **synchronously** inside `Write` — slog
  handlers do not buffer through goroutines, so every record
  arrives on the writer in order and the `<path>.<stamp>`
  archives are guaranteed to slot in between adjacent slog
  records, never mid-record.
- The rotator deliberately does *not* depend on a third-party
  package like `lumberjack`. Operators who need richer rotation
  semantics (compression, daily rotation, copytruncate, …)
  should still pair `-log-output` with `logrotate(8)` and leave
  `-log-rotate-size=0`.
- Examples: `tmux-mcp -log-output=/var/log/tmux-mcp/agent.log -log-rotate-size=10485760` (10MB cap, 5 archives kept), or `tmux-mcp -log-output=/var/log/tmux-mcp/agent.log -log-rotate-size=104857600 -log-rotate-keep=10` (100MB cap, 10 archives kept).

## `-max-concurrent-calls`

- 64 is a generous default for an interactive single-agent client
  (Claude Desktop typically runs 1–4 tools in parallel) while still
  putting a ceiling on goroutines a misbehaving / flooding client can
  spawn.
- Excess callers **wait** rather than fail — the limiter is a
  back-pressure semaphore, not an admission gate, so latency degrades
  gracefully under bursts instead of returning errors.
- Pass `0` to disable the cap entirely (the original unbounded
  behaviour).

## `-max-response-bytes`

- Default `0` keeps the cap disabled and preserves the historical
  "stream whatever the handler produced" behaviour. Set to a positive
  byte count to enforce a hard ceiling on the marshalled JSON-RPC
  response body that crosses stdout.
- When the cap fires, the original payload is **not** sent. The server
  synthesises a typed JSON-RPC error in its place: code `-32010`
  (`CodeOversizedResponse`), message
  `response body N bytes exceeds max-response-bytes M`. Clients see a
  structured error and can decide how to recover — there is no
  truncated frame to misparse.
- The underlying `tools/call` still ran. Its audit record and Prometheus
  metric fire with the oversize sentinel as the error code, so operators
  can distinguish "the tool failed" from "the answer was too big" in
  log / dashboard queries.
- Notifications (no id) carry no response, oversize or otherwise; the
  cap is a no-op for them.
- Useful for protecting fragile MCP clients (or pipes / sockets with
  bounded buffers) from `capture_pane` on a 10MB scrollback or other
  pathologically large outputs. Pair with a generous
  `-max-concurrent-calls` to keep the cost-per-rejection bounded too.

## `-audit-log`

- Empty default keeps audit logging opt-in: existing deployments see
  no behaviour change.
- `stderr` is a magic value that shares the slog stream — handy for
  desktop debugging where you want everything on one fd.
- Any other value is treated as a filesystem path and opened
  append-only with mode `0600` so audit records do not leak through
  group-readable files.
- **Privacy:** every record carries `args_size_bytes` (the byte length
  of the raw arguments JSON) but **not** argument content. Commands
  and any embedded secrets stay out of the audit trail.

## `-snapshot-ttl`

- The snapshot store keeps the two most-recent captures per session so
  `snapshot_diff` can return only what changed. Long-lived sessions
  that go quiet would otherwise pin those captures in memory forever.
- The reaper runs at roughly the TTL cadence and drops history for any
  session that has not been captured against within the window.
- `0` disables the reaper — history is released only when the session
  is killed (the historical behaviour). Useful for tests that want
  deterministic memory.

## `-shutdown-timeout`

- 5s is long enough that an in-flight `tools/call` returning a
  capture-pane snapshot or a `wait_for_text` result has time to
  serialise its response, but short enough to never trip systemd's
  default `TimeoutStopSec=90s`.
- The drain begins on `SIGTERM`/`SIGINT`. New `tools/call` frames
  arriving during the drain are still served until the deadline; once
  the deadline expires, in-flight handlers are abandoned and the
  binary exits non-zero so supervisors can flag the forced shutdown.
- Set to `0` to disable the drain entirely (legacy behaviour for tests
  / scripts that don't care about losing the last response).

## `-session-idle-timeout`

- The reaper goroutine is only launched when the value is positive, so
  leaving the flag unset (or passing `0` explicitly) preserves the
  historical "tmux-mcp never kills a session for you" behaviour for
  desktop deployments.
- "Activity" is defined as any `tools/call` that references the
  session by name. Session-spanning calls (`session_list`,
  `kill_all_sessions`) are explicitly excluded so they cannot extend
  an idle session's lifetime.
- Reaped sessions go through the same kill path as `session_kill`, so
  snapshot history is dropped and any subscribed audit log records the
  reason.
- Strictly negative durations are rejected at startup with exit code
  2; `0` is the documented "disabled" value.

## `-allowlist`

- Empty default keeps every registered tool exposed (the original
  behaviour). Unrelated deployments see no behaviour change.
- A non-empty value is a comma-separated list of tool names; only
  those tools appear in `tools/list` and are accepted by
  `tools/call`. Calls for filtered tools return JSON-RPC error
  `-32601` (methodNotFound) with message
  `tool "<name>" is not in -allowlist`. Whitespace around individual
  names is trimmed, blank entries (e.g. from a trailing comma) are
  skipped, and duplicates collapse to one entry.
- Unknown names are validated against the **live** tool registry at
  startup, so future tools added to the binary are pickable up by
  name without changing this validator. A typo aborts the binary
  with `unknown tools in -allowlist: <names>` before stdin is
  consumed, so a misconfigured unit file cannot silently disable
  tools the operator expected to expose.
- Enforcement runs ahead of dispatch — a client that calls
  `tools/call` without first enumerating `tools/list` cannot bypass
  the filter.
- Examples:
  - Read-only inspector: `-allowlist=capture,wait_for_text,wait_for_stable,snapshot_diff,session_list,session_describe,session_inspect,list_panes,list_windows`
  - Block destructive tools (everything except
    `kill_all_sessions`, `pane_kill`, `session_kill`, `send_signal`):
    pass an explicit allowlist of the tools you do want — there is
    no `-denylist` flag.

## `-session-prefix`

- Empty default keeps the prefix feature opt-in: the original
  single-tenant behaviour is preserved, and existing deployments see
  no behaviour change.
- A non-empty value namespaces every session this server creates so
  multiple tmux-mcp instances can safely share one tmux server (e.g. a
  shared dev container with one agent per developer). `session_create`
  with `name=demo` and `-session-prefix=agent_alice_` lands on tmux as
  `agent_alice_demo`. The reverse direction is transparent: `capture`,
  `send_keys`, `session_kill`, `session_describe`, `session_inspect`,
  `session_rename`, `wait_for_*`, `snapshot_diff`, `resize`,
  `send_signal`, `pane_*`, `window_*`, and `clear_history` all accept
  the bare logical name and forward to the prefixed identity.
- `session_list` returns only sessions inside this prefix (with the
  prefix stripped), and `kill_all_sessions` kills only those — a
  co-tenant agent's sessions (different prefix or none) are left
  running.
- **Validation rules** (enforced at startup; failure exits 2):
  - Must match the regex `[A-Za-z0-9_-]+` — no whitespace, colons,
    dots, slashes, or shell metacharacters that would let a hostile
    name break out of the prefix into a sibling session.
  - May **not** end with `-` (regex-legal but creates surprising
    names like `agent--build` when the user-supplied name itself
    starts with a dash); the idiomatic separator is `_`.
  - Must leave room for at least one byte of session name within the
    64-byte tmux session-name budget — i.e. the prefix length must be
    ≤ 63.
  - At runtime, `session_create` rejects a `prefix + name`
    combination that would overflow 64 bytes with `-32602`
    (invalid params) so the JSON-RPC client gets a clean error rather
    than a tmux session that no other tool can reference.
- The pane-target shapes (`session`, `session:window`,
  `session:window.pane`) are rewritten so only the session half picks
  up the prefix; tmux pane-id strings (`%5`) carry no session
  reference and pass through unchanged.
- The `window_move` `src`/`dst` arguments (always
  `<session>:<window>` or `<session>:`) get the prefix on the session
  half too.
- The `[IdleReaper]` activity table is keyed on the tmux-real
  (prefixed) name, so a session reaped after the configured
  `-session-idle-timeout` reaches the same session the controller
  drives; a deployment with `-session-prefix=` and
  `-session-idle-timeout=…` works correctly out of the box.

Examples:

```sh
# alice and bob share one tmux server, each with their own namespace
tmux-mcp -session-prefix=agent_alice_   # agent A
tmux-mcp -session-prefix=agent_bob_     # agent B (separate process)

# combine with -allowlist for a least-privilege namespaced deployment
tmux-mcp -session-prefix=intake_ -allowlist=capture,wait_for_text,session_list
```

## `-read-only`

- Default `false` keeps the dispatcher byte-identical to the pre-flag
  wire response: every registered tool dispatches normally.
  Unrelated deployments see no behaviour change.
- When set, the dispatcher rejects every `tools/call` whose tool name
  is not in the inspection allowlist (see
  `internal/server/readonly.go`) with a typed JSON-RPC error: code
  `-32011` (`CodeReadOnly`), message
  `tool 'X' is rejected: server in read-only mode`. The handler is
  never invoked — the rejection is synthesised by the dispatcher
  itself, so a misbehaving handler cannot bypass the gate.
- The allowlist covers both the tool names this server registers
  today and the spec-named aliases the read-only feature reserves for
  future renames / additions, so a tool registered later under one of
  the alias names is automatically inspection-allowed without a second
  policy edit:

  | Registered today   | Spec alias       |
  | ------------------ | ---------------- |
  | `capture`          | `capture_pane`   |
  | `wait_for_text`    | (same)           |
  | `session_list`     | `list_sessions`  |
  | `list_panes`       | (same)           |
  | `list_windows`     | (same)           |
  | `list_clients`     | (same)           |
  | `list_keys`        | (same)           |
  | (none)             | `list_buffers`   |
  | (none)             | `show_buffer`    |
  | (none)             | `show_options`   |
  | `display_message`  | `show_message`   |
  | `session_describe` | (same)           |
  | `session_inspect`  | (same)           |

- Anything not in the table — `send_keys`, `session_create`,
  `session_kill`, `kill_all_sessions`, `pane_*` (except listing),
  `window_*` (except listing), `clear_history`, `send_signal`,
  `session_rename`, `resize`, `wait_for_stable`, `snapshot_diff`,
  and any tool a future contributor adds without updating the
  allowlist — is rejected at dispatch time.
- `tools/list` is **not** gated. A read-only client can still
  enumerate the full surface and pick which inspection tools to
  invoke; only `tools/call` is constrained. This mirrors the MCP
  spec contract that surface enumeration is always allowed.
- Audit + metrics records fire on rejected calls exactly as they
  would for any other typed error, so operators see blocked attempts
  in their dashboards (a counter labelled `result="error"` plus an
  audit record carrying `error_code=-32011`). A read-only deployment
  is never silently permissive — every blocked attempt is observable.
- Composes cleanly with `-allowlist`: the allowlist runs ahead of
  the read-only gate (because the allowlist is checked inside the
  handler), so a tool that is filtered out by `-allowlist` still
  returns `-32601` (`methodNotFound`) instead of `-32011`. Operators
  who want both signals can combine the flags; operators who want a
  pure "diagnosis only" surface can pass `-read-only` alone and
  leave `-allowlist` empty.
- Useful as a safer default for novice agents (no risk of
  destructive side effects), production diagnostics where the LLM
  should only DIAGNOSE a session, or running multiple agents against
  the same session where exactly one is allowed to mutate.

```sh
# diagnostic-only agent: any send_keys / kill / pane mutate gets -32011
tmux-mcp -read-only

# pair with a session prefix so the diagnostic agent can only see its own
# sessions in addition to being barred from mutating them
tmux-mcp -read-only -session-prefix=ops_
```

## Environment variables

| Variable             | Used by      | Notes                                                         |
| -------------------- | ------------ | ------------------------------------------------------------- |
| `TMUX_MCP_SOCKET`    | `-socket`    | Absolute path. Loses to `-socket` when both are set.          |
| `TMUX_MCP_TMUX_BIN`  | `-tmux-bin`  | Absolute path to the tmux executable. Loses to `-tmux-bin` when both are set; empty value keeps the legacy `exec.LookPath("tmux")` behaviour. |
| `TMUX_MCP_TMUX_CONFIG_PATH` | `-tmux-config-path` | Absolute path to a `tmux.conf` file passed via `tmux -f`. Loses to `-tmux-config-path` when both are set; empty value keeps the legacy "no `-f` argument" behaviour. |
| `TMPDIR`             | (default)    | Used to derive the fallback socket directory when `-socket`/`TMUX_MCP_SOCKET` are unset. Inherited from the OS, not declared by tmux-mcp. |

## Examples

```sh
# print versions, machine-readable
tmux-mcp -version
tmux-mcp -version-json | jq

# debug logging while the agent talks to the server
tmux-mcp -log-level=debug
tmux-mcp -log-level=debug -log-format=text   # pin text even at debug
tmux-mcp -log-source                         # add file:line to every record
tmux-mcp -log-output=/var/log/tmux-mcp/agent.log  # redirect slog to a file

# in-process size-based rotation: 10MB cap, keep 5 archives (the default)
tmux-mcp -log-output=/var/log/tmux-mcp/agent.log -log-rotate-size=10485760

# 100MB cap, keep 10 archives (forensic / compliance retention)
tmux-mcp -log-output=/var/log/tmux-mcp/agent.log -log-rotate-size=104857600 -log-rotate-keep=10

# pin the socket for a systemd unit / container
tmux-mcp -socket=/run/tmux-mcp/sock
TMUX_MCP_SOCKET=/run/tmux-mcp/sock tmux-mcp

# pin a specific tmux binary (Nix / Homebrew / vendored static build)
tmux-mcp -tmux-bin=/nix/store/abcd-tmux-3.5a/bin/tmux
tmux-mcp -tmux-bin=/opt/homebrew/Cellar/tmux/3.5a/bin/tmux
TMUX_MCP_TMUX_BIN=/usr/local/bin/tmux tmux-mcp

# load a custom tmux.conf for every session this server creates
tmux-mcp -tmux-config-path=/etc/tmux-mcp/tmux.conf
TMUX_MCP_TMUX_CONFIG_PATH=/etc/tmux-mcp/tmux.conf tmux-mcp

# liveness probe
tmux-mcp -probe || echo "tmux missing or too old"

# dry run: parses flags, opens the audit log, builds the tool surface,
# prints "dry-run ok\t…" and exits without serving stdio
tmux-mcp -dry-run -socket=/run/tmux-mcp/sock -audit-log=/var/log/tmux-mcp/audit.jsonl

# audit log to a file (privacy: argument content is never logged)
tmux-mcp -audit-log=/var/log/tmux-mcp/audit.jsonl

# bound burst goroutines and reap idle sessions
tmux-mcp -max-concurrent-calls=32 -session-idle-timeout=30m

# graceful shutdown for systemd
tmux-mcp -shutdown-timeout=10s

# least-privilege: only expose read-only inspection tools
tmux-mcp -allowlist=capture,wait_for_text,snapshot_diff,session_list

# multi-agent isolation: each agent gets its own session namespace
tmux-mcp -session-prefix=agent_alice_
```
