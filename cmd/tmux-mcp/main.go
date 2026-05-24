// Command tmux-mcp speaks MCP over stdio and exposes a private tmux
// server as a tool surface for an LLM agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

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
  -version                print version and exit
  -version-json           print version metadata as JSON and exit
  -help                   print this message and exit
  -probe                  run a startup health check (verify tmux + version)
                          and exit. Prints "ok\ttmux=<v>\ttmux-mcp=<v>" on
                          success; non-zero exit + stderr diagnostic on
                          failure. Useful for k8s liveness, systemd
                          ExecStartPre, Docker HEALTHCHECK.
  -log-level LEVEL        log verbosity: error|warn|info|debug (default "info")
  -socket PATH            absolute path for the private tmux socket
                          (also TMUX_MCP_SOCKET env var; flag wins).
                          Default: a fresh directory under $TMPDIR.
  -max-concurrent-calls N cap simultaneously-executing tools/call frames
                          (default 64). Excess callers wait — back-pressure
                          rather than failure. 0 disables the cap (unbounded
                          goroutines, original behaviour).
  -audit-log PATH         when set, write one JSONL audit record per
                          tools/call. Use "stderr" to share the slog
                          stream, or any other value as a file path
                          (opened append-only at mode 0600).
                          Records carry args_size_bytes only — never
                          args content. Default: disabled.

Smoke test:
  printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize"}' | tmux-mcp

Docs:  https://github.com/Kcrong/tmux-mcp
`

func main() {
	err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	if err == nil {
		return
	}
	// The -probe path has already written a "probe failed: …" line to
	// stderr; logging the error again via slog would just duplicate it.
	// Every other failure mode goes through slog so it shows up in the
	// structured log stream a supervisor is likely scraping.
	if !errors.Is(err, errProbeFailed) {
		// Logger may or may not be initialised yet (e.g. flag parsing
		// failed). slog falls back to a default text handler on stderr,
		// which is fine — stdout is reserved for JSON-RPC frames.
		slog.Error("startup failed", "err", err)
	}
	os.Exit(1)
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tmux-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, usage) }
	showVersion := fs.Bool("version", false, "print version and exit")
	versionJSONFlag := fs.Bool("version-json", false, "print version metadata as JSON and exit")
	probe := fs.Bool("probe", false,
		"run a startup health check (verify tmux + version) and exit")
	logLevel := fs.String("log-level", "info", "log verbosity: error|warn|info|debug")
	// Default to the env var so systemd / container deployments can
	// pin a known socket path without rewriting argv. The flag, when
	// passed, wins.
	socket := fs.String("socket", os.Getenv("TMUX_MCP_SOCKET"),
		"absolute path for the private tmux socket "+
			"(env TMUX_MCP_SOCKET; default: fresh tempdir)")
	// 64 is a generous default for an interactive single-agent client
	// (Claude Desktop typically runs 1–4 tools in parallel) while still
	// putting a ceiling on goroutines a misbehaving / flooding client
	// can spawn. Operators who genuinely want unbounded behaviour can
	// pass -max-concurrent-calls=0.
	maxConcurrentCalls := fs.Int("max-concurrent-calls", 64,
		"cap simultaneously-executing tools/call frames; 0 disables")
	// Empty default keeps the audit log opt-in: existing deployments
	// see no behaviour change. "stderr" is a magic path that shares
	// the slog stream; any other value is a filesystem path.
	auditLog := fs.String("audit-log", "",
		"path for JSONL audit records (\"stderr\" or a file path; default: disabled)")
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
	if *versionJSONFlag {
		return emitVersionJSON(stdout, version, runtime.Version())
	}
	if *probe {
		return runProbe(stdout, stderr)
	}

	lvl, err := parseLogLevel(*logLevel)
	if err != nil {
		return err
	}
	// All structured logs go to stderr — stdout is reserved for the
	// line-delimited JSON-RPC frames the MCP client consumes.
	slog.SetDefault(slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: lvl})))

	ctl, err := tmuxctl.NewWithSocket(*socket)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	defer ctl.Shutdown(context.Background())

	// Open the audit sink before constructing the server so we surface
	// path/permission problems as a clean startup error instead of
	// half-running with a broken sink. OpenAudit returns (nil, nil) for
	// the disabled case so audit stays a true no-op when the flag is
	// not set.
	audit, err := server.OpenAudit(*auditLog, stderr)
	if err != nil {
		return err
	}
	defer func() { _ = audit.Close() }()

	tools := server.NewTools(ctl)
	// Propagate the ldflags-injected binary version so MCP clients see
	// the same value the -version flag prints, instead of a hardcoded
	// constant inside the server package.
	tools.Version = version
	return server.Serve(ctx, stdin, stdout, tools.Handle,
		server.WithMaxConcurrentCalls(*maxConcurrentCalls),
		server.WithAudit(audit),
	)
}

// parseLogLevel maps the -log-level flag value onto a slog.Level.
func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return slog.LevelError, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	}
	return 0, fmt.Errorf("invalid -log-level %q (want error|warn|info|debug)", s)
}

// versionString returns a human-readable version string. Prefers the
// ldflags-injected version when set, otherwise falls back to the module
// version embedded by `go install` / module-aware builds.
func versionString() string {
	return "tmux-mcp " + binaryVersion()
}

// binaryVersion returns the bare version string (no leading "tmux-mcp ")
// so callers like the -probe path can embed it in machine-readable
// output. Same precedence as [versionString]: ldflags wins, then
// debug.ReadBuildInfo, then "dev".
func binaryVersion() string {
	if version != "" && version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// errProbeFailed is the sentinel returned from [runProbe] when the
// startup health check fails. main() uses [errors.Is] to recognise it
// and skip the structured-log error message — the probe path already
// wrote a "probe failed: …" diagnostic to stderr and we don't want to
// surface the same failure twice.
var errProbeFailed = errors.New("probe failed")

// runProbe is the body of the -probe flag. It probes tmux on PATH (looks
// it up, runs `tmux -V`, checks the minimum version) and writes a single
// tab-delimited "ok" line to stdout when everything is healthy:
//
//	ok\ttmux=<tmux-version>\ttmux-mcp=<binary-version>\n
//
// On failure it writes a "probe failed: …" diagnostic to stderr and
// returns an error wrapping [errProbeFailed] so the caller can map it
// to a non-zero exit code. Stdout is left untouched on the failure path
// so orchestrators can rely on stdout being empty when probing failed.
func runProbe(stdout, stderr io.Writer) error {
	// 5s is generous: `tmux -V` is essentially instant. A timeout
	// keeps a wedged binary on a misconfigured PATH from hanging the
	// liveness check forever.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tmuxVer, err := tmuxctl.ProbeVersion(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "probe failed: %s\n", err)
		return fmt.Errorf("%w: %w", errProbeFailed, err)
	}
	_, _ = fmt.Fprintf(stdout, "ok\ttmux=%s\ttmux-mcp=%s\n", tmuxVer, binaryVersion())
	return nil
}
