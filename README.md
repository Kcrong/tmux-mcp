# tmux-mcp

[![CI](https://github.com/Kcrong/tmux-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/Kcrong/tmux-mcp/actions/workflows/ci.yml)
[![CodeQL](https://github.com/Kcrong/tmux-mcp/actions/workflows/codeql.yml/badge.svg)](https://github.com/Kcrong/tmux-mcp/actions/workflows/codeql.yml)
[![lint-actions](https://github.com/Kcrong/tmux-mcp/actions/workflows/lint-actions.yml/badge.svg)](https://github.com/Kcrong/tmux-mcp/actions/workflows/lint-actions.yml)
[![Merge Queue: enabled](https://img.shields.io/badge/Merge%20Queue-enabled-2ea44f)](https://github.com/Kcrong/tmux-mcp/queue/main)
[![codecov](https://codecov.io/gh/Kcrong/tmux-mcp/branch/main/graph/badge.svg)](https://codecov.io/gh/Kcrong/tmux-mcp)
[![Go Reference](https://pkg.go.dev/badge/github.com/Kcrong/tmux-mcp.svg)](https://pkg.go.dev/github.com/Kcrong/tmux-mcp)
[![Go Report Card](https://goreportcard.com/badge/github.com/Kcrong/tmux-mcp)](https://goreportcard.com/report/github.com/Kcrong/tmux-mcp)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Latest Release](https://img.shields.io/github/v/release/Kcrong/tmux-mcp?display_name=tag&sort=semver)](https://github.com/Kcrong/tmux-mcp/releases/latest)

Coverage is enforced via [`.codecov.yml`](.codecov.yml): project coverage
must stay at or above 70%, and patch (new) code must hit 80%, both with a
1% drop tolerance.

A Model Context Protocol (MCP) stdio server that exposes a real `tmux`
session to an LLM agent so it can drive a terminal the way a human
does — typing into a real PTY, waiting for the screen to settle, reading
the visible pane, and reacting.

The goal is to keep the agent's experience as close to a human user's
as possible: it sees what you would see, types what you would type, and
waits the way you would wait.

---

## Quickstart (60 seconds)

```sh
# 1. install (Linux/macOS, amd64/arm64)
curl -fsSL https://github.com/Kcrong/tmux-mcp/releases/latest/download/tmux-mcp_$(uname -s)_$(uname -m).tar.gz \
  | sudo tar -xz -C /usr/local/bin tmux-mcp
tmux-mcp -version

# 2. wire it into your MCP client
cat > ~/.config/your-mcp-client/mcp.json <<'JSON'
{ "mcpServers": { "tmux": { "command": "/usr/local/bin/tmux-mcp" } } }
JSON

# 3. ask the agent to drive a terminal — it will call session_create,
#    send_keys, wait_for_stable, capture, …
```

If `tmux` is not installed, the server tells you exactly what to run
(`apt-get install tmux` / `brew install tmux`). For other ways to install,
see [Install](#install). For the full per-tool reference, see
[`docs/tools.md`](docs/tools.md); for every CLI flag and env var, see
[`docs/flags.md`](docs/flags.md).

### Log output format

Pass `-log-format=text|json` to pin the slog output shape (default
`text`; `-log-level=debug` auto-promotes to `json` only when the flag
is left unset, so log aggregators can opt in to JSON at any level).

Add `-log-source=true` to include the file/line of each `slog` call
site in every record — JSON records gain a structured `"source": {
"function": "...", "file": "...", "line": N }` object, text records
gain a `source=…` attribute. Off by default because `AddSource` walks
`runtime.Callers` on every record (small but real cost on a hot logging
path); flip it on for ad-hoc debugging when you need to grep a log line
back to the exact `slog.Info` call that produced it.

Pass `-log-output=PATH` to redirect the slog stream to a file instead
of stderr — useful when the launcher (Claude Desktop, an agent
framework, …) discards stderr and you still want a durable log. The
file is opened append-only at mode `0600` and closed cleanly on
shutdown. **DANGER:** `-log-output=stdout` is honoured for ad-hoc
debugging in tandem with `-dry-run` / `-version`, but using it while
the server is actually serving stdio interleaves slog records with
JSON-RPC frames and corrupts the protocol. tmux-mcp does not rotate
the log — pair the file with `logrotate(8)` on long-lived hosts.

| Flag             | Default  | Notes                                               |
| ---------------- | -------- | --------------------------------------------------- |
| `-log-level`     | `info`   | `error\|warn\|info\|debug`                          |
| `-log-format`    | `text`   | `text\|json`; `debug` auto-promotes to `json` when unset |
| `-log-source`    | `false`  | include `file:line` of each call site (slight perf cost) |
| `-log-output`    | `stderr` | `stderr` (default), `stdout` (DANGER — corrupts JSON-RPC frames if combined with serving), or a file path (append-only, mode `0600`) |

---

## Contents

- [Quickstart](#quickstart-60-seconds)
- [Log output format](#log-output-format)
- [Why tmux](#why-tmux)
- [Requirements](#requirements)
- [Install](#install)
- [Embed as a Go library](#embed-as-a-go-library)
- [Wire it up](#wire-it-up)
- [Deploy](#deploy)
- [Tool surface](#tool-surface)
- [End-to-end example](#end-to-end-example)
- [Patterns](#patterns)
- [Architecture](#architecture)
- [Design notes](#design-notes)
- [FAQ](#faq)
- [Troubleshooting](#troubleshooting)
- [Performance & tuning](#performance--tuning)
- [Metrics (Prometheus)](#metrics-prometheus)
- [Releases](#releases)
- [Verifying a release](#verifying-a-release)

---

## Why tmux

Raw PTY harnesses force you to maintain your own terminal emulator just
to answer the question "what is on screen right now?". `tmux` already
solves this with a stable CLI:

- `tmux send-keys` accepts both literal text and named keys
  (`Up`, `Enter`, `C-c`).
- `tmux capture-pane -p [-e]` prints the current pane — with ANSI
  sequences preserved on request.
- Sessions and windows are first-class, so several agents can share a
  host without stepping on each other.

## Requirements

- `tmux` 3.0+ on `$PATH`
- Linux or macOS
- Go 1.24+ (only when building from source)

> Windows binaries cross-compile and ship in releases, but `tmux` runs
> on Linux/macOS only — to actually use the server on Windows you need
> WSL or to ssh to a Linux/macOS host.

## Install

### Prebuilt binary

Pick the asset for your OS / architecture from the
[latest release](https://github.com/Kcrong/tmux-mcp/releases/latest)
(Linux, macOS, and Windows — `amd64` and `arm64`). Linux/macOS archives
ship as `.tar.gz`, Windows ships as `.zip`. Each archive contains a
single `tmux-mcp` binary — drop it on `$PATH`:

```sh
curl -fsSL https://github.com/Kcrong/tmux-mcp/releases/latest/download/tmux-mcp_$(uname -s)_$(uname -m).tar.gz \
  | tar -xz -C /usr/local/bin tmux-mcp
tmux-mcp -version
```

Windows binaries are provided for completeness (e.g. you build on
Windows but ssh to a Linux host, or you run via WSL), but the runtime
still requires `tmux`, which is Linux/macOS only — see
[Requirements](#requirements).

Releases are signed with checksums (`checksums.txt` next to the
archives) — see [Verifying a release](#verifying-a-release).

### With `go install`

```sh
go install github.com/Kcrong/tmux-mcp/cmd/tmux-mcp@latest
which tmux-mcp
```

Make sure `$(go env GOBIN)` (or `$GOPATH/bin`) is on `$PATH`, otherwise
your MCP client won't find the binary.

### From source

```sh
git clone https://github.com/Kcrong/tmux-mcp.git
cd tmux-mcp
make build              # produces ./tmux-mcp
./tmux-mcp -version     # smoke test — prints version and exits
```

For CI scripts, deployment dashboards, and supply-chain probes that
want machine-readable metadata, `-version-json` emits a single JSON
object (`version`, `go`, `commit`, `date`) on stdout and exits 0:
`./tmux-mcp -version-json | jq`.

Pass `-log-level=debug` for verbose JSON logs to stderr (stdout stays JSON-RPC).
Every flag and environment variable the binary accepts is catalogued in
[`docs/flags.md`](docs/flags.md).

`make help` lists every available target.

### Container image

Multi-arch (`linux/amd64` + `linux/arm64`) images are published to
[GitHub Container Registry](https://github.com/Kcrong/tmux-mcp/pkgs/container/tmux-mcp)
on every release. The image is based on `alpine` and bundles `tmux`, so
nothing else needs to be installed on the host:

```sh
docker pull ghcr.io/kcrong/tmux-mcp:latest
docker run --rm -i ghcr.io/kcrong/tmux-mcp -version
```

`tmux-mcp` is an MCP **stdio** server, so the most common way to use
the container is to let your MCP client launch it on demand. Wire
`docker` as the command and let it run the image with `-i` (interactive,
so stdin/stdout stay attached):

```jsonc
{
  "mcpServers": {
    "tmux": {
      "command": "docker",
      "args": ["run", "--rm", "-i", "ghcr.io/kcrong/tmux-mcp:latest"]
    }
  }
}
```

Pin a specific version (e.g. `ghcr.io/kcrong/tmux-mcp:v0.2.0`) instead
of `latest` if you want reproducibility.

### Embed as a Go library

If you'd rather drive a tmux session from your own Go program instead
of speaking JSON-RPC over stdio, import the public
[`pkg/tmuxctl`](https://pkg.go.dev/github.com/Kcrong/tmux-mcp/pkg/tmuxctl)
package — it exposes the same `Controller` the binary uses internally:

```go
import "github.com/Kcrong/tmux-mcp/pkg/tmuxctl"

c, _ := tmuxctl.New()
defer c.Shutdown(context.Background())

ctx := context.Background()
_ = c.CreateSession(ctx, tmuxctl.SessionSpec{Name: "demo", Command: "/bin/sh"})
_ = c.SendKeys(ctx, "demo", []string{"echo hello", "Enter"}, false)
body, _ := c.WaitForStable(ctx, "demo",
    300*time.Millisecond, 100*time.Millisecond, 5*time.Second)
fmt.Println(body)
```

A runnable end-to-end version lives at
[`examples/go-library/`](examples/go-library/) (in its own Go module so
its dependencies don't bloat the main `go.sum`).

## Wire it up

`tmux-mcp` is a generic MCP stdio server — any client that speaks MCP
over stdio can use it. The config typically looks like this:

```json
{
  "mcpServers": {
    "tmux": {
      "command": "/absolute/path/to/tmux-mcp",
      "args": []
    }
  }
}
```

Where this config goes depends on the client — most desktop MCP clients
ship a config file under their app-data directory; agent frameworks
usually have their own discovery path (e.g. `.<tool>/mcp.json` in the
project root or `~/.<tool>/mcp.json` for user-wide). Check your
client's docs for the exact path.

If your client expects a flat `{ name: spec }` map instead of an
`mcpServers` wrapper, drop the wrapper:

```json
{
  "tmux": { "command": "/absolute/path/to/tmux-mcp" }
}
```

Restart your client after editing the config. On launch the server's
tools usually appear under a namespaced prefix (e.g. `tmux__send_keys`)
so they don't collide with tools from other servers.

### Client examples

Concrete, copy-paste configs for the clients people ask about most.
Always use an **absolute** path to the binary — `which tmux-mcp` after
install gives you the right one.

#### Claude Desktop

Edit `claude_desktop_config.json`:

| OS      | Path                                                       |
| ------- | ---------------------------------------------------------- |
| macOS   | `~/Library/Application Support/Claude/claude_desktop_config.json` |
| Linux   | `~/.config/Claude/claude_desktop_config.json`              |
| Windows | `%APPDATA%\Claude\claude_desktop_config.json`              |

```json
{
  "mcpServers": {
    "tmux": {
      "command": "/usr/local/bin/tmux-mcp",
      "args": ["-log-level=info"]
    }
  }
}
```

Restart Claude Desktop. The tools will show up as `tmux__session_create`,
`tmux__send_keys`, etc.

#### Claude Code (CLI / IDE extensions)

`~/.claude/mcp.json` (user-wide) or `<repo>/.claude/mcp.json` (project):

```json
{
  "mcpServers": {
    "tmux": {
      "command": "/usr/local/bin/tmux-mcp"
    }
  }
}
```

Or use the helper:

```sh
claude mcp add tmux /usr/local/bin/tmux-mcp
```

#### VS Code (MCP extension)

In `settings.json` (User or Workspace):

```jsonc
"mcp.servers": {
  "tmux": {
    "command": "/usr/local/bin/tmux-mcp"
  }
}
```

Reload the window after saving.

#### Cursor / Windsurf / other agent frameworks

Most follow the same `{ "mcpServers": { "<name>": { "command": "..." } } }`
shape. Some expect a flat `{ "<name>": { "command": "..." } }` map — drop
the `mcpServers` wrapper if your client's docs say so. Always use an
absolute path to the binary.

### Smoke test by hand

You can drive the server from a shell to confirm it's alive:

```sh
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  | ./tmux-mcp | head
```

Each line is one JSON-RPC frame. The server responds line-by-line with
the same framing.

### Health check

For orchestrators that just need to confirm the binary is functional
(k8s liveness probes, systemd `ExecStartPre=`, Docker `HEALTHCHECK`),
use `-probe`:

```sh
tmux-mcp -probe   # prints "ok\ttmux=3.4\ttmux-mcp=v0.4.0" and exits 0
```

On failure (no tmux on `$PATH`, version too old, …) it prints a
`probe failed: …` diagnostic to stderr and exits non-zero — stdout
stays empty so a parser can rely on the `ok\t…` shape.

### Config dry-run

`-probe` only checks that `tmux` itself is healthy. When you also want
to validate the **rest** of the config — the `-socket` parent
directory, the `-audit-log` path, the `-log-format` value, every other
flag — without committing to the JSON-RPC loop, use `-dry-run`:

```sh
tmux-mcp -dry-run -socket=/run/tmux-mcp/sock -audit-log=/var/log/tmux-mcp/audit.jsonl
# prints "dry-run ok\ttmux=3.4\ttmux-mcp=v0.5.0" and exits 0
```

The flag walks the full startup path (parse flags, install slog
handler, init the tmux controller, open the audit sink, build the tool
surface), then exits **before** stdin is read. Any misconfiguration —
a relative socket, a missing audit-log parent, a bogus
`-log-format` — surfaces as a non-zero exit with the same diagnostic
the JSON-RPC server would have printed at startup, so you can iterate
on a unit file or Claude Desktop config locally before reloading
anything.

### Concurrency cap

`-max-concurrent-calls=N` caps simultaneously-executing `tools/call`
frames; excess callers wait for back-pressure rather than failing
(default `64`, `0` disables the cap entirely). `initialize`,
`tools/list`, and notifications stay ungated so probes/listings remain
snappy. When a call blocks more than 100ms a single `slog.Warn` records
the method, queue depth, and configured limit so saturation is visible.

### Process management (systemd, containers, supervisors)

By default `tmux-mcp` puts its private socket inside a freshly created
directory under `$TMPDIR`. That is fine for desktop MCP clients that
spawn the binary on demand, but it makes the socket path unpredictable —
which breaks systemd unit health checks, log forwarders, and any
supervisor that wants to peek at the underlying tmux server.

Pin the socket location with `-socket=/path` (or `TMUX_MCP_SOCKET=/path`)
so it lives at a known, well-known address:

```sh
# flag form — wins over the env var
tmux-mcp -socket=/run/tmux-mcp/sock

# env form — handy in unit files / Dockerfiles
TMUX_MCP_SOCKET=/run/tmux-mcp/sock tmux-mcp
```

Rules of the road:

- The path must be **absolute**. Relative paths are rejected up front
  with a clear error.
- The **parent directory must already exist**. `tmux-mcp` will not
  create `/run/tmux-mcp` for you — that is the operator's job (e.g. a
  systemd `RuntimeDirectory=` or a `RUN mkdir` step in a Dockerfile).
  Refusing to auto-create avoids accidentally writing to the wrong
  place when a typo sneaks in.
- On shutdown the socket file is removed but the parent directory is
  left intact, so unit restarts stay idempotent.
- If neither the flag nor the env var is set the old behaviour applies,
  so existing setups keep working unchanged.

Minimal systemd snippet:

```ini
[Service]
RuntimeDirectory=tmux-mcp
ExecStart=/usr/local/bin/tmux-mcp -socket=/run/tmux-mcp/sock
```

### Idle session reaper (`-session-idle-timeout`)

Long-running deployments accumulate orphaned tmux sessions: an agent
loses interest, an MCP client crashes mid-task, a developer forgets a
debug session running. Each one keeps a PTY, a shell, and the snapshot
history in memory until the next operator-initiated cleanup. Set
`-session-idle-timeout=DUR` to make the server reap sessions that have
seen no tool-call activity within `DUR`:

```sh
# Auto-kill any session idle for 30 minutes.
tmux-mcp -session-idle-timeout=30m

# Keep historical behaviour — never reap (default).
tmux-mcp -session-idle-timeout=0
```

Activity is any `tools/call` that names the session (`send_keys`,
`capture`, `wait_for_*`, `resize`, `snapshot_diff`, `send_signal`,
`pane_select`, `session_describe`, `session_rename`, `list_panes` when
scoped). The table-wide methods `session_list` and `kill_all_sessions`
are deliberately **excluded** so polling for state cannot keep a dead
session alive. `session_create` resets the timer for a freshly named
session.

Behaviour:

- The reaper sweeps every `min(DUR/4, 30s)` (floor `1s`), so a session
  is killed within roughly a quarter of the configured window after it
  goes quiet.
- Each kill is logged at `INFO` (`reaping idle session session=demo
  idle=…`). Failures are logged and swallowed — one wedged session
  does not strand the rest of the table.
- Default `0` disables the feature entirely; the reaper goroutine and
  per-session map are not allocated, so leaving the flag unset costs
  nothing. Negative values are rejected at startup with a non-zero exit.

### Graceful shutdown (`-shutdown-timeout`)

When a supervisor sends `SIGTERM` (or you hit `Ctrl+C`), `tmux-mcp` stops
dispatching new `tools/call` frames and waits for any in-flight
handlers to finish writing their JSON-RPC responses before exiting.
That window is bounded by `-shutdown-timeout` (default `5s`):

```sh
# Wait up to 30s for slow `wait_for_text` calls to land their response.
tmux-mcp -shutdown-timeout=30s

# Disable the drain — useful for tests and short-lived scripts that
# don't care about partial responses.
tmux-mcp -shutdown-timeout=0
```

Behaviour:

- New requests arriving after the signal get a JSON-RPC error
  (`code: -32603`, `message: "shutting down"`) instead of being
  dispatched, so a flooding client can't extend the drain window.
- If the drain finishes inside the budget the process exits **0**.
- If the budget elapses with handlers still running, `tmux-mcp` logs
  `shutdown drain timed out` at `WARN` and exits **non-zero** so
  supervisors flag the forced teardown. In-flight goroutines are
  abandoned mid-write — pair this with a slightly larger
  `TimeoutStopSec=` in your unit file when you bump the timeout above
  systemd's default.

## Deploy

### Run as a systemd service

`tmux-mcp` is a stdio MCP server, so the usual deployment is to let
your MCP client spawn it on demand. For ops who instead want a single
long-running `tmux-mcp` process pinned to a known socket (e.g. so a
thin wrapper like `socat` can hand stdin/stdout off to it), the repo
ships a reference unit at
[`scripts/tmux-mcp.service`](scripts/tmux-mcp.service). It assumes the
`-socket` flag (released in v0.2) and uses `-probe` as
`ExecStartPre=` so the unit fails fast on hosts missing `tmux`.

```sh
# 1. Create the unprivileged system user the unit runs as.
sudo useradd --system --no-create-home --shell /usr/sbin/nologin tmux-mcp

# 2. Drop the unit in place and start it.
sudo cp scripts/tmux-mcp.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now tmux-mcp
sudo systemctl status tmux-mcp
```

The unit pins the socket at `/run/tmux-mcp/sock` (managed by
`RuntimeDirectory=tmux-mcp`, mode `0700`) and applies stdlib hardening
(`NoNewPrivileges`, `ProtectSystem=strict`, `RestrictAddressFamilies=AF_UNIX`,
`MemoryMax=512M`, …). Tweak `User=`, the socket path, or the resource
caps to taste before installing.

### Audit log

Pass `-audit-log=PATH` to emit one JSONL record per `tools/call` (other
methods are protocol bookkeeping and stay out of the log). Use
`-audit-log=stderr` to share the slog stream, or any other value as a
file path opened append-only at mode `0600`. Each record carries
`ts`, `request_id`, `tool`, `session`, `duration_ms`, `result`,
`error_code` (errors only), and `args_size_bytes`.
**Privacy:** `args_size_bytes` is the byte length of the raw arguments
JSON only — argument *content* is never logged, so commands and any
embedded secrets stay out of the audit trail.

## Tool surface

| Tool | Purpose |
| --- | --- |
| `session_create` | Start a new detached `tmux` session running a command, with chosen size. |
| `session_list` | List sessions managed by this server. |
| `session_kill` | Kill a session by name. |
| `kill_all_sessions` | Kill every session this server manages and clear all snapshot history. |
| `session_describe` | Return structured metadata for one session (windows, panes, size, creation time). |
| `session_rename` | Rename an existing session via `tmux rename-session -t OLD NEW`. |
| `session_inspect` | Return process-level metadata (pid, cwd, command) for a session's active pane. |
| `send_keys` | Type into a session. Accepts literal text or named keys (`C-c`, `Up`, `Enter`, …). |
| `capture` | Read the visible pane (or scrollback) as text, optionally with ANSI escapes. |
| `wait_for_stable` | Block until the screen has not changed for `quiet_ms`, then return the snapshot. |
| `wait_for_text` | Block until a regex appears on screen, then return the match + snapshot. |
| `snapshot_diff` | Capture and return only what changed since a previous snapshot token. |
| `resize` | Resize the pane (cols × rows). |
| `list_panes` | Enumerate panes (optionally scoped to a session) so an agent can target a non-default pane. |
| `pane_select` | Make a `session:window.pane` target the active pane of its window. |
| `pane_split` | Split a pane horizontally or vertically; optionally run a command in the new pane. |
| `pane_kill` | Destroy a single pane via `tmux kill-pane`; tmux's own collapse semantics on the last pane of a window/session are preserved. |
| `pane_join` | Move a pane out of its current window and re-attach it to another window via `tmux join-pane`; horizontal flag toggles `-h`. |
| `clear_history` | Drop the scrollback buffer of a pane via `tmux clear-history`; visible region and running process are untouched. |
| `send_signal` | Send a POSIX signal (TERM, HUP, INT, ...) to the session's active pane PID. |
| `window_create` | Add a new window to an existing session (optional name / command, focus toggle). |
| `new_window` | Like `window_create` but returns a JSON payload with the new window's index, id, and name; supports an `after_index` insertion slot. |
| `window_kill` | Destroy a single window of a session; refuses the last remaining window. |
| `window_select` | Make a window the active one of its session via `tmux select-window`. |
| `window_rename` | Rename a window via `tmux rename-window` (same regex/length policy as window_create). |
| `list_windows` | Enumerate windows (optionally scoped to a session) with their index, name, active flag, and pane count. |

Per-tool reference (inputs, outputs, error codes, examples) lives in
[`docs/tools.md`](docs/tools.md). The canonical schemas live in
[`internal/server/tools.go`](internal/server/tools.go).

## End-to-end example

A complete walkthrough showing how an agent might smoke-test `htop`:

```jsonc
// 1. spin up a session
{ "name": "session_create",
  "arguments": { "name": "top", "command": "/bin/sh", "width": 100, "height": 30 } }

// 2. launch the program
{ "name": "send_keys",
  "arguments": { "session": "top", "keys": ["htop", "Enter"] } }

// 3. wait for the UI to settle
{ "name": "wait_for_stable",
  "arguments": { "session": "top", "quiet_ms": 500, "timeout_ms": 5000 } }

// 4. confirm we're really in htop
{ "name": "wait_for_text",
  "arguments": { "session": "top", "pattern": "F10\\s*Quit", "timeout_ms": 3000 } }

// 5. interact (sort by memory)
{ "name": "send_keys", "arguments": { "session": "top", "keys": ["F6"] } }
{ "name": "wait_for_stable", "arguments": { "session": "top", "quiet_ms": 200 } }

// 6. quit cleanly
{ "name": "send_keys", "arguments": { "session": "top", "keys": ["F10"] } }
{ "name": "session_kill", "arguments": { "name": "top" } }
```

## Patterns

### Press a key, then read the screen

Don't capture immediately after `send_keys` — TUIs redraw on every
keystroke and you'll see a half-rendered frame. Wait for the pane to
settle first:

```text
send_keys       session=demo  keys=["echo hi", "Enter"]
wait_for_stable session=demo  quiet_ms=300
capture         session=demo
```

### Wait for the prompt to come back

Use `wait_for_text` with a regex that matches your shell prompt or a
sentinel your tool prints when it's ready. More robust than a fixed
sleep.

```text
send_keys      session=demo  keys=["./long-build", "Enter"]
wait_for_text  session=demo  pattern="^\\$ $"  timeout_ms=300000
```

### Only show me what changed

After your first capture you'll get a `token`. Pass it to
`snapshot_diff` on the next call to receive only the lines that changed.

```jsonc
// first call
{ "name": "snapshot_diff", "arguments": { "session": "demo", "prior_token": "" } }
// next call, with the token from the previous response
{ "name": "snapshot_diff", "arguments": { "session": "demo", "prior_token": "ab12cd34" } }
```

### Cancel a runaway TUI

```text
send_keys       session=demo  keys=["C-c"]
wait_for_stable session=demo  quiet_ms=200
```

## Architecture

```
                       ┌────────────────────────┐
   MCP client (LLM) ── │  stdio: JSON-RPC 2.0   │ ── tmux-mcp
                       └────────────────────────┘
                                                 │
                                  exec.Command   │   tmux -S /tmp/.../sock
                                                 ▼
                                ┌─────────────────────────────────────┐
                                │ tmux server (one per tmux-mcp PID)  │
                                │   ├── session "demo"                │
                                │   │     └── window 0 ── pane (PTY)  │
                                │   └── session "build"               │
                                └─────────────────────────────────────┘
```

One `tmux-mcp` instance owns one private socket, which means one isolated
`tmux` server. Two `tmux-mcp` PIDs cannot see each other's sessions even
on the same host — each gets its own socket under `/tmp`. The MCP server
itself is single-process; each incoming request is dispatched on its own
goroutine, so a slow `wait_for_text` doesn't block other traffic on the
same stdio pipe.

## Design notes

- **No shared global tmux server.** Each invocation of `tmux-mcp` uses
  its own private socket via `tmux -S`, so concurrent servers don't see
  each other's sessions.
- **Stable detection is timer-based.** The server polls the pane until
  it has been unchanged for `quiet_ms`. This works well for TUIs that
  redraw on every keystroke; pathological always-changing widgets
  (spinners) should be masked at the application level.
- **Diff snapshots are line-anchored.** `snapshot_diff` returns the
  changed lines plus their indices, plus an opaque token to pass on the
  next call.
- **Each request is dispatched on its own goroutine** so a slow tool
  call (e.g. a 30s `wait_for_text`) does not block other traffic on the
  same stdio pipe.

## FAQ

**Q: Can two agents share one `tmux-mcp`?**
A: Yes — they will see the same sessions, which is useful for
collaboration. To isolate them, run a separate `tmux-mcp` process per
agent; each gets its own private socket and its own tmux server.

**Q: Does `tmux-mcp` persist sessions across restarts?**
A: No. When `tmux-mcp` exits, the private tmux server it spawned exits
with it and every session dies. `tmux-mcp` is designed for ephemeral
driving by an agent, not for long-lived sessions you reattach to later.

**Q: Why does my regex for `wait_for_text` always time out?**
A: It is a Go [RE2](https://pkg.go.dev/regexp/syntax) regex, not a shell
glob. Escape any of `.`, `?`, `+`, `*`, `(`, `)`, `[`, `]`, `{`, `}`,
`^`, `$`, `|`, `\` you mean literally. When in doubt, prototype the
pattern in a small `go run` snippet against a captured pane.

**Q: Can I use `tmux-mcp` on Windows?**
A: Binaries cross-compile for Windows, but `tmux` itself only runs on
Linux and macOS. Use WSL or `ssh` into a *nix host and point your MCP
client at the binary there.

**Q: How do I debug what tools the agent is calling?**
A: Run with `-log-level=debug`. Each request logs `rid`, `method`, and
`dur_ms` to stderr — stdout stays pure JSON-RPC, so the logs never
corrupt the protocol stream. Add `-log-source=true` when investigating
an issue to attach the `file:line` of each emitting `slog` call to the
record (small perf cost — leave off in production).

**Q: Is the `snapshot_diff` token persistent?**
A: No. Snapshots are kept in memory per session, and only the two most
recent are retained. The token is good for short-lived comparisons
between consecutive calls; older tokens fall back to a full reset where
every line is reported as new. Sessions that go idle longer than
`-snapshot-ttl` (default `1h`) are pruned in the background to bound
memory growth on long-running servers — set `-snapshot-ttl=0` to keep
history until the session is killed explicitly.

## Troubleshooting

The defaults aim to "just work" for desktop MCP clients, but a handful
of failure modes show up often enough to be worth naming. Each entry
below maps the symptom to the smallest fix and links to the flag or
section that documents it in full.

Want to validate config before swap? Run [`tmux-mcp -dry-run`](#config-dry-run) with the flags you intend to ship — it walks the full bootstrap and exits 0 (or prints the same startup error you'd see at runtime) without ever opening stdin.

### `tmux server already running` / socket conflict

Every `tmux-mcp` PID gets its own private socket under `$TMPDIR` by
default, so two instances never collide unless an operator pinned both
to the same path. If you see this, either drop the explicit socket flag
and let the server pick a fresh path, or override
[`-socket`](#process-management-systemd-containers-supervisors) (or the
`TMUX_MCP_SOCKET` env var) to a path that no other process owns —
e.g. `tmux-mcp -socket=/run/tmux-mcp/agent-b.sock`. The flag wins over
the env var, so a one-off launch can override a baked-in unit setting.

### `tmux: command not found` / startup fails before stdio

`tmux-mcp` looks `tmux` up on `$PATH` at startup and refuses to serve
without it. Run [`tmux-mcp -probe`](#health-check) to confirm — it
prints `ok\ttmux=<v>\ttmux-mcp=<v>` on success, or `probe failed: …`
to stderr with the exact diagnostic. Install with your package manager:
`sudo apt-get install tmux` on Debian/Ubuntu, `brew install tmux` on
macOS. Container users can pull `ghcr.io/kcrong/tmux-mcp` which already
bundles `tmux`.

### `audit-log permission denied` / nothing in the audit file

The [`-audit-log`](#audit-log) flag opens its target append-only at
mode `0600`, so the running user must be able to create or write to
that file and **the parent directory must already exist** —
`tmux-mcp` will not `mkdir -p` for you. Under systemd, expand
`ReadWritePaths=` to include the audit path (or put the file under the
unit's `RuntimeDirectory=` / `LogsDirectory=`), otherwise the strict
`ProtectSystem=strict` sandbox in
[`scripts/tmux-mcp.service`](scripts/tmux-mcp.service) will block the
open. Use `-audit-log=stderr` to side-step file permissions entirely
when you only need ad-hoc inspection.

### JSON-RPC frame too large / `EOF` after a `capture`

A `capture` with `mode=scrollback` on a long-lived shell can return
tens of MB in a single response, which inflates the JSON-RPC frame
past what some clients buffer comfortably and shows up as an `EOF`
or truncated read. The [`capture`](#capture) tool already caps
`mode=scrollback` at **5000 lines by default**; lower it further with
`max_lines` (e.g. `"max_lines": 500`) when you only need recent
history. The oldest lines drop first and the response sets
`truncated: true` so the agent knows it saw a window, not the full
buffer. See also [Performance & tuning](#performance--tuning) for the
broader rationale.

### Server hangs on shutdown

`tmux-mcp` installs a SIGINT/SIGTERM handler via
`signal.NotifyContext`, then drains every in-flight `tools/call` before
returning — see [Architecture](#architecture). A hang almost always
means a handler is stuck inside tmux itself (e.g. a `wait_for_text`
that never matches with no `timeout_ms`). Always pass a finite
`timeout_ms` on long waits, and send SIGKILL as a last resort if the
process needs to disappear immediately. A `-shutdown-timeout` flag is
on the roadmap for a hard ceiling on the drain; until it lands, rely
on per-call timeouts plus the supervisor's own `TimeoutStopSec=`.

### Why are my hangs so long after the client closed?

When the MCP client closes its end of the stdio pipe (process exit, pipe
break, IDE shutdown without SIGTERM), `tmux-mcp` cancels every in-flight
handler's request context immediately — a `wait_for_text` call with
`timeout_ms=10000` returns within one poll step (~100 ms) instead of
riding out the full 10 seconds. If you're seeing a multi-second hang
after a client disconnect, the call is almost certainly stuck inside
tmux itself rather than spinning on its own polling loop; check the
structured log (each request carries an `rid` correlation key) for the
last `rpc start` with no matching `rpc end`, and confirm the
`-shutdown-timeout` you've configured (default `5s`) bounds the wait.
Code surfaced to the client in this case is `-32003`
("context cancelled"), not `-32002` ("timeout"), so callers can
distinguish "client gave up" from "wait budget exceeded".

### Sessions disappear without an explicit `session_kill`

If `-session-idle-timeout` is set, any session that goes silent for
longer than the configured window is reaped automatically — see
[Idle session reaper](#idle-session-reaper--session-idle-timeout). Look
for `reaping idle session` in the structured log to confirm. Reduce
the window's risk by either bumping `DUR`, calling a quick `capture`
to renew the timer on sessions you want to keep, or set
`-session-idle-timeout=0` to disable the feature entirely. Note:
`session_list` and `kill_all_sessions` deliberately do **not** count as
activity, so a polling client cannot keep a stale session alive.

### Tool calls deadlock under load

The [concurrency cap](#concurrency-cap) (`-max-concurrent-calls`,
default `64`) makes excess `tools/call` frames wait rather than fail.
That is the desired back-pressure, but a misconfigured cap of `1` or
`2` plus a long-running `wait_for_text` will look like a deadlock to
the client. Either raise the cap (`-max-concurrent-calls=128`) or set
it to `0` to disable gating entirely. When a call blocks more than
100 ms a single `slog.Warn` records the queue depth, so look for
`rpc concurrency wait` in the structured log to confirm saturation
before tuning.

### `tmux version too old`

`tmux-mcp` requires **tmux 3.0+** (older releases lack flags this
package depends on, e.g. `new-session -x/-y`) — see
[Requirements](#requirements). Upgrade with `apt-get install tmux`
or `brew upgrade tmux`; on a frozen distro pin a backport repo or
build from source. The error wraps a stable JSON-RPC code so clients
can switch on it instead of substring-matching the message, and
`tmux-mcp -probe` surfaces the same diagnostic before the JSON-RPC
loop starts.

## Performance & tuning

`tmux-mcp` is happy under everyday agent traffic, but there are a few
knobs an operator should know about before pointing it at a heavy-load
TUI workflow or a fleet of long-lived sessions. Each item below is a
real foot-gun seen in practice — the defaults are deliberately
conservative so that "do nothing" is the safe choice.

- **Scrollback capping.** `capture` with `mode=scrollback` is bounded
  at **5000 lines by default**. Lifting that cap with a large
  `max_lines` (or `max_lines: 0`) on a long-lived shell can trivially
  return tens of MB of text in a single response, which inflates the
  JSON-RPC frame, stalls the stdio pipe while the client decodes it,
  and burns model context if an agent forwards the whole snapshot.
  Stick with the default unless you genuinely need deeper history; if
  you do, prefer the smallest value that answers your question and let
  the oldest lines drop (the response keeps the most recent activity
  and sets `truncated: true`).

- **`wait_for_text` regex caching.** Compiled regexes are cached
  internally, so calling `wait_for_text` repeatedly with the same
  `pattern` is materially cheaper than rotating patterns each call.
  When you're polling for the same prompt or sentinel — `^\$ $`,
  `READY-\d+`, an app-specific banner — reuse one canonical pattern
  string instead of building a fresh one per request and you'll skip
  the recompile on every iteration.

- **Polling cadence (`step_ms`).** `wait_for_stable` and
  `wait_for_text` re-capture the pane every `step_ms` milliseconds. A
  very small value (e.g. `step_ms: 20`) means you fire ~50
  `tmux capture-pane` calls per second per waiter, which under
  concurrent agents quickly saturates the tmux server and starves real
  work. **100–200 ms is the sweet spot** — fast enough that humans and
  agents perceive it as responsive, slow enough that capture overhead
  stays in the noise. Drop below 100 ms only when you've measured a
  genuine need.

- **Sessions per server instance.** Operations on a session take a
  per-controller mutex, so as one `tmux-mcp` PID accumulates many
  active sessions (especially with several agents waiting on different
  panes at once) you'll start to see contention show up as latency on
  unrelated calls. If you're driving more than a handful of busy
  sessions, prefer to **shard across multiple `tmux-mcp` processes**
  (each gets its own private socket and tmux server — see
  [Architecture](#architecture)) rather than piling everything onto one
  instance.

- **Concurrent-call rate limiting.** The server does not yet cap how
  many tool calls run in flight; in practice this is bounded by the
  client, but a misbehaving caller can flood the dispatch path. A
  `-max-concurrent-calls` flag (default 64) is being added in
  [PR #51](https://github.com/Kcrong/tmux-mcp/pull/51) — once that
  lands, set it explicitly on shared deployments so a runaway client
  cannot exhaust goroutines or the tmux server.

## Metrics (Prometheus)

`tmux-mcp` ships an optional Prometheus exporter behind the
`-metrics-addr` flag. The default is **disabled** — when the flag is
empty no extra HTTP listener is opened and the dispatcher's metrics
hooks are no-ops, so existing deployments see zero overhead. Pass an
address to turn it on:

```bash
tmux-mcp -metrics-addr=127.0.0.1:9090
```

The flag value is passed to `net.Listen` verbatim, so the bind scope is
explicit: `127.0.0.1:9090` exposes /metrics only to local scrapers,
`:9090` binds to every interface. Pick whichever your deployment needs;
the server does **not** silently expand to all interfaces.

Three series are published, alongside the standard Go runtime / process
collectors:

| Metric                                  | Type      | Labels         | Meaning                                                              |
|-----------------------------------------|-----------|----------------|----------------------------------------------------------------------|
| `tmuxmcp_tools_call_total`              | counter   | `tool`, `result` (`ok`/`error`) | One increment per `tools/call` dispatch. Other JSON-RPC methods (`initialize`, `tools/list`, notifications) are intentionally not metered. |
| `tmuxmcp_tools_call_duration_seconds`   | histogram | `tool`         | Wall-clock duration of each `tools/call`. Buckets cover 1 ms – 10 s. |
| `tmuxmcp_sessions_active`               | gauge     | none           | Number of tmux sessions on this server's private socket. Refreshed by a 5 s background poller. |

Curl example:

```bash
$ curl -s http://127.0.0.1:9090/metrics | grep -E '^tmuxmcp'
tmuxmcp_sessions_active 2
tmuxmcp_tools_call_duration_seconds_bucket{tool="capture",le="0.005"} 4
…
tmuxmcp_tools_call_total{result="ok",tool="capture"} 4
tmuxmcp_tools_call_total{result="error",tool="capture"} 0
```

Operational notes:

- The exporter binds **eagerly** at startup. A misconfigured address
  (port already in use, malformed host:port) surfaces as a clean
  startup failure instead of a half-running goroutine.
- Metrics are best-effort: a transient `ListSessions` failure logs a
  single `metrics: ListSessions failed` warning and the gauge keeps its
  prior value. The exporter never crashes the server on its own.
- The `tool` label is bounded by the static MCP tool surface — there is
  no client-controlled cardinality vector. Add new tools sparingly to
  keep the histogram series count predictable.
- The `result` label is normalised to exactly `"ok"` or `"error"`; no
  handler-specific strings ever leak into label values.


## Releases

Releases are cut automatically by
[release-please](https://github.com/googleapis/release-please) from
[Conventional Commits](https://www.conventionalcommits.org/) on `main`:

- Every push to `main` updates a long-lived "release PR" that accumulates
  the pending changelog and bumps the next semver based on the commit
  types it sees (`feat:` → minor, `fix:`/`perf:` → patch, anything with
  `!` or a `BREAKING CHANGE:` footer → major).
- Merging that release PR tags the new version and publishes a GitHub
  Release. The existing release workflow then triggers off the tag and
  builds binaries, signatures, and SBOMs (see
  [Verifying a release](#verifying-a-release)).

Contributors should write commits in Conventional Commits style
(`feat:`, `fix:`, `perf:`, `ci:`, `docs:`, `test:`, `refactor:`,
`chore:`) so release-please can categorise them. `chore:` is hidden
from the changelog. Manual `git tag` is no longer needed.

## Verifying a release

Each release ships three layers of provenance:

1. **`checksums.txt`** — SHA-256 of every archive.
2. **Cosign keyless signatures** — every archive, `checksums.txt`, and
   every SBOM is signed via GitHub Actions OIDC (no long-lived key) and
   the signing event is recorded in the public
   [Rekor transparency log](https://docs.sigstore.dev/logging/overview/).
   Each artifact has a sibling `<artifact>.sig` (signature) and
   `<artifact>.pem` (signing certificate).
3. **SPDX SBOMs** — one
   [SPDX 2.3 JSON](https://spdx.dev/use/specifications/) document per
   archive, named `<archive>.sbom.json`, listing every Go module that
   went into the binary.

### Step 1 — checksums

```sh
sha256sum -c checksums.txt --ignore-missing
```

### Step 2 — cosign signatures

Install [cosign](https://docs.sigstore.dev/cosign/installation/) (e.g.
`brew install cosign`), then verify any artifact against the GitHub
Actions OIDC identity that produced the release:

```sh
# Replace <tag> with the release tag (e.g. v0.2.0) and <archive> with
# the file you downloaded (e.g. tmux-mcp_Linux_x86_64.tar.gz).
TAG=<tag>
ARCHIVE=<archive>

cosign verify-blob \
  --certificate "${ARCHIVE}.pem" \
  --signature   "${ARCHIVE}.sig" \
  --certificate-identity-regexp \
    "https://github.com/Kcrong/tmux-mcp/.github/workflows/release.yml@refs/tags/${TAG}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  "${ARCHIVE}"
```

The same command verifies `checksums.txt` and any
`*.sbom.json` — just point `ARCHIVE` at that file. A successful verify
prints `Verified OK` and confirms the artifact came from this repo's
release workflow at the given tag, with the event logged to Rekor.

### Step 3 — SBOM

Each `<archive>.sbom.json` is an SPDX 2.3 document. Inspect it with
[`syft`](https://github.com/anchore/syft), `jq`, or any SPDX-aware tool:

```sh
syft convert tmux-mcp_Linux_x86_64.tar.gz.sbom.json -o table
```

`checksums.txt`, signatures, certificates, and SBOMs are all built by
GoReleaser inside the release workflow — the
[release run](https://github.com/Kcrong/tmux-mcp/actions/workflows/release.yml)
in GitHub Actions is the authoritative provenance.

### Reproducible builds

Releases are built reproducibly: the GoReleaser config pins build
timestamps to the commit's authored time (`mod_timestamp` /
`builds_info.mtime`) and uses `-trimpath` plus `-buildvcs=false`, so
running `goreleaser release --snapshot --clean` (or `make
release-snapshot`) from the same commit produces byte-identical
binaries and tarballs as the official release. You can rebuild from
source and check the resulting archive's SHA-256 against
`checksums.txt` to confirm nothing in the supply chain changed your
binary.

## Examples

Concrete prompts, JSON-RPC transcripts, and ready-made MCP client configs live in [`examples/`](examples/).

## License

MIT — see [LICENSE](LICENSE).
