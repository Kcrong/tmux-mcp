# tmux-mcp

A Model Context Protocol (MCP) stdio server that exposes a real `tmux`
session to an LLM agent so it can drive a terminal the way a human
does — typing into a real PTY, waiting for the screen to settle, reading
the visible pane, and reacting.

The goal is to keep the agent's experience as close to a human user's
as possible: it sees what you would see, types what you would type, and
waits the way you would wait.

> **Docs:** the same content with examples lives at
> [kcrong.github.io/tmux-mcp](https://kcrong.github.io/tmux-mcp/).

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

## Install

### From source

```sh
git clone https://github.com/Kcrong/tmux-mcp.git
cd tmux-mcp
make build              # produces ./tmux-mcp
./tmux-mcp < /dev/null  # smoke test — exits cleanly on EOF
```

### With `go install`

```sh
go install github.com/Kcrong/tmux-mcp/cmd/tmux-mcp@latest
which tmux-mcp
```

Make sure `$(go env GOBIN)` (or `$GOPATH/bin`) is on `$PATH`, otherwise
your MCP client won't find the binary.

## Wire it up

### With the [coding](https://github.com/Kcrong/coding) agent

Add an entry to your project's `.coding/mcp.json` (or
`~/.coding/mcp.json` for a user-wide config). The config is a flat
`{ name: spec }` map:

```json
{
  "tmux": {
    "command": "/absolute/path/to/tmux-mcp"
  }
}
```

On launch, every tool is exposed as `tmux__<tool-name>` —
e.g. `tmux__session_create`, `tmux__send_keys`, `tmux__capture`.

### With any MCP client

The server speaks JSON-RPC 2.0 over stdin/stdout. Most clients use a
config block similar to this:

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

### Smoke test by hand

```sh
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  | ./tmux-mcp | head
```

Each line is one JSON-RPC frame. The server responds line-by-line with
the same framing.

## Tool surface

| Tool | Purpose |
| --- | --- |
| `session_create` | Start a new detached `tmux` session running a command, with chosen size. |
| `session_list` | List sessions managed by this server. |
| `session_kill` | Kill a session by name. |
| `send_keys` | Type into a session. Accepts literal text or named keys (`C-c`, `Up`, `Enter`, …). |
| `capture` | Read the visible pane (or scrollback) as text, optionally with ANSI escapes. |
| `wait_for_stable` | Block until the screen has not changed for `quiet_ms`, then return the snapshot. |
| `wait_for_text` | Block until a regex appears on screen, then return the match + snapshot. |
| `snapshot_diff` | Capture and return only what changed since a previous snapshot token. |
| `resize` | Resize the pane (cols × rows). |

The full schemas live in
[`internal/server/tools.go`](internal/server/tools.go).

## Patterns

### Press a key, then read the screen

Don't capture immediately after `send_keys` — TUIs redraw on every
keystroke and you'll see a half-rendered frame. Wait for the pane to
settle first:

```text
tmux__send_keys      session=demo  keys=["echo hi", "Enter"]
tmux__wait_for_stable session=demo  quiet_ms=300
tmux__capture        session=demo
```

### Wait for the prompt to come back

Use `wait_for_text` with a regex that matches your shell prompt or a
sentinel your tool prints when it's ready. More robust than a fixed
sleep.

```text
tmux__send_keys      session=demo  keys=["./long-build", "Enter"]
tmux__wait_for_text  session=demo  pattern="^\\$ $"  timeout_ms=300000
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
tmux__send_keys      session=demo  keys=["C-c"]
tmux__wait_for_stable session=demo  quiet_ms=200
```

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

## Troubleshooting

- **`tmux not found on PATH`** — install `tmux` with your package
  manager. The server probes `$PATH` at startup.
- **Capture looks empty even though the program is running** — you
  probably captured during a redraw. Call `wait_for_stable` first, or
  wait for a sentinel string with `wait_for_text`.
- **Two agents see each other's sessions** — they shouldn't; each
  `tmux-mcp` instance creates its own private socket. If you're seeing
  leakage, you're sharing one server process across both agents. Spawn a
  separate `tmux-mcp` per agent.
- **Agent calls report `method not found: tools/call:<name>`** — your
  client is calling a tool the server doesn't expose. Run `tools/list`
  to see the canonical names.

## License

MIT — see [LICENSE](LICENSE).
