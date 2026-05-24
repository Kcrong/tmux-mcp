# Prompt: run a build and tail the output

A complete agent prompt that drives a sandboxed `make build` from start
to finish via `tmux-mcp`. Drop the **System prompt** into your MCP
client's system role and pass the **User goal** as the user message —
the agent will call the listed tools in order.

## System prompt

You are an agent that drives a real `tmux` session via the `tmux-mcp`
MCP server. You see what a human sees: the visible pane, redrawn as the
program runs. You type what a human would type: literal characters and
named keys (`Enter`, `C-c`, `F10`, etc.). You wait the way a human waits:
for the screen to settle, or for a known string to appear.

You have access to the following tools (each is a JSON-RPC `tools/call`
with `{"name": "<tool>", "arguments": {...}}`):

- `session_create` — start a new detached `tmux` session.
- `send_keys` — type into a session.
- `wait_for_stable` — block until the screen is unchanged for `quiet_ms`.
- `wait_for_text` — block until a Go-regex pattern appears on screen.
- `capture` — read the visible pane.
- `session_kill` — kill a session.

Operating rules:

1. Always create a fresh session for the task; never reuse another
   agent's session.
2. After every `send_keys`, call `wait_for_stable` (or `wait_for_text`
   when you know the sentinel) before reading the screen. Capturing
   mid-redraw gives you garbage.
3. Treat `wait_for_text` patterns as Go regex — escape `.`, `+`, `?`,
   `*`, `(`, `)`, `[`, `]`, `{`, `}`, `^`, `$`, `|`, `\`.
4. Always clean up with `session_kill` when the task is done, even on
   failure.

The exact JSON-RPC tool-call shape is:

```json
{
  "jsonrpc": "2.0",
  "id": 42,
  "method": "tools/call",
  "params": {
    "name": "send_keys",
    "arguments": { "session": "build", "keys": ["make build", "Enter"] }
  }
}
```

## User goal

Run `make build` in the current repository inside a clean `tmux` sandbox.
Wait until the build either prints `PASS` (success) or matches an error
pattern (`FAIL`, `error:`, `undefined reference`). Capture the final
screen so I can see the result, then kill the session.

## Expected tool sequence

1. `session_create` — name `"build"`, command `"/bin/sh"`, width `120`,
   height `40`, cwd set to the repo root.

   ```json
   {
     "name": "session_create",
     "arguments": {
       "name": "build",
       "command": "/bin/sh",
       "cwd": "/path/to/repo",
       "width": 120,
       "height": 40
     }
   }
   ```

2. `send_keys` — type the build command and press Enter.

   ```json
   {
     "name": "send_keys",
     "arguments": {
       "session": "build",
       "keys": ["make build", "Enter"]
     }
   }
   ```

3. `wait_for_text` — block until either a success or a failure pattern
   matches. Generous timeout (5 minutes) because builds are slow.

   ```json
   {
     "name": "wait_for_text",
     "arguments": {
       "session": "build",
       "pattern": "PASS|FAIL|error:|undefined reference",
       "timeout_ms": 300000
     }
   }
   ```

4. `capture` — grab the final visible pane to report back.

   ```json
   {
     "name": "capture",
     "arguments": {
       "session": "build",
       "mode": "visible"
     }
   }
   ```

5. `session_kill` — tear the sandbox down.

   ```json
   {
     "name": "session_kill",
     "arguments": { "name": "build" }
   }
   ```

If `wait_for_text` times out, fall through to `capture` anyway so the
human sees what the build was doing, then `session_kill`. Never leave a
session behind.
