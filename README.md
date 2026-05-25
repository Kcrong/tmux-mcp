# tmux-mcp

[![CI](https://github.com/Kcrong/tmux-mcp/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/Kcrong/tmux-mcp/actions/workflows/ci.yml)
[![security](https://github.com/Kcrong/tmux-mcp/actions/workflows/security.yml/badge.svg?branch=main)](https://github.com/Kcrong/tmux-mcp/actions/workflows/security.yml)
[![release](https://github.com/Kcrong/tmux-mcp/actions/workflows/release.yml/badge.svg?branch=main)](https://github.com/Kcrong/tmux-mcp/actions/workflows/release.yml)
[![codecov](https://codecov.io/gh/Kcrong/tmux-mcp/branch/main/graph/badge.svg)](https://codecov.io/gh/Kcrong/tmux-mcp)
[![Go Reference](https://pkg.go.dev/badge/github.com/Kcrong/tmux-mcp.svg)](https://pkg.go.dev/github.com/Kcrong/tmux-mcp)
[![Go Report Card](https://goreportcard.com/badge/github.com/Kcrong/tmux-mcp)](https://goreportcard.com/report/github.com/Kcrong/tmux-mcp)
[![Latest Release](https://img.shields.io/github/v/release/Kcrong/tmux-mcp?display_name=tag&sort=semver)](https://github.com/Kcrong/tmux-mcp/releases/latest)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Give your LLM agent a real terminal.

`tmux-mcp` is a Model Context Protocol (MCP) stdio server that exposes a
live `tmux` session to an agent. The agent types into a real PTY, waits
for the screen to settle, reads the visible pane, and reacts — the same
loop a human runs at a terminal.

Use it for things like driving a TUI, running interactive shells,
watching a long build, or smoke-testing a CLI from inside an MCP-aware
client (Claude Desktop, Claude Code, Cursor, VS Code, …).

## Install

Pick one. All three give you a `tmux-mcp` binary on `$PATH`.

```sh
# 1) Prebuilt binary (Linux/macOS, amd64/arm64)
curl -fsSL https://github.com/Kcrong/tmux-mcp/releases/latest/download/tmux-mcp_$(uname -s)_$(uname -m).tar.gz \
  | sudo tar -xz -C /usr/local/bin tmux-mcp

# 2) Go toolchain
go install github.com/Kcrong/tmux-mcp/cmd/tmux-mcp@latest

# 3) Container (tmux is bundled)
docker pull ghcr.io/kcrong/tmux-mcp:latest
```

Requirements: `tmux` 3.0+ on Linux or macOS. Windows users need WSL or
ssh to a *nix host — `tmux` itself does not run on Windows.

Verify the install:

```sh
tmux-mcp -version
```

## Wire it up

`tmux-mcp` is a generic MCP stdio server. Point your client's config at
the binary's absolute path. Example for Claude Desktop
(`~/Library/Application Support/Claude/claude_desktop_config.json` on
macOS, `~/.config/Claude/claude_desktop_config.json` on Linux):

```json
{
  "mcpServers": {
    "tmux": {
      "command": "/usr/local/bin/tmux-mcp"
    }
  }
}
```

Restart the client. The tools appear under the `tmux__` prefix
(`tmux__session_create`, `tmux__send_keys`, …).

The same shape works for Claude Code (`~/.claude/mcp.json`), Cursor,
Windsurf, and the VS Code MCP extension. Some clients want a flat
`{ "tmux": { "command": "..." } }` map instead — drop the `mcpServers`
wrapper if your client's docs say so.

To run the container instead of a local binary:

```json
{
  "mcpServers": {
    "tmux": {
      "command": "docker",
      "args": ["run", "--rm", "-i", "ghcr.io/kcrong/tmux-mcp:latest"]
    }
  }
}
```

## Tool surface

The agent gets these tools (full reference in
[`docs/tools.md`](docs/tools.md)):

| Tool | What it does |
| --- | --- |
| `session_create`, `session_kill`, `session_list`, `session_describe`, `session_rename`, `session_inspect`, `has_session`, `kill_all_sessions` | Lifecycle and metadata for tmux sessions. |
| `send_keys` | Type literal text or named keys (`Enter`, `C-c`, `Up`, …) into a session. |
| `capture` | Read the visible pane (or scrollback) as text, optionally with ANSI escapes. |
| `wait_for_stable`, `wait_for_text` | Block until the screen settles, or until a regex appears. |
| `snapshot_diff` | Return only the lines that changed since a previous snapshot. |
| `resize`, `pane_split`, `pane_join`, `pane_kill`, `pane_select`, `list_panes` | Pane geometry and layout. |
| `window_create`, `new_window`, `window_kill`, `window_select`, `window_rename`, `list_windows` | Window management. |
| `clear_history`, `send_signal`, `show_options` | Misc: scrollback, POSIX signals, tmux options. |

Every flag and environment variable the binary accepts is catalogued in
[`docs/flags.md`](docs/flags.md).

## License

MIT — see [LICENSE](LICENSE).
