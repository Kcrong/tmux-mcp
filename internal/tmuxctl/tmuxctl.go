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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// Controller drives a private tmux server.
type Controller struct {
	socket string
	bin    string
	// configPath is the absolute path to a tmux.conf file that should be
	// loaded for every tmux invocation. Empty = no -f argument (tmux
	// uses its built-in defaults plus ~/.tmux.conf). Validated at
	// construction time so a misconfigured value fails fast rather than
	// poisoning every later tmux call.
	configPath string
	// ownsDir is true when the controller created its own scratch
	// directory under MkdirTemp and is therefore responsible for
	// removing it on Shutdown. When the caller supplied an explicit
	// socket path we leave the surrounding directory alone.
	ownsDir bool
}

// Option configures a [Controller] at construction time. The set is
// kept small on purpose — each option must justify itself by mapping
// onto an operator-visible CLI knob, not by exposing internal knobs
// for their own sake.
type Option func(*controllerOpts)

// controllerOpts is the mutable bag the option closures write into. It
// stays unexported so the option API is the only way to set a knob.
type controllerOpts struct {
	// bin overrides the tmux binary path. Empty = resolve "tmux" via
	// exec.LookPath (the historical behaviour). When non-empty it must
	// be an absolute path to an existing executable; validation happens
	// in [NewWithSocket] / [New] before any tmux command is dispatched.
	bin string
	// configPath pins the tmux.conf file passed to every tmux invocation
	// via `-f <path>`. Empty = no -f argument (tmux uses its built-in
	// defaults plus ~/.tmux.conf). When non-empty it must be an absolute
	// path to an existing regular file; validation happens in
	// [NewWithSocket] / [New] before any tmux command is dispatched.
	configPath string
}

// WithBinary pins the tmux executable the controller will invoke for
// every command. Pass an absolute path. Empty values are ignored so
// callers can forward a possibly-empty CLI flag without an extra
// branch — the controller falls back to [exec.LookPath]("tmux") in
// that case.
//
// The path is validated at construction time:
//   - must be absolute (relative paths are rejected up front for the
//     same reason the -socket flag rejects them — they hide where the
//     binary actually lives once the working directory shifts);
//   - must resolve to an existing executable file ([os.Stat] succeeds
//     and any of the executable bits is set);
//   - must satisfy the same minimum-version gate as the default path.
//
// A failed validation surfaces the path verbatim so the operator
// immediately sees which binary tmux-mcp tried to use.
func WithBinary(path string) Option {
	return func(o *controllerOpts) { o.bin = path }
}

// WithConfigPath pins a tmux.conf file the controller will load for
// every tmux invocation via `-f <path>`. Pass an absolute path. Empty
// values are ignored so callers can forward a possibly-empty CLI flag
// without an extra branch — the controller falls back to tmux's
// built-in defaults (and ~/.tmux.conf) in that case.
//
// The path is validated at construction time:
//   - must be absolute (relative paths are rejected up front for the
//     same reason the -socket and -tmux-bin flags reject them — they
//     hide where the file actually lives once the working directory
//     shifts);
//   - must resolve to an existing regular file ([os.Stat] succeeds and
//     the entry is not a directory or other irregular type).
//
// Validation runs at server start so a misconfiguration fails fast
// rather than poisoning every later tmux call. A failed validation
// surfaces the path verbatim so the operator immediately sees which
// file tmux-mcp tried to use.
func WithConfigPath(path string) Option {
	return func(o *controllerOpts) { o.configPath = path }
}

// New creates a Controller with a private tmux socket under a fresh
// MkdirTemp directory. The tmux server itself is started lazily by the
// first command. Variadic options apply on top of the defaults — the
// only one currently supported is [WithBinary].
func New(opts ...Option) (*Controller, error) {
	return NewWithSocket("", opts...)
}

