// Package tmuxctl is the public Go API for driving a tmux session
// programmatically. It is the same controller the tmux-mcp binary uses
// under the hood, exposed for embedding in your own programs.
//
// # Quick start
//
//	c, err := tmuxctl.New()
//	if err != nil {
//		return err
//	}
//	defer c.Shutdown(context.Background())
//
//	ctx := context.Background()
//	if err := c.CreateSession(ctx, tmuxctl.SessionSpec{
//		Name: "demo", Command: "/bin/sh", Width: 100, Height: 30,
//	}); err != nil {
//		return err
//	}
//
//	if err := c.SendKeys(ctx, "demo", []string{"echo hello", "Enter"}, false); err != nil {
//		return err
//	}
//
//	body, err := c.WaitForStable(ctx, "demo",
//		300*time.Millisecond, 100*time.Millisecond, 5*time.Second)
//	if err != nil {
//		return err
//	}
//	fmt.Println(body)
//
// # Requirements
//
// tmux 3.0 or newer must be on PATH. Linux and macOS are supported;
// Windows is not (tmux itself does not run there).
//
// # API stability
//
// This package follows semver. Major version bumps signal a method
// signature change; minor versions add API surface; patch versions are
// bug fixes. The types exported here are aliases of the internal
// implementation, so the method set you see on godoc is the full
// supported surface.
//
// # Concurrency
//
// Each [Controller] owns a private tmux server (a private socket under
// MkdirTemp) so concurrent processes do not see one another's sessions.
// A single Controller is safe to call from multiple goroutines as long
// as you serialise calls that target the same session — tmux itself
// serialises commands per server.
package tmuxctl
