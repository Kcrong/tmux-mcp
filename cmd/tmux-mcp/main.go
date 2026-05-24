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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/Kcrong/tmux-mcp/internal/server"
	"github.com/Kcrong/tmux-mcp/internal/snapshot"
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
  -dry-run                perform full startup (parse flags, validate paths,
                          init tmux controller, open audit sink, build the
                          tool surface), then exit 0 without reading stdin.
                          Prints "dry-run ok\ttmux=<v>\ttmux-mcp=<v>" on
                          success. Useful for unit-test config / liveness
                          check before swapping in a real config (systemd
                          ExecStartPre, Claude Desktop config dry-test, env
                          var validation).
  -log-level LEVEL        log verbosity: error|warn|info|debug (default "info")
  -log-format FMT         slog output format: text|json. When unset, the
                          server emits text by default and switches to json
                          automatically when -log-level=debug. Passing this
                          flag explicitly overrides that auto-switch.
  -log-source             include file:line of the call site in each log
                          record (slight perf cost). Default: disabled.
                          When enabled, JSON records gain a "source" object
                          ({"function","file","line"}); text records gain a
                          "source=…" key. Useful for ad-hoc debugging where
                          you need to grep a log line back to the exact
                          slog.Info call that produced it.
  -log-output PATH        destination for slog output: "stderr" (default),
                          "stdout" (DANGER — corrupts JSON-RPC frames; only
                          useful with -dry-run / -version), or a file path
                          (opened append-only at mode 0600). The file is
                          closed cleanly on shutdown. By default tmux-mcp
                          does not rotate the file — pair it with
                          logrotate(8) or pass -log-rotate-size for an
                          in-process size-based rotator.
  -log-rotate-size N      enable size-based rotation on -log-output. When
                          the next Write would push the file past N bytes,
                          tmux-mcp renames the live file to
                          <path>.<unix-ns> and reopens a fresh <path> in
                          place. Default: 0 (disabled — preserves the
                          legacy "open once, never rotate" behaviour for
                          deployments that pair with logrotate(8)).
                          Counted in bytes — e.g. 10485760 for 10MB.
  -log-rotate-keep K      maximum count of rotated archive files to keep
                          on disk alongside -log-output (default 5). After
                          a rollover, the oldest archives (by mtime) are
                          deleted once the count exceeds K. Ignored when
                          -log-rotate-size=0.
  -socket PATH            absolute path for the private tmux socket
                          (also TMUX_MCP_SOCKET env var; flag wins).
                          Default: a fresh directory under $TMPDIR.
  -tmux-bin PATH          absolute path to the tmux executable to invoke
                          (also TMUX_MCP_TMUX_BIN env var; flag wins).
                          Empty default = resolve tmux from $PATH (the
                          historical behaviour). When set the path must
                          be absolute and point at an existing executable
                          file, otherwise startup fails with "tmux binary
                          PATH not executable: ...". Useful for pinning
                          a specific tmux version on Nix / Homebrew /
                          static builds, sandboxes, and containers where
                          multiple tmux versions live side-by-side.
  -tmux-config-path PATH  absolute path to a tmux.conf file to load for
                          every session this server creates (also
                          TMUX_MCP_TMUX_CONFIG_PATH env var; flag wins).
                          When set the controller injects "-f <path>"
                          into every tmux invocation, so options the
                          file declares (e.g. set -g escape-time 17)
                          take effect without touching ~/.tmux.conf.
                          The path must be absolute and point at an
                          existing regular file, otherwise startup
                          fails with "tmux config path PATH ..." — a
                          single clean diagnostic instead of poisoning
                          every later tmux call. Empty default = no
                          -f argument (tmux uses its built-in defaults
                          plus ~/.tmux.conf, the historical behaviour).
                          Useful for testing against a known-good
                          tmux.conf, vendoring agent-friendly defaults
                          (e.g. low escape-time, large history-limit)
                          alongside the binary, or running multiple
                          tmux-mcp instances with different option
                          sets on the same host.
  -max-concurrent-calls N cap simultaneously-executing tools/call frames
                          (default 64). Excess callers wait — back-pressure
                          rather than failure. 0 disables the cap (unbounded
                          goroutines, original behaviour).
  -max-response-bytes N   hard ceiling on the marshalled JSON-RPC response
                          body (in bytes) before it is written to stdout
                          (default 0 = disabled, original behaviour). When
                          a reply exceeds the cap, the server replaces it
                          with a typed JSON-RPC error (code -32010, message
                          "response body N bytes exceeds max-response-bytes
                          M") so a misbehaving tool (capture_pane on a 10MB
                          scrollback, etc.) cannot dump a multi-megabyte
                          frame onto an MCP client whose reader can't
                          tolerate it. Clients see the error — not a
                          truncated payload. The underlying tools/call still
                          ran (its audit + metrics records fire with the
                          oversize sentinel) so operators can distinguish
                          "the tool failed" from "the answer was too big".
  -audit-log PATH         when set, write one JSONL audit record per
                          tools/call. Use "stderr" to share the slog
                          stream, or any other value as a file path
                          (opened append-only at mode 0600).
                          Records carry args_size_bytes only — never
                          args content. Default: disabled.
  -snapshot-ttl D         maximum idle time a session's snapshot history may
                          sit in memory before it is pruned (default 1h).
                          A value of 0 disables cleanup entirely (history is
                          only released when the session is killed). Accepts
                          any Go duration: 30s, 5m, 2h, …
  -shutdown-timeout DUR   on SIGTERM/SIGINT, wait up to DUR for in-flight
                          tools/call handlers to finish writing their
                          JSON-RPC responses before exiting (default 5s).
                          Set to 0 to disable the drain (exit immediately,
                          abandoning in-flight responses). On timeout the
                          binary exits non-zero so supervisors can flag a
                          forced shutdown.
  -session-idle-timeout D auto-kill any session that has had no tool-call
                          activity for at least D (default 0 = disabled).
                          Activity is any tools/call referencing the
                          session by name; session_list and
                          kill_all_sessions are explicitly excluded so
                          they cannot extend an idle session's lifetime.
                          Negative values are rejected at startup.
  -metrics-addr ADDR      when set, expose Prometheus metrics on the
                          given listen address (e.g. "127.0.0.1:9090"
                          or ":9090"). The exporter publishes
                          tmuxmcp_tools_call_total,
                          tmuxmcp_tools_call_duration_seconds, and
                          tmuxmcp_sessions_active at /metrics. The same
                          listener also serves GET /healthz (200 "ok"
                          once the startup tmux probe succeeds, 503
                          "unhealthy" before; 405 on non-GET) so k8s
                          liveness/readiness and load balancers can
                          reuse the port.
                          Default: "" (exporter disabled, no HTTP
                          listener opened).
  -pprof                  when set, mount net/http/pprof handlers under
                          /debug/pprof/ on the metrics listener (Index,
                          cmdline, profile, symbol, trace, plus the
                          goroutine/heap/allocs/block/mutex/threadcreate
                          runtime profiles). Requires -metrics-addr to be
                          set; startup fails otherwise. Disabled by
                          default because heap and goroutine profiles can
                          leak in-memory state — only enable on a
                          loopback / private listener you trust to
                          access. Default: false.
  -pid-file PATH          when set, atomically write the server PID as a
                          single decimal line to PATH on startup
                          (mode 0644) and remove it on graceful
                          shutdown. Startup fails if PATH already
                          exists, so two instances cannot silently
                          clobber each other — operators can rm the
                          stale file manually if the previous run
                          died. Default: "" (no pid file written).
                          Useful for systemd PIDFile=, supervisord,
                          runit, or k8s preStop hooks.
  -allowlist NAMES        comma-separated list of tool names; when set,
                          tools/list omits every other tool and
                          tools/call rejects them with -32601
                          (methodNotFound). Unknown names abort
                          startup with "unknown tools in -allowlist:
                          …" so a typo is caught before the JSON-RPC
                          loop opens. Default: "" (no filter — every
                          registered tool is exposed, the original
                          behaviour). Useful for least-privilege
                          deployments: pass e.g.
                          -allowlist=capture,wait_for_text for a
                          read-only inspector, or omit destructive
                          tools (kill_all_sessions, pane_kill,
                          session_kill, send_signal) for untrusted
                          contexts.
  -session-prefix STRING  when set, every session this server creates
                          lands on tmux as "<prefix><name>", and every
                          session-bearing tool (capture, send_keys,
                          session_kill, …) resolves the bare name back
                          to the prefixed identity transparently.
                          session_list and kill_all_sessions are
                          scoped to the prefix and strip it from the
                          response, so a co-tenant agent's sessions
                          (created with a different prefix or none)
                          stay invisible to this instance. The prefix
                          must match [A-Za-z0-9_-]+, may not end with
                          '-', and must leave at least one byte for a
                          user-supplied session name (combined length
                          ≤ 64). Default: "" (no prefix — the
                          original single-tenant behaviour). Useful
                          when several agents share one tmux-mcp
                          server and need disjoint session namespaces.
  -read-only              reject every tools/call whose tool would
                          mutate tmux state. Only inspection tools
                          (capture, wait_for_text, session_list,
                          list_panes, list_windows, list_clients,
                          list_keys, display_message, session_describe,
                          session_inspect, plus the spec-named aliases
                          capture_pane / list_sessions / list_buffers /
                          show_buffer / show_options / show_message)
                          are dispatched; everything else (send_keys,
                          session_create, session_kill, kill_all_sessions,
                          pane_*, window_*, clear_history, send_signal,
                          session_rename, …) is rejected with a typed
                          JSON-RPC error (code -32011, message
                          "tool 'X' is rejected: server in read-only
                          mode"). tools/list still returns every
                          registered tool so a constrained agent can
                          enumerate the full surface; only tools/call
                          is gated. The audit + metrics records still
                          fire on rejected calls so operators see the
                          blocked attempts in their dashboards. Useful
                          as a safer default for novice agents or
                          production diagnostics where the LLM should
                          only DIAGNOSE a session and never modify it.
                          Default: false (the dispatcher dispatches
                          every registered tool, the original behaviour).

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
	// Same exit-code convention for a malformed -session-idle-timeout
	// (currently: a negative duration). The diagnostic was already
	// written to stderr by the validator, so we don't double-log.
	if errors.Is(err, errInvalidIdleTimeout) {
		os.Exit(2)
	}
	// Same convention for an invalid -session-prefix (bad regex, trailing
	// dash, or no room left for a session name). The validator already
	// wrote a single-line diagnostic to stderr.
	if errors.Is(err, errInvalidSessionPrefix) {
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

// errInvalidIdleTimeout is the sentinel returned when -session-idle-timeout
// receives a value the run path can't accept (currently: any strictly
// negative duration). main() recognises it via [errors.Is] and maps it
// to exit code 2 — the conventional "CLI usage error" status.
var errInvalidIdleTimeout = errors.New("invalid -session-idle-timeout")

// errInvalidSessionPrefix is the sentinel returned when -session-prefix
// receives a value [server.ValidateSessionPrefix] rejects: anything
// outside the [A-Za-z0-9_-]+ regex, a value ending in '-', or a prefix
// long enough to leave no room for a user-supplied session name within
// the 64-byte tmux session-name budget. main() recognises it via
// [errors.Is] and maps it to exit code 2 — the conventional "CLI usage
// error" status used by stdlib `flag` and most Unix utilities.
var errInvalidSessionPrefix = errors.New("invalid -session-prefix")

// errPprofRequiresMetricsAddr is the sentinel returned when -pprof is
// enabled without -metrics-addr. The pprof handlers are co-located on
// the metrics listener by design (single bind address, single shutdown
// handle, single audit point for what the operator deliberately
// exposed), so there is no surface to mount them on if -metrics-addr is
// empty. Failing fast at startup is the right contract — silently
// ignoring -pprof would leave operators thinking they had pprof when
// they didn't, and auto-binding a default port behind their back would
// expose a sensitive endpoint they never asked for. main() recognises
// this via [errors.Is] but does not (currently) special-case the exit
// code; the wrapped run() error is enough.
var errPprofRequiresMetricsAddr = errors.New("-pprof requires -metrics-addr to be set")

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tmux-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, usage) }
	showVersion := fs.Bool("version", false, "print version and exit")
	versionJSONFlag := fs.Bool("version-json", false, "print version metadata as JSON and exit")
	probe := fs.Bool("probe", false,
		"run a startup health check (verify tmux + version) and exit")
	// dry-run goes further than -probe: it walks the entire startup
	// path (slog handler install, tmux controller init, audit open,
	// tool registry build) and exits cleanly *before* server.Serve
	// touches stdin. That way operators can validate a unit-file or
	// MCP-client config end-to-end without committing to the JSON-RPC
	// loop. Defers (ctl.Shutdown, audit.Close) still fire, so any
	// resource the bootstrap acquired is released before we exit.
	dryRun := fs.Bool("dry-run", false,
		"perform full startup, then exit 0 without reading stdin")
	logLevel := fs.String("log-level", "info", "log verbosity: error|warn|info|debug")
	logFormatRaw := fs.String("log-format", "text", "slog output format: text|json (debug auto-promotes to json when this flag is not set)")
	// Off by default — AddSource walks runtime.Callers on every record
	// and inflates structured-log volume, so we keep the legacy zero-cost
	// path the default and let operators opt in when investigating.
	logSource := fs.Bool("log-source", false,
		"include file:line of the call site in each log record (slight perf cost)")
	// "stderr" preserves the legacy behaviour so existing deployments
	// see no behaviour change. "stdout" is a magic escape hatch
	// (debugging with -dry-run); any other value is a filesystem path
	// opened append-only at mode 0600. tmux-mcp does not rotate the
	// file — operators pair it with logrotate(8) on long-lived hosts.
	logOutput := fs.String("log-output", LogOutputStderr,
		"slog destination: \"stderr\" (default), \"stdout\" (DANGER), or a file path (append-only, mode 0600)")
	// Size-based rotation for -log-output. Default 0 = disabled, which
	// preserves the legacy "open once, never rotate" behaviour
	// byte-for-byte for deployments paired with logrotate(8). When
	// positive, the rotator renames the live file to
	// "<path>.<unix-ns>" and reopens a fresh <path> whenever a Write
	// would push the file past the cap. Int64 so multi-GB caps stay
	// representable on 32-bit hosts; negative values are folded into
	// the disabled case by openLogOutput.
	var logRotateSize int64
	fs.Int64Var(&logRotateSize, "log-rotate-size", 0,
		"size-based rotation cap for -log-output in bytes; 0 disables")
	// Maximum archive count retained on disk after a rollover. The
	// oldest (by mtime) are removed once the directory holds more than
	// this many "<path>.<stamp>" files. Default 5 mirrors logrotate(8)'s
	// `rotate` default so operators get a familiar number; ignored when
	// -log-rotate-size=0 because no archives are ever produced.
	logRotateKeep := fs.Int("log-rotate-keep", 5,
		"maximum rotated archive files to retain when -log-rotate-size>0 (default 5)")
	// Default to the env var so systemd / container deployments can
	// pin a known socket path without rewriting argv. The flag, when
	// passed, wins.
	socket := fs.String("socket", os.Getenv("TMUX_MCP_SOCKET"),
		"absolute path for the private tmux socket "+
			"(env TMUX_MCP_SOCKET; default: fresh tempdir)")
	// Same env-var-falls-through pattern as -socket: operators on Nix /
	// Homebrew / static builds want to pin a specific tmux binary
	// instead of relying on PATH (e.g. testing against a known tmux
	// version, sandboxing, or container deployments where multiple
	// tmux versions live side-by-side). Empty default preserves the
	// historical "resolve `tmux` from PATH" behaviour for everyone
	// else. Validation lives inside tmuxctl.WithBinary so a bogus path
	// surfaces a single, consistent diagnostic regardless of whether
	// the value came from the flag or the env var.
	tmuxBin := fs.String("tmux-bin", os.Getenv("TMUX_MCP_TMUX_BIN"),
		"absolute path to the tmux executable "+
			"(env TMUX_MCP_TMUX_BIN; default: resolve `tmux` from PATH)")
	// Same env-var-falls-through pattern as -tmux-bin: operators want a
	// single place to declare a custom tmux.conf for the agent's
	// private tmux server (low escape-time, large history-limit, etc.)
	// without touching ~/.tmux.conf and bleeding into the user's
	// interactive shell. Empty default preserves the historical
	// "tmux uses its built-in defaults / ~/.tmux.conf" behaviour for
	// everyone else. Validation lives inside tmuxctl.WithConfigPath so
	// a bogus path surfaces a single, consistent diagnostic regardless
	// of whether the value came from the flag or the env var.
	tmuxConfigPath := fs.String("tmux-config-path", os.Getenv("TMUX_MCP_TMUX_CONFIG_PATH"),
		"absolute path to a tmux.conf file passed via tmux -f "+
			"(env TMUX_MCP_TMUX_CONFIG_PATH; default: tmux's built-in defaults)")
	// 64 is a generous default for an interactive single-agent client
	// (Claude Desktop typically runs 1–4 tools in parallel) while still
	// putting a ceiling on goroutines a misbehaving / flooding client
	// can spawn. Operators who genuinely want unbounded behaviour can
	// pass -max-concurrent-calls=0.
	maxConcurrentCalls := fs.Int("max-concurrent-calls", 64,
		"cap simultaneously-executing tools/call frames; 0 disables")
	// Default 0 = disabled, preserving the historical "stream whatever
	// the handler produced" behaviour. A positive value caps the
	// marshalled JSON-RPC response body so a misbehaving tool (e.g.
	// capture_pane on a 10MB scrollback) cannot dump a multi-megabyte
	// frame onto an MCP client whose reader can't tolerate it. Int64 so
	// the flag can name multi-GB ceilings on 32-bit hosts without
	// overflow; negative values are treated as 0 by the server.
	var maxResponseBytes int64
	fs.Int64Var(&maxResponseBytes, "max-response-bytes", 0,
		"cap marshalled JSON-RPC response body in bytes; 0 disables")
	// Empty default keeps the audit log opt-in: existing deployments
	// see no behaviour change. "stderr" is a magic path that shares
	// the slog stream; any other value is a filesystem path.
	auditLog := fs.String("audit-log", "",
		"path for JSONL audit records (\"stderr\" or a file path; default: disabled)")
	// Default mirrors snapshot.DefaultTTL so the help text and the
	// library default cannot drift apart. 0 disables cleanup, which
	// preserves pre-flag behaviour for anyone who explicitly opts out.
	snapshotTTL := fs.Duration("snapshot-ttl", snapshot.DefaultTTL,
		"max idle time a session's snapshot history is kept (0 disables cleanup)")
	// 5s is long enough that an in-flight `tools/call` returning a
	// capture-pane snapshot or a wait_for_text result has time to
	// serialise its response; short enough that systemd's default
	// TimeoutStopSec=90s never trips on us. Operators with longer
	// wait_for_text deadlines can bump it; setting 0 keeps the legacy
	// "drop in-flight responses on the floor" behaviour for tests /
	// scripts that don't care.
	shutdownTimeout := fs.Duration("shutdown-timeout", 5*time.Second,
		"on SIGTERM/SIGINT, drain in-flight tools/call responses for up to DUR "+
			"before exiting; 0 disables the drain")
	// Default 0 = feature disabled. The reaper goroutine is only
	// launched when this is positive, so leaving the flag unset (or
	// passing 0 explicitly) preserves the historical "tmux-mcp never
	// kills a session for you" behaviour for desktop deployments
	// where the human / agent decides session lifetime.
	sessionIdleTimeout := fs.Duration("session-idle-timeout", 0,
		"auto-kill any session idle for at least DUR (0 disables; rejected if negative)")
	// Empty default keeps the Prometheus exporter opt-in: no extra
	// HTTP listener appears unless the operator names a bind address.
	// We deliberately do NOT default to a wildcard like ":9090" —
	// binding to all interfaces should be a deliberate choice.
	metricsAddr := fs.String("metrics-addr", "",
		"listen address for Prometheus /metrics (e.g. 127.0.0.1:9090); empty disables")
	// pprof is opt-in for two reasons: heap / goroutine snapshots can
	// leak in-memory state (tool args have already been redacted from
	// the audit log, but a process-memory dump bypasses that), and a
	// /debug/pprof/profile request blocks for 30s by default — fine
	// when an operator deliberately runs it, surprising if it appears
	// silently. Bool default false keeps the metrics-only deployment
	// (the dominant path) byte-identical to the pre-flag behaviour.
	enablePprof := fs.Bool("pprof", false,
		"mount net/http/pprof under /debug/pprof on the -metrics-addr listener (requires -metrics-addr; default: disabled)")
	// Empty default keeps the pid-file feature opt-in: existing
	// deployments see no behaviour change. When set, the file is
	// written atomically (write to PATH.tmp + rename) before any
	// sockets are opened so a half-running process never appears in a
	// supervisor's view of the world.
	pidFile := fs.String("pid-file", "",
		"path to write the server PID to; removed on graceful shutdown (default: disabled)")
	// Empty default keeps every tool exposed (the original behaviour).
	// A non-empty value is a comma-separated set of tool names; only
	// those names appear in tools/list and are dispatchable through
	// tools/call. Unknown names abort startup so a typo never silently
	// disables a tool. Validation happens against the live tool
	// registry (after the package init() hooks have run) so a future
	// tool addition is automatically pickable up by name without
	// touching this flag.
	allowlist := fs.String("allowlist", "",
		"comma-separated list of tool names to expose; empty = no filter (default)")
	// Empty default keeps the prefix feature opt-in: existing
	// deployments see no behaviour change. When set, every session
	// session_create lands on tmux as "<prefix><name>" and every other
	// session-bearing tool resolves the bare name back to the prefixed
	// identity transparently. session_list/kill_all_sessions filter the
	// view to entries inside the prefix so co-tenant agents stay
	// isolated.
	sessionPrefix := fs.String("session-prefix", "",
		"when set, prepend this string to every session name created on tmux (regex [A-Za-z0-9_-]+, no trailing '-')")
	// Default false keeps the dispatcher byte-identical to the
	// pre-flag wire response: every registered tool dispatches normally
	// and clients see exactly the surface they have today. When set,
	// the dispatcher rejects any tools/call whose tool name is not in
	// the inspection allowlist (see internal/server.IsReadOnlyTool)
	// with a typed -32011 error, while leaving tools/list unchanged so
	// a constrained agent can still enumerate the full surface and
	// pick which inspection tools to invoke.
	readOnly := fs.Bool("read-only", false,
		"reject every tools/call that would mutate tmux state (only inspection tools allowed)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected positional argument %q (run with -help)", fs.Arg(0))
	}
	// Reject negative durations up front with a clean stderr line and a
	// non-zero exit. Positive zero is the documented "disabled" case so
	// we leave it alone; only the strictly-negative path is invalid.
	if *sessionIdleTimeout < 0 {
		_, _ = fmt.Fprintf(stderr, "tmux-mcp: -session-idle-timeout %s must not be negative\n", *sessionIdleTimeout)
		return errInvalidIdleTimeout
	}
	// -pprof has no listener of its own — it co-mounts on the metrics
	// server's mux. Demanding -metrics-addr keeps the contract
	// auditable: the operator who chose the bind address is the
	// operator who chose to expose pprof. Failing fast at the CLI
	// boundary (before any side effects below) means a misconfigured
	// systemd unit / Claude Desktop config surfaces the mistake
	// immediately instead of half-running with a silently-disabled
	// pprof.
	if *enablePprof && *metricsAddr == "" {
		_, _ = fmt.Fprintln(stderr, "tmux-mcp: -pprof requires -metrics-addr to be set")
		return errPprofRequiresMetricsAddr
	}
	// Validate -session-prefix at startup — a malformed value must abort
	// before any tmux state is created so the operator never half-runs
	// with a prefix that would hit tmux as a name no other tool can
	// validly reference. The empty default is always accepted (no
	// prefixing). Diagnostics go to stderr; main() maps the sentinel to
	// exit code 2.
	if perr := server.ValidateSessionPrefix(*sessionPrefix); perr != nil {
		_, _ = fmt.Fprintf(stderr, "tmux-mcp: %s\n", perr)
		return fmt.Errorf("%w: %w", errInvalidSessionPrefix, perr)
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, versionString())
		return nil
	}
	if *versionJSONFlag {
		return emitVersionJSON(stdout, version, runtime.Version())
	}
	if *probe {
		return runProbe(stdout, stderr, *tmuxBin)
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
	// Resolve -log-output before installing the slog handler so a bad
	// path (parent missing, no write permission) surfaces as a clean
	// startup error instead of half-running with logs lost on the
	// floor. The default value is "stderr", which preserves the legacy
	// behaviour of routing structured logs to the supplied stderr
	// writer; stdout is a magic value for ad-hoc debugging in tandem
	// with -dry-run / -version, and any other value is a filesystem
	// path opened append-only at mode 0600. -log-rotate-size and
	// -log-rotate-keep wire the in-process size-based rotator on top
	// of the file path; rotateSize<=0 keeps the legacy "open once,
	// never rotate" behaviour byte-for-byte for deployments paired
	// with logrotate(8).
	logWriter, closeLogOutput, err := openLogOutput(*logOutput, stderr, stdout, logRotateSize, *logRotateKeep)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tmux-mcp: %s\n", err)
		return err
	}
	defer func() { _ = closeLogOutput() }()
	// Structured logs go to the resolved writer — by default that is
	// stderr (stdout stays reserved for JSON-RPC frames). Operators
	// who passed -log-output=PATH get a private append-only file; the
	// stdout magic value is honoured for debugging but corrupts the
	// JSON-RPC framing if combined with serving stdio.
	slog.SetDefault(slog.New(newLogHandler(logWriter, lvl, format, *logSource)))

	// Write the pid file before opening any sockets so a permission /
	// "stale pid file" failure surfaces as a clean startup error and we
	// never half-run with sockets bound but no externalised PID. The
	// defer covers every failure path below (tmuxctl, audit open,
	// metrics bind, Serve return) so the file is removed on graceful
	// shutdown regardless of where the bootstrap unwound.
	if *pidFile != "" {
		if perr := writePIDFile(*pidFile); perr != nil {
			return perr
		}
		defer func() {
			// Best-effort: a missing file (e.g. an operator removed it
			// mid-run) or a permission flap on the parent dir is not
			// worth surfacing as a non-zero exit — the process has
			// already finished its real work.
			if rerr := os.Remove(*pidFile); rerr != nil && !os.IsNotExist(rerr) {
				slog.Warn("pid file cleanup failed", "path", *pidFile, "err", rerr)
			}
		}()
	}

	ctl, err := tmuxctl.NewWithSocket(
		*socket,
		tmuxctl.WithBinary(*tmuxBin),
		tmuxctl.WithConfigPath(*tmuxConfigPath),
	)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Bind SIGTERM/SIGINT to ctx cancellation by hand (instead of via
	// signal.NotifyContext) so we can also try to close stdin on
	// signal. Serve's dispatcher already wakes on ctx.Done(), but its
	// internal reader goroutine is parked in a blocking ReadBytes; the
	// stdin Close lets that helper exit cleanly instead of being leaked
	// until process teardown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case sig := <-sigCh:
			slog.Info("shutdown signal received",
				"signal", sig.String(),
				"shutdown_timeout", *shutdownTimeout,
			)
			cancel()
			// Close stdin to unblock Serve's ReadBytes. If stdin
			// isn't a Closer (uncommon — os.Stdin always is) we just
			// rely on ctx cancellation + the next frame to wake the
			// loop.
			if c, ok := stdin.(io.Closer); ok {
				_ = c.Close()
			}
		case <-ctx.Done():
		}
	}()
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

	// Stand up the Prometheus exporter when the operator passes
	// -metrics-addr. We bind eagerly so port-in-use / malformed-addr
	// failures surface as a clean startup error instead of a
	// silently-broken background goroutine. The session-count poller
	// runs alongside the HTTP server and refreshes the gauge every 5s.
	var (
		metrics       *server.Metrics
		metricsServer *server.MetricsServer
	)
	if *metricsAddr != "" {
		// A fresh registry per process keeps the exporter scoped to
		// the metrics this server owns plus the standard Go / process
		// collectors, without leaking into prometheus.DefaultRegisterer.
		reg := prometheus.NewRegistry()
		reg.MustRegister(collectors.NewGoCollector())
		reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
		metrics = server.NewMetrics(reg)
		metricsServer, err = server.NewMetricsServer(*metricsAddr, reg, metrics, *enablePprof)
		if err != nil {
			return fmt.Errorf("metrics listener %q: %w", *metricsAddr, err)
		}
		slog.Info("metrics exporter listening",
			"addr", metricsServer.Addr(),
			"pprof", *enablePprof,
		)
		go metrics.RunSessionsPoller(ctx, ctl, 5*time.Second)
		// Drive a one-shot startup probe in the background so /healthz
		// flips to 200 once tmux is reachable. We do this in a
		// goroutine (not inline) because the listener is already up: a
		// k8s readiness probe hitting /healthz during a slow probe
		// should see 503, not block on us. The probe shares the parent
		// ctx so a SIGTERM during a wedged tmux start cancels it
		// cleanly. Failure leaves Healthy() false forever — if tmux
		// can't be reached at startup the server is genuinely not
		// ready, and the rest of the binary will fail loudly the
		// first time a tools/call hits it.
		go func() {
			probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
			defer probeCancel()
			// Probe through the same binary the controller is driving so
			// /healthz reflects the runtime path, not whatever happens to
			// be on PATH. tmuxctl.ProbeVersionWithBinary("") is identical
			// to the legacy ProbeVersion(ctx) so the unset case is
			// byte-compatible with pre-flag deployments.
			if _, perr := tmuxctl.ProbeVersionWithBinary(probeCtx, *tmuxBin); perr != nil {
				slog.Warn("startup probe failed; /healthz stays 503",
					"err", perr)
				return
			}
			metrics.MarkHealthy()
		}()
		defer func() {
			// Shutdown is best-effort with a short deadline so we
			// don't hang the process on a slow client. http.Server
			// closes the listener and drains in-flight requests
			// within this window.
			shutCtx, cancelShut := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancelShut()
			_ = metricsServer.Shutdown(shutCtx)
		}()
	}

	tools := server.NewTools(ctl, snapshot.WithTTL(*snapshotTTL))
	// Propagate the ldflags-injected binary version so MCP clients see
	// the same value the -version flag prints, instead of a hardcoded
	// constant inside the server package.
	tools.Version = version
	// Hand the (already-validated) -session-prefix value to the tool
	// surface. Empty keeps the historical no-prefix behaviour; non-empty
	// arms resolveSessionRef / resolvePaneTarget / resolveWindowMoveTarget
	// across every session-bearing tool so the JSON-RPC client sees its
	// logical names and tmux sees the prefixed ones.
	tools.SessionPrefix = *sessionPrefix
	// Mirror -max-response-bytes onto the tool surface so handlers
	// that want to enforce the cap up front (today: save_buffer with
	// `error_on_truncation=true`) can reject oversize bodies before
	// the dispatcher's framing-level guard rewrites them. Most
	// handlers ignore this field entirely — the dispatcher already
	// caps the response after marshal — so this assignment is a no-op
	// for the rest of the surface.
	tools.MaxResponseBytes = maxResponseBytes
	// Install the operator-supplied allowlist (if any) before any
	// tools/list / tools/call frame can reach the dispatcher. Validation
	// runs against the live registry now that every init()-time
	// registration has fired, so a typo in -allowlist surfaces as a
	// clean startup error ("unknown tools in -allowlist: …") instead of
	// a silently-empty surface. The empty-string default skips the
	// SetAllowlist call entirely so unrelated deployments see no
	// behaviour change.
	if *allowlist != "" {
		if err := tools.SetAllowlist(strings.Split(*allowlist, ",")); err != nil {
			return err
		}
	}
	// -dry-run wants every bootstrap side-effect (tmux init, audit
	// open, tool registry build) to happen so we surface real config
	// problems, but stop short of opening stdin. Reporting tmux's
	// version + our own gives operators a single line to grep on for
	// a successful pre-flight, mirroring the -probe tab-delimited
	// shape with a distinct "dry-run ok" prefix so callers can tell
	// the two paths apart. Returning here unwinds the deferred
	// ctl.Shutdown + audit.Close, so resources acquired by the
	// bootstrap are released before exit.
	if *dryRun {
		// Probe through the same binary the controller is driving so the
		// "dry-run ok" line names the binary the runtime would actually
		// invoke — not whatever else happens to be on PATH.
		tmuxVer, err := tmuxctl.ProbeVersionWithBinary(ctx, *tmuxBin)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "dry-run ok\ttmux=%s\ttmux-mcp=%s\n",
			tmuxVer, binaryVersion())
		return nil
	}
	serr := server.Serve(ctx, stdin, stdout, tools.Handle,
		server.WithMaxConcurrentCalls(*maxConcurrentCalls),
		server.WithMaxResponseBytes(maxResponseBytes),
		server.WithAudit(audit),
		server.WithShutdownTimeout(*shutdownTimeout),
		// Pass the controller's KillSession as the reaper's kill hook so
		// reaped sessions go through the same code path session_kill
		// uses. WithSessionIdleTimeout treats d <= 0 as "disabled" and
		// returns a no-op option, so the goroutine cost is paid only
		// when the operator explicitly opted in.
		server.WithSessionIdleTimeout(*sessionIdleTimeout, ctl.KillSession),
		// Hand the writeMu-bound list-change emitter to *Tools so a
		// runtime RegisterTool / UnregisterTool call pushes a
		// spec-compliant notifications/tools/list_changed frame
		// without main needing to know about the notification shape.
		server.WithToolsListChangedNotifier(tools.SetNotifier),
		server.WithMetrics(metrics),
		// Mirror the prefix into the dispatcher so the [IdleReaper]'s
		// activity table keys on the same tmux-real names the controller
		// kills against. *Tools.SessionPrefix already takes care of the
		// handler side; this option is the dispatcher-side counterpart.
		server.WithSessionPrefix(*sessionPrefix),
		// Arm the read-only gate when -read-only was passed. WithReadOnly(false)
		// is a no-op so the option is safe to wire unconditionally.
		server.WithReadOnly(*readOnly),
	)
	if errors.Is(serr, server.ErrShutdownTimedOut) {
		// Surface the timeout via a non-zero exit so supervisors can
		// flag a forced shutdown. The slog.Warn from Serve already
		// logged the cause; main() will log the wrapped error too.
		return serr
	}
	// Plain ctx cancellation is the happy SIGTERM path — not an error.
	if errors.Is(serr, context.Canceled) {
		return nil
	}
	return serr
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
//
// source toggles slog.HandlerOptions.AddSource: when true, every record
// carries the file/line/function of the call site that produced it.
// JSON records gain a "source" object ({"function","file","line"}) and
// text records gain a "source=…" attribute. The flag is off by default
// because AddSource walks runtime.Callers on every record — fine for
// debugging, measurable on a hot logging path.
func newLogHandler(w io.Writer, lvl slog.Level, format logFormat, source bool) slog.Handler {
	opts := &slog.HandlerOptions{Level: lvl, AddSource: source}
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

// writePIDFile atomically writes the current process PID to path as a
// single decimal line ("1234\n"). It is the body of the -pid-file flag
// and the contract is precisely:
//
//   - If path already exists → return an "already exists (stale?)"
//     error so two instances cannot silently clobber each other. The
//     operator is expected to rm the file manually if they're sure the
//     previous run died — better an explicit recovery step than a lost
//     PID for a competing supervisor that's still tracking the old
//     process.
//   - Otherwise → write the PID to "path.tmp" first, then os.Rename it
//     onto path so a reader at any moment sees either no file or a
//     fully-written PID — never a half-written byte. Mode 0644 because
//     pid files are not secrets.
//   - On any failure (perm denied, parent dir missing, …) → return a
//     wrapped error with the path so the operator immediately knows
//     which file failed. Any temp file we created is cleaned up
//     before returning so a retry isn't blocked by our own debris.
func writePIDFile(path string) error {
	// Stat-then-rename has an inherent race against another instance
	// starting at the same instant, but the failure mode is "second
	// starter wins" rather than "both run silently" — and we don't
	// have a portable atomic-no-clobber rename in stdlib. The
	// existence check is the operator-facing contract; the rename
	// below keeps the *content* write atomic for any reader.
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("pid file %q already exists (stale?)", path)
	} else if !os.IsNotExist(err) {
		// Stat failed for a reason other than "missing" (e.g. a
		// permission error on an ancestor directory). Surface that
		// rather than silently proceeding to write — the rename below
		// would just fail with a less informative error.
		return fmt.Errorf("pid file %q: %w", path, err)
	}

	// Write to a sibling .tmp first so the final filename only ever
	// appears with the complete PID. WriteFile truncates if .tmp
	// already exists, which is the right thing for a leftover from a
	// crashed previous attempt: it's debris, not state we want to
	// preserve.
	tmp := path + ".tmp"
	content := fmt.Appendf(nil, "%d\n", os.Getpid())
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return fmt.Errorf("pid file %q: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// Clean up the .tmp so a retry doesn't trip over our debris.
		_ = os.Remove(tmp)
		return fmt.Errorf("pid file %q: %w", path, err)
	}
	return nil
}

