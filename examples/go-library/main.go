// Command go-library demonstrates embedding tmux-mcp's controller
// inside another Go program via the public pkg/tmuxctl/ package.
//
// Run with:
//
//	cd examples/go-library && go run .
//
// The example:
//  1. starts a private tmux server,
//  2. creates a session,
//  3. types a command,
//  4. waits for the pane to settle,
//  5. prints what was captured,
//  6. kills the session.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/Kcrong/tmux-mcp/pkg/tmuxctl"
)

func main() {
	if err := run(); err != nil {
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}

func run() error {
	c, err := tmuxctl.New()
	if err != nil {
		return fmt.Errorf("tmuxctl.New: %w", err)
	}
	defer c.Shutdown(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const session = "demo"
	if err := c.CreateSession(ctx, tmuxctl.SessionSpec{
		Name:    session,
		Command: "/bin/sh",
		Width:   100,
		Height:  30,
	}); err != nil {
		return fmt.Errorf("CreateSession: %w", err)
	}

	if err := c.SendKeys(ctx, session,
		[]string{"echo hello-from-tmuxctl", "Enter"}, false); err != nil {
		return fmt.Errorf("SendKeys: %w", err)
	}

	body, err := c.WaitForStable(ctx, session,
		300*time.Millisecond, 100*time.Millisecond, 5*time.Second)
	if err != nil {
		return fmt.Errorf("WaitForStable: %w", err)
	}
	fmt.Println("--- captured pane ---")
	fmt.Println(body)
	fmt.Println("--- end ---")

	if err := c.KillSession(ctx, session); err != nil {
		return fmt.Errorf("KillSession: %w", err)
	}
	return nil
}
