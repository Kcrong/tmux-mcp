// Package tmuxctl is a thin wrapper around the tmux CLI.
//
// Each Controller owns a private tmux server (selected with -L <socket>)
// so concurrent processes don't see each other's sessions.
package tmuxctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Controller drives a private tmux server.
type Controller struct {
	socket string
	bin    string
}

// New creates a Controller bound to a freshly named socket. The tmux
// server itself is started lazily by the first command.
func New() (*Controller, error) {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return nil, fmt.Errorf(
			"tmux not found on PATH — install it first "+
				"(e.g. `apt-get install tmux`, `brew install tmux`): %w",
			err,
		)
	}
	dir, err := os.MkdirTemp("", "tmux-mcp-*")
	if err != nil {
		return nil, err
	}
	return &Controller{
		bin:    bin,
		socket: filepath.Join(dir, "sock"),
	}, nil
}

// Shutdown kills the entire private tmux server.
func (c *Controller) Shutdown(ctx context.Context) {
	_, _ = c.run(ctx, "kill-server")
	_ = os.RemoveAll(filepath.Dir(c.socket))
}

func (c *Controller) run(ctx context.Context, args ...string) (string, error) {
	// -S takes an absolute socket path (whereas -L names a socket inside
	// /tmp/tmux-<uid>/). We control the path explicitly so multiple
	// servers can coexist on the same host.
	full := append([]string{"-S", c.socket}, args...)
	cmd := exec.CommandContext(ctx, c.bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("tmux %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// SessionSpec describes a session to create.
type SessionSpec struct {
	Name    string
	Command string
	Cwd     string
	Width   int
	Height  int
	Env     map[string]string
}

// CreateSession starts a new detached session.
func (c *Controller) CreateSession(ctx context.Context, s SessionSpec) error {
	if s.Name == "" {
		return errors.New("session name required")
	}
	if s.Width == 0 {
		s.Width = 120
	}
	if s.Height == 0 {
		s.Height = 40
	}
	args := []string{
		"new-session", "-d",
		"-s", s.Name,
		"-x", strconv.Itoa(s.Width),
		"-y", strconv.Itoa(s.Height),
	}
	if s.Cwd != "" {
		args = append(args, "-c", s.Cwd)
	}
	for k, v := range s.Env {
		args = append(args, "-e", k+"="+v)
	}
	if s.Command != "" {
		args = append(args, s.Command)
	}
	_, err := c.run(ctx, args...)
	return err
}

// KillSession kills a single session.
func (c *Controller) KillSession(ctx context.Context, name string) error {
	_, err := c.run(ctx, "kill-session", "-t", name)
	return err
}

// ListSessions returns the names of all sessions on this controller's
// tmux server. Returns an empty slice (no error) when the server has
// not been started yet.
func (c *Controller) ListSessions(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		// Either tmux exited cleanly with "no server running on ...",
		// or — when the socket file does not yet exist — the client
		// fails to connect at all. Both cases just mean "zero sessions".
		msg := err.Error()
		if strings.Contains(msg, "no server running") ||
			strings.Contains(msg, "error connecting") ||
			strings.Contains(msg, "No such file or directory") {
			return nil, nil
		}
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// HasSession returns true if the named session exists.
func (c *Controller) HasSession(ctx context.Context, name string) (bool, error) {
	names, err := c.ListSessions(ctx)
	if err != nil {
		return false, err
	}
	for _, n := range names {
		if n == name {
			return true, nil
		}
	}
	return false, nil
}

// SendKeys sends keys to the session's active pane. Each key string is
// passed to `tmux send-keys` as a separate argument; tmux interprets
// names like "Up", "Enter", "C-c" specially. When literal is true the
// keys are sent verbatim with `-l`, suppressing key-name interpretation.
func (c *Controller) SendKeys(ctx context.Context, session string, keys []string, literal bool) error {
	if session == "" {
		return errors.New("session required")
	}
	if len(keys) == 0 {
		return nil
	}
	args := []string{"send-keys", "-t", session}
	if literal {
		args = append(args, "-l")
	}
	args = append(args, keys...)
	_, err := c.run(ctx, args...)
	return err
}

// CaptureMode selects the area to capture.
type CaptureMode int

const (
	CaptureVisible CaptureMode = iota
	CaptureScrollback
)

// Capture returns the current pane contents. If ansi is true the output
// preserves escape sequences via `tmux capture-pane -e`.
func (c *Controller) Capture(ctx context.Context, session string, mode CaptureMode, ansi bool) (string, error) {
	if session == "" {
		return "", errors.New("session required")
	}
	args := []string{"capture-pane", "-p", "-t", session}
	if ansi {
		args = append(args, "-e")
	}
	if mode == CaptureScrollback {
		// -S - includes the entire scrollback up to the visible region.
		args = append(args, "-S", "-")
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return "", err
	}
	return out, nil
}

// Resize changes the pane size.
func (c *Controller) Resize(ctx context.Context, session string, width, height int) error {
	if session == "" {
		return errors.New("session required")
	}
	if width <= 0 || height <= 0 {
		return fmt.Errorf("width and height must be positive (got %dx%d)", width, height)
	}
	_, err := c.run(ctx, "resize-window", "-t", session,
		"-x", strconv.Itoa(width), "-y", strconv.Itoa(height))
	return err
}

// WaitForStable polls the visible pane until it has not changed for
// quiet, then returns the final snapshot. Polls every step. Aborts when
// ctx is cancelled or when total elapsed time exceeds timeout.
func (c *Controller) WaitForStable(ctx context.Context, session string, quiet, step, timeout time.Duration) (string, error) {
	if step <= 0 {
		step = 100 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	last, err := c.Capture(ctx, session, CaptureVisible, false)
	if err != nil {
		return "", err
	}
	stableSince := time.Now()
	for {
		if time.Since(stableSince) >= quiet {
			return last, nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return last, fmt.Errorf("wait_for_stable: timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(step):
		}
		cur, err := c.Capture(ctx, session, CaptureVisible, false)
		if err != nil {
			return last, err
		}
		if cur != last {
			last = cur
			stableSince = time.Now()
		}
	}
}
