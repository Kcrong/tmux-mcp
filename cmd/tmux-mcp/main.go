// Command tmux-mcp speaks MCP over stdio and exposes a private tmux
// server as a tool surface for an LLM agent.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Kcrong/tmux-mcp/internal/server"
	"github.com/Kcrong/tmux-mcp/internal/tmuxctl"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tmux-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	ctl, err := tmuxctl.New()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	defer ctl.Shutdown(context.Background())

	tools := server.NewTools(ctl)
	return server.Serve(ctx, os.Stdin, os.Stdout, tools.Handle)
}