// NewWithSocket creates a Controller. When socket is empty the
// behaviour matches [New] (a private temp directory is created and the
// socket lives inside it). When socket is non-empty it is used verbatim
// as the absolute path passed to `tmux -S`; the caller is responsible
// for ensuring the parent directory exists and is writable. The path
// must be absolute and its parent directory must already exist — this
// keeps systemd / container deployments explicit and refuses to
// silently create directories on behalf of the operator.
//
// Variadic options ([WithBinary], …) override the defaults derived
// from the environment.
func NewWithSocket(socket string, opts ...Option) (*Controller, error) {
	cfg := controllerOpts{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	bin, err := resolveTmuxBin(cfg.bin)
	if err != nil {
		return nil, err
	}
	// Validate the config path up front so a misconfigured -f surfaces
	// as a single clean diagnostic before any tmux command is dispatched
	// (otherwise every later run() would fail with the same error). The
	// empty default short-circuits the validation so the legacy
	// "tmux uses its own defaults / ~/.tmux.conf" behaviour stays
	// byte-identical for deployments that don't set the flag.
	configPath, err := resolveTmuxConfigPath(cfg.configPath)
	if err != nil {
		return nil, err
	}
	// Verify the tmux on PATH is new enough before doing any other work.
	// Older tmux silently rejects flags this package relies on.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = checkTmuxVersion(ctx, bin); err != nil {
		return nil, err
	}
	if socket != "" {
		if !filepath.IsAbs(socket) {
			return nil, fmt.Errorf(
				"socket path %q must be absolute "+
					"(e.g. /run/tmux-mcp/sock)",
				socket,
			)
		}
		parent := filepath.Dir(socket)
		info, statErr := os.Stat(parent)
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				return nil, fmt.Errorf(
					"socket parent directory %q does not exist — "+
						"create it before starting tmux-mcp "+
						"(e.g. `mkdir -p %s`)",
					parent, parent,
				)
			}
			return nil, fmt.Errorf("stat socket parent %q: %w", parent, statErr)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("socket parent %q is not a directory", parent)
		}
		return &Controller{bin: bin, socket: socket, configPath: configPath, ownsDir: false}, nil
	}
	dir, err := os.MkdirTemp("", "tmux-mcp-*")
	if err != nil {
		return nil, err
	}
	return &Controller{
		bin:        bin,
		socket:     filepath.Join(dir, "sock"),
		configPath: configPath,
		ownsDir:    true,
	}, nil
}

// resolveTmuxBin picks the tmux binary the controller will drive.
// override is the value coming from [WithBinary] / the -tmux-bin CLI
// flag; an empty string means "no operator override, use PATH".
//
// When override is empty the function preserves the historical
// behaviour: [exec.LookPath]("tmux") with the standard "install it
// first" diagnostic on failure.
//
// When override is non-empty the function runs the operator-facing
// validation contract documented on [WithBinary]: absolute path, file
// exists, file is executable. Failure surfaces the supplied path
// verbatim ("tmux binary %q ...") so the diagnostic immediately points
// at the wrong knob — there's no point hiding which path tmux-mcp
// actually tried to use.
func resolveTmuxBin(override string) (string, error) {
	if override == "" {
		bin, err := exec.LookPath("tmux")
		if err != nil {
			return "", fmt.Errorf(
				"tmux not found on PATH — install it first "+
					"(e.g. `apt-get install tmux`, `brew install tmux`): %w",
				err,
			)
		}
		return bin, nil
	}
	if !filepath.IsAbs(override) {
		return "", fmt.Errorf(
			"tmux binary %q must be absolute "+
				"(e.g. /usr/local/bin/tmux)",
			override,
		)
	}
	info, err := os.Stat(override)
	if err != nil {
		return "", fmt.Errorf("tmux binary %q not executable: %w", override, err)
	}
	// Reject a directory at the supplied path early — Stat would
	// otherwise succeed and the operator would only see the failure
	// when exec'ing the path returns an obscure permission error.
	if info.IsDir() {
		return "", fmt.Errorf("tmux binary %q not executable: is a directory", override)
	}
	// Any executable bit (owner / group / world) is enough — we don't
	// know which uid will run the binary, only that it must be
	// executable for someone. Mirrors the spirit of os.Stat-based
	// "is this a runnable file" checks throughout stdlib.
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("tmux binary %q not executable: mode %s", override, info.Mode().Perm())
	}
	return override, nil
}

// resolveTmuxConfigPath validates the operator-supplied -f argument
// before any tmux invocation. override is the value coming from
// [WithConfigPath] / the -tmux-config-path CLI flag; an empty string
// means "no operator override, use tmux's built-in defaults plus
// ~/.tmux.conf" — the empty default flows through unchanged.
//
// When override is non-empty the function runs the operator-facing
// validation contract documented on [WithConfigPath]: absolute path,
// path exists, path is a regular file (not a directory). Failure
// surfaces the supplied path verbatim ("tmux config path %q ...") so
// the diagnostic immediately points at the wrong knob — there's no
// point hiding which file tmux-mcp actually tried to load.
func resolveTmuxConfigPath(override string) (string, error) {
	if override == "" {
		return "", nil
	}
	if !filepath.IsAbs(override) {
		return "", fmt.Errorf(
			"tmux config path %q must be absolute "+
				"(e.g. /etc/tmux-mcp/tmux.conf)",
			override,
		)
	}
	info, err := os.Stat(override)
	if err != nil {
		return "", fmt.Errorf("tmux config path %q: %w", override, err)
	}
	// Reject a directory at the supplied path early — a directory is
	// never a valid tmux.conf, and tmux's own error for `-f <dir>`
	// ("file is a directory") only surfaces at command time, far from
	// the operator's mistake.
	if info.IsDir() {
		return "", fmt.Errorf("tmux config path %q is a directory, not a file", override)
	}
	// Mode().IsRegular() rejects symlinks too in principle, but Stat
	// already followed the symlink, so this only refuses pipes /
	// sockets / device nodes — bizarre paths for a tmux.conf that
	// would only confuse tmux at command time.
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("tmux config path %q is not a regular file (mode %s)", override, info.Mode())
	}
	return override, nil
}