// errProbeFailed is the sentinel returned from [runProbe] when the
// startup health check fails. main() uses [errors.Is] to recognise it
// and skip the structured-log error message — the probe path already
// wrote a "probe failed: …" diagnostic to stderr and we don't want to
// surface the same failure twice.
var errProbeFailed = errors.New("probe failed")

// runProbe is the body of the -probe flag. It probes the configured
// tmux binary (an explicit path passed via -tmux-bin / TMUX_MCP_TMUX_BIN
// when non-empty, otherwise tmux looked up on PATH), runs `tmux -V`,
// checks the minimum version, and writes a single tab-delimited "ok"
// line to stdout when everything is healthy:
//
//	ok\ttmux=<tmux-version>\ttmux-mcp=<binary-version>\n
//
// On failure it writes a "probe failed: …" diagnostic to stderr and
// returns an error wrapping [errProbeFailed] so the caller can map it
// to a non-zero exit code. Stdout is left untouched on the failure path
// so orchestrators can rely on stdout being empty when probing failed.
//
// tmuxBin is the operator-supplied override (empty = legacy PATH
// resolution). Threading it through here keeps the -probe path
// honouring the same binary the runtime would actually drive.
func runProbe(stdout, stderr io.Writer, tmuxBin string) error {
	// 5s is generous: `tmux -V` is essentially instant. A timeout
	// keeps a wedged binary on a misconfigured PATH from hanging the
	// liveness check forever.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tmuxVer, err := tmuxctl.ProbeVersionWithBinary(ctx, tmuxBin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "probe failed: %s\n", err)
		return fmt.Errorf("%w: %w", errProbeFailed, err)
	}
	_, _ = fmt.Fprintf(stdout, "ok\ttmux=%s\ttmux-mcp=%s\n", tmuxVer, binaryVersion())
	return nil
}
