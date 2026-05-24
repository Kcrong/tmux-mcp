# tmux-mcp

A Model Context Protocol (MCP) stdio server that exposes a real `tmux`
session to an LLM agent so the agent can drive a terminal the way a human
does — typing into a real PTY, waiting for the screen to settle, reading
the visible pane, and reacting.

The goal is to keep the agent's experience as close to a human user's as
possible: it sees what you would see, types what you would type, and waits
the way you would wait.

## Status

Early. The wire protocol is stable; the tool surface may grow.

## Why tmux

Raw PTY harnesses make capturing "what is currently on screen" hard
because you have to maintain your own terminal emulator. `tmux` already
solves this:

- `tmux send-keys` accepts both literal text and named keys (`Up`,
  `Enter`, `C-c`).
- `tmux capture-pane -p [-e]` prints the current pane contents — with
  ANSI sequences preserved on request.
- Sessions and windows are first-class, so multiple agents can share a
  host without stepping on each other.

## Tool surface

All tools are exposed over MCP `tools/call`. The schemas live in
[`internal/server/tools.go`](internal/server/tools.go). Summary:

| Tool | Purpose |
| --- | --- |
| `session_create` | Start a new detached tmux session running a command, with chosen size. |
| `session_list` | List sessions managed by this server. |
| `session_kill` | Kill a session by name. |
| `send_keys` | Type into a session. Accepts literal text or named keys (`C-c`, `Up`, `Enter`, …). |
| `capture` | Read the visible pane (or scrollback) as text, optionally with ANSI escapes. |
| `wait_for_stable` | Block until the screen has not changed for `quiet_ms`, then return the snapshot. |
| `wait_for_text` | Block until a regex appears on screen, then return the match + snapshot. |
| `snapshot_diff` | Return only what changed since a previous snapshot token. |
| `resize` | Change the pane size (rows × cols). |

## Quickstart

```sh
make build
./tmux-mcp
```

The binary speaks MCP over stdin/stdout. Wire it up as a server in your
agent's MCP config; for the [coding](https://github.com/Kcrong/coding)
agent that means a `mcp.json` entry like:

```json
{
  "tmux": { "command": "/path/to/tmux-mcp" }
}
```

## Requirements

- `tmux` 3.0+ on `$PATH`.
- A POSIX-y OS (Linux / macOS).

## Design notes

- **No shared global tmux server.** Each invocation of `tmux-mcp` uses
  its own `-L <socket>` so concurrent servers don't see each other's
  sessions.
- **Stable detection is timer-based.** The server polls the pane until
  it has been unchanged for `quiet_ms`. This works well for TUIs that
  redraw on every keystroke; pathological always-changing widgets
  (spinners) should be masked at the application level.
- **Diff snapshots are line-anchored.** `snapshot_diff` returns the
  changed lines plus their indices, plus an opaque token to pass on the
  next call.

## License

MIT — see [LICENSE](LICENSE).
