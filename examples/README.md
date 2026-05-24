# Examples

Concrete, copy-pastable artifacts for driving `tmux-mcp` end-to-end. The
top-level [README](../README.md) inlines short snippets; this directory
is the canonical place to point new users at.

## prompts/

Agent prompt templates that drive `tmux-mcp` tools end-to-end. Drop one
into your client's system prompt (or hand it to the model as the user
goal) and the agent will know exactly which tools to call in which
order.

## transcripts/

Actual JSON-RPC wire transcripts. Each line is one frame, prefixed with
`CLIENT:` (request) or `SERVER:` (response). Use them to see the exact
framing without spinning up a client.

## mcp-configs/

Drop-in configs for popular MCP clients — Claude Desktop, Claude Code,
and the VS Code MCP extension. Replace `/usr/local/bin/tmux-mcp` with
the absolute path printed by `which tmux-mcp`.