// Socket returns the absolute path tmux is talking to via `-S`. Useful
// for diagnostics and tests that want to assert the controller honoured
// an explicit socket path.
func (c *Controller) Socket() string { return c.socket }

// ConfigPath returns the absolute tmux.conf path the controller
// injects into every tmux invocation via `-f`, or the empty string
// when no override is configured. Useful for diagnostics and tests
// that want to assert the controller picked up [WithConfigPath].
func (c *Controller) ConfigPath() string { return c.configPath }

// Shutdown kills the entire private tmux server. When the controller
// owns its scratch directory (the [New] case) it is also removed.
func (c *Controller) Shutdown(ctx context.Context) {
	_, _ = c.run(ctx, "kill-server")
	if c.ownsDir {
		_ = os.RemoveAll(filepath.Dir(c.socket))
		return
	}
	// Caller-supplied paths: only clean up the socket file we created.
	// Leave the parent directory (which they manage) alone.
	_ = os.Remove(c.socket)
}

// isSessionMissingMsg reports whether stderr text from tmux indicates the
// targeted session does not exist. tmux phrases this several ways across
// versions ("can't find session", "session not found", "no current
// session", "session not found:foo"); detect them all so callers can
// rely on errs.ErrSessionNotFound regardless of the version on PATH.
func isSessionMissingMsg(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "can't find session") ||
		strings.Contains(m, "session not found") ||
		strings.Contains(m, "no current session") ||
		strings.Contains(m, "no such session")
}

func (c *Controller) run(ctx context.Context, args ...string) (string, error) {
	// -S takes an absolute socket path (whereas -L names a socket inside
	// /tmp/tmux-<uid>/). We control the path explicitly so multiple
	// servers can coexist on the same host. -f, when configured, must
	// also precede the subcommand verb — tmux parses "server flags" in
	// argv up to the first non-flag token, so injecting them between
	// the binary and the verb is the only argv shape tmux accepts.
	full := make([]string, 0, 4+len(args))
	full = append(full, "-S", c.socket)
	if c.configPath != "" {
		full = append(full, "-f", c.configPath)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, c.bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		// Surface "session not found" as a typed sentinel so the JSON-RPC
		// layer can map it to a stable code; clients can then reliably
		// switch on the code instead of substring-matching the message.
		if isSessionMissingMsg(msg) {
			return "", fmt.Errorf("tmux %s: %s: %w",
				strings.Join(args, " "), msg, errs.ErrSessionNotFound)
		}
		return "", fmt.Errorf("tmux %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// SessionSpec describes a session to create.
type SessionSpec struct {
	Name    string            // tmux session name; required.
	Command string            // initial command to run; empty starts the user's default shell.
	Cwd     string            // working directory for the new session.
	Width   int               // pane width in columns; 0 falls back to a sensible default.
	Height  int               // pane height in rows; 0 falls back to a sensible default.
	Env     map[string]string // extra environment variables passed to tmux via -e.
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
		// fails to connect at all. The "server exited unexpectedly"
		// phrase is the third variant: it surfaces when a list-sessions
		// races a kill-server and the client noticed the daemon
		// disappeared mid-call. All four phrases mean the same thing
		// at this layer: zero sessions. Treat them uniformly so callers
		// (kill_server's snapshot bookkeeping, agent recovery loops)
		// don't have to substring-match tmux's version-dependent stderr.
		msg := err.Error()
		if strings.Contains(msg, "no server running") ||
			strings.Contains(msg, "error connecting") ||
			strings.Contains(msg, "server exited unexpectedly") ||
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

// Capture areas.
const (
	// CaptureVisible captures only the currently visible region of the pane.
	CaptureVisible CaptureMode = iota
	// CaptureScrollback captures the visible region plus all scrollback.
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
			// Wrap ErrTimeout so callers can switch on errors.Is and the
			// JSON-RPC layer can map this to CodeTimeout (-32002).
			return last, fmt.Errorf("wait_for_stable: timed out after %s: %w", timeout, errs.ErrTimeout)
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
