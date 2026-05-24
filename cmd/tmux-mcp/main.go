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
  -log-format FMT         slog output format: text|json. When unset, the
                          server emits text by default and switches to json
                          automatically when -log-level=debug. Passing this
                          flag explicitly overrides that auto-switch.
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
	// Bad CLI usage (currently: invalid -log-format) exits with status
	// 2, matching the convention used by stdlib `flag` and most Unix
	// utilities. The validator already wrote a single-line diagnostic
	// to stderr, so we don't need to log again here.
	if errors.Is(err, errInvalidLogFormat) {
		os.Exit(2)
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
	logFormatRaw := fs.String("log-format", "text", "slog output format: text|json (debug auto-promotes to json when this flag is not set)")
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
	// fs.Visit only reports flags that the user actually passed, so this
	// distinguishes "operator picked text" from "took the default" — we
	// need that to keep the legacy debug→json auto-switch working only
	// when the operator has not opted in to an explicit format.
	formatExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "log-format" {
			formatExplicit = true
		}
	})
	format, err := resolveLogFormat(*logFormatRaw, lvl, formatExplicit)
	if err != nil {
		// stderr lands cleanly on a dedicated diagnostic line; stdout
		// stays untouched so an MCP client parsing JSON-RPC from it
		// won't see a stray non-frame. main() maps errInvalidLogFormat
		// to exit code 2.
		_, _ = fmt.Fprintf(stderr, "tmux-mcp: %s\n", err)
		return err
	}
	// All structured logs go to stderr — stdout is reserved for the
	// line-delimited JSON-RPC frames the MCP client consumes.
	slog.SetDefault(slog.New(newLogHandler(stderr, lvl, format)))

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

// errInvalidLogFormat is the sentinel returned by [resolveLogFormat]
// when the user passes a value other than "text" or "json" to
// -log-format. main() recognises it via [errors.Is] and maps it to
// exit code 2 — the conventional "CLI usage error" status used by the
// stdlib `flag` package and most Unix tools.
var errInvalidLogFormat = errors.New("invalid -log-format")

// logFormat is the small string enum carried by the -log-format flag.
// Keeping it a typed string (rather than a bool) lets future additions
// — say, "logfmt" — plug in without rippling through every call site.
type logFormat string

const (
	logFormatText logFormat = "text"
	logFormatJSON logFormat = "json"
)

// resolveLogFormat decides which slog handler the server should install
// based on the parsed -log-format flag value, the resolved log level,
// and whether the operator passed -log-format explicitly.
//
// Rules:
//   - explicit "text" / "json"  → that format wins, regardless of level.
//   - implicit (default "text") → "json" iff lvl == debug, else "text".
//     This preserves the legacy "debug logs are JSON" affordance for
//     people who never touch the flag.
//   - any other value → returns a wrapped errInvalidLogFormat so main()
//     can report it cleanly and exit 2.
func resolveLogFormat(raw string, lvl slog.Level, explicit bool) (logFormat, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "text":
		if !explicit && lvl == slog.LevelDebug {
			return logFormatJSON, nil
		}
		return logFormatText, nil
	case "json":
		return logFormatJSON, nil
	}
	return "", fmt.Errorf("%w %q (want text|json)", errInvalidLogFormat, raw)
}

// newLogHandler returns the slog handler matching the resolved format.
// It always writes to the supplied writer (stderr in production) so
// stdout stays reserved for JSON-RPC frames.
func newLogHandler(w io.Writer, lvl slog.Level, format logFormat) slog.Handler {
	opts := &slog.HandlerOptions{Level: lvl}
	if format == logFormatJSON {
		return slog.NewJSONHandler(w, opts)
	}
	return slog.NewTextHandler(w, opts)
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
