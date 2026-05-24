// Command tmux-mcp speaks MCP over stdio and exposes a private tmux
// server as a tool surface for an LLM agent.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/Kcrong/tmux-mcp/internal/server"
	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

// version is overridden at build time via -ldflags="-X main.version=...".
// It defaults to "dev" so a `go run` or unversioned `go install` build
// still has a sensible value to print.
var version = "dev"

const usage = `tmux-mcp — Model Context Protocol stdio server for tmux

Usage:
  tmux-mcp [flags]

The server reads JSON-RPC frames from stdin and writes responses to
stdout, one JSON object per line. It is meant to be launched by an MCP
client (Claude Desktop, an agent framework, etc.) — running it directly
in a terminal is only useful for smoke tests.

Flags:
  -version    print version and exit
  -help       print this message and exit

Smoke test:
  printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize"}' | tmux-mcp

Docs:  https://github.com/Kcrong/tmux-mcp
`

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "tmux-mcp:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tmux-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, usage) }
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected positional argument %q (run with -help)", fs.Arg(0))
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, versionString())
		return nil
	}

	ctl, err := tmuxctl.New()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	defer ctl.Shutdown(context.Background())

	tools := server.NewTools(ctl)
	return server.Serve(ctx, stdin, stdout, tools.Handle)
}

// versionString returns a human-readable version string. Prefers the
// ldflags-injected version when set, otherwise falls back to the module
// version embedded by `go install` / module-aware builds.
func versionString() string {
	if version != "" && version != "dev" {
		return "tmux-mcp " + version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return "tmux-mcp " + info.Main.Version
	}
	return "tmux-mcp dev"
}
