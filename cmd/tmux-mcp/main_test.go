package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-version"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("run(-version): %v (stderr=%q)", err, stderr.String())
	}
	got := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(got, "tmux-mcp ") {
		t.Fatalf("expected version line, got %q", got)
	}
}

func TestHelpFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-help"}, strings.NewReader(""), &stdout, &stderr)
	// flag.ContinueOnError surfaces -help as flag.ErrHelp; treat that as success.
	if err != nil && err.Error() != "flag: help requested" {
		t.Fatalf("run(-help): unexpected error %v", err)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Fatalf("expected usage in stderr, got %q", stderr.String())
	}
}

func TestUnknownFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-nope"}, strings.NewReader(""), &stdout, &stderr); err == nil {
		t.Fatal("expected error for unknown flag, got nil")
	}
}

func TestPositionalArgsRejected(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"oops"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for positional arg, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected positional argument") {
		t.Fatalf("expected positional-arg error, got %v", err)
	}
}

func TestInvalidLogLevelRejected(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-log-level=loud"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for invalid log level, got nil")
	}
	if !strings.Contains(err.Error(), "invalid -log-level") {
		t.Fatalf("expected invalid -log-level error, got %v", err)
	}
}

// TestSessionIdleTimeoutFlag_RejectsNegative pins the contract that a
// negative -session-idle-timeout is a startup error with a non-zero
// exit (mapped via [errInvalidIdleTimeout] in main()), and that the
// flag is documented in the -help usage block so operators discover
// it. Positive zero is the documented "disabled" case and must NOT
// error — that's what the second case asserts.
func TestSessionIdleTimeoutFlag_RejectsNegative(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-session-idle-timeout=-1m"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for negative -session-idle-timeout, got nil")
	}
	if !errors.Is(err, errInvalidIdleTimeout) {
		t.Fatalf("expected errInvalidIdleTimeout, got %v", err)
	}
	if !strings.Contains(stderr.String(), "must not be negative") {
		t.Fatalf("expected stderr to explain the rejection, got %q", stderr.String())
	}

	// -help must document the flag so operators can discover it.
	stdout.Reset()
	stderr.Reset()
	if err := run([]string{"-help"}, strings.NewReader(""), &stdout, &stderr); err != nil && err.Error() != "flag: help requested" {
		t.Fatalf("run(-help): unexpected error %v", err)
	}
	if !strings.Contains(stderr.String(), "-session-idle-timeout") {
		t.Fatalf("expected -session-idle-timeout in usage block, got %q", stderr.String())
	}
}

// TestPprofFlag_RejectsWithoutMetricsAddr pins the contract that
// -pprof is a deliberately co-mounted feature: it has no listener of
// its own and therefore requires -metrics-addr to be set. A missing
// -metrics-addr must surface as a clean startup error wrapping
// errPprofRequiresMetricsAddr, with a stderr line that names the
// prerequisite so an operator on a misconfigured systemd unit doesn't
// have to guess. This guards against a regression that silently
// disables pprof (or, worse, auto-binds a port on the operator's
// behalf) when -pprof appears without an address.
func TestPprofFlag_RejectsWithoutMetricsAddr(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-pprof"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for -pprof without -metrics-addr, got nil")
	}
	if !errors.Is(err, errPprofRequiresMetricsAddr) {
		t.Fatalf("expected errPprofRequiresMetricsAddr, got %v", err)
	}
	if !strings.Contains(stderr.String(), "-pprof requires -metrics-addr") {
		t.Fatalf("expected stderr to explain the prerequisite, got %q", stderr.String())
	}
	// stdout stays untouched so an operator piping JSON-RPC into the
	// binary doesn't see a stray non-frame on the failure path.
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout on the rejection path, got %q", stdout.String())
	}

	// -help must document the flag so operators discover it without
	// reading the source.
	stdout.Reset()
	stderr.Reset()
	if err := run([]string{"-help"}, strings.NewReader(""), &stdout, &stderr); err != nil && err.Error() != "flag: help requested" {
		t.Fatalf("run(-help): unexpected error %v", err)
	}
	if !strings.Contains(stderr.String(), "-pprof") {
		t.Fatalf("expected -pprof in usage block, got %q", stderr.String())
	}
}

// TestSnapshotTTLFlag_AcceptedAndDocumented confirms that the
// -snapshot-ttl flag is parsed (i.e. not rejected as "unknown flag")
// and that its help line is part of the -help usage block. Behavioural
// coverage for the underlying TTL plumbing lives in
// internal/snapshot — here we just guard the wire-up so a future
// rename of either side trips a test instead of silently breaking
// operator deployment knobs.
func TestSnapshotTTLFlag_AcceptedAndDocumented(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-help"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil && err.Error() != "flag: help requested" {
		t.Fatalf("run(-help): unexpected error %v", err)
	}
	if !strings.Contains(stderr.String(), "-snapshot-ttl") {
		t.Fatalf("expected -snapshot-ttl in usage block, got %q", stderr.String())
	}

	// Reject obviously bad duration syntax — flag.Duration handles
	// this for us, so all we need to confirm is that we wired the
	// flag up. "abc" is unparseable.
	stdout.Reset()
	stderr.Reset()
	if err := run([]string{"-snapshot-ttl=abc"}, strings.NewReader(""), &stdout, &stderr); err == nil {
		t.Fatal("expected error for malformed -snapshot-ttl value, got nil")
	}
}

// TestRelativeSocketRejected makes sure the surface validation in
// tmuxctl.NewWithSocket bubbles up through main.run, so users see the
// error message instead of a confused "no server running" later.
func TestRelativeSocketRejected(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	var stdout, stderr bytes.Buffer
	err := run([]string{"-socket=relative/sock"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for relative socket path")
	}
	if !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSocketEnvFallback covers the env-var path: when -socket is not
// passed but TMUX_MCP_SOCKET is set to a bogus relative value, run()
// must still surface the validation error rather than silently fall
// through to MkdirTemp.
func TestSocketEnvFallback(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	t.Setenv("TMUX_MCP_SOCKET", "relative/sock-from-env")
	var stdout, stderr bytes.Buffer
	err := run(nil, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for relative socket path from env")
	}
	if !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseLogLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"error", false},
		{"warn", false},
		{"warning", false},
		{"info", false},
		{"DEBUG", false},
		{"", false},
		{"trace", true},
	}
	for _, tc := range cases {
		_, err := parseLogLevel(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseLogLevel(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
		}
	}
}

// TestProbeFlag exercises the happy path of `tmux-mcp -probe` end-to-end
// through run(): with tmux on PATH the probe prints the
// "ok\ttmux=<v>\ttmux-mcp=<v>" line on stdout, writes nothing on
// stderr, and returns nil so the binary exits 0.
func TestProbeFlag(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-probe"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("run(-probe): %v (stderr=%q)", err, stderr.String())
	}
	got := stdout.String()
	if !strings.HasPrefix(got, "ok\ttmux=") {
		t.Fatalf("expected stdout to start with %q, got %q", "ok\ttmux=", got)
	}
	if !strings.Contains(got, "\ttmux-mcp=") {
		t.Fatalf("expected stdout to contain tmux-mcp version field, got %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("expected stdout to end with newline, got %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr on success, got %q", stderr.String())
	}
}

// TestRunProbeFailure unit-tests runProbe directly with a stripped-down
// PATH so tmux cannot be found. The function must write a "probe
// failed: " diagnostic to stderr, leave stdout untouched, and return an
// error that matches errProbeFailed so main() can suppress the slog
// duplicate.
func TestRunProbeFailure(t *testing.T) {
	t.Setenv("PATH", "/nonexistent-empty-dir-for-probe-test")
	var stdout, stderr bytes.Buffer
	err := runProbe(&stdout, &stderr, "")
	if err == nil {
		t.Fatal("expected error when tmux is not on PATH")
	}
	if !errors.Is(err, errProbeFailed) {
		t.Fatalf("expected errProbeFailed, got %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout on failure, got %q", stdout.String())
	}
	if !strings.HasPrefix(stderr.String(), "probe failed: ") {
		t.Fatalf("expected stderr to start with %q, got %q",
			"probe failed: ", stderr.String())
	}
}

// TestRunProbeSuccess unit-tests runProbe directly with a real tmux on
// PATH so we can assert the exact tab-delimited shape and field order
// without spinning up a subprocess. Skips when tmux is unavailable.
func TestRunProbeSuccess(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	var stdout, stderr bytes.Buffer
	if err := runProbe(&stdout, &stderr, ""); err != nil {
		t.Fatalf("runProbe: %v (stderr=%q)", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr on success, got %q", stderr.String())
	}
	line := strings.TrimSuffix(stdout.String(), "\n")
	parts := strings.Split(line, "\t")
	if len(parts) != 3 {
		t.Fatalf("expected 3 tab-separated fields, got %d in %q", len(parts), line)
	}
	if parts[0] != "ok" {
		t.Fatalf("expected first field %q, got %q", "ok", parts[0])
	}
	if !strings.HasPrefix(parts[1], "tmux=") {
		t.Fatalf("expected second field to start with %q, got %q", "tmux=", parts[1])
	}
	if !strings.HasPrefix(parts[2], "tmux-mcp=") {
		t.Fatalf("expected third field to start with %q, got %q", "tmux-mcp=", parts[2])
	}
}

// TestAuditLogBadPathFailsStartup pins the operator-visible contract for
// -audit-log: when the supplied path is unopenable (e.g. the parent
// directory does not exist), run() must surface the error so main exits
// non-zero. Silently running with audit disabled would betray the
// operator's expectation that audit is on.
func TestAuditLogBadPathFailsStartup(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	var stdout, stderr bytes.Buffer
	err := run([]string{"-audit-log=/no/such/dir/audit.log"},
		strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for unopenable audit-log path, got nil")
	}
	if !strings.Contains(err.Error(), "audit log") {
		t.Fatalf("expected error to mention audit log, got %v", err)
	}
}

// TestInvalidShutdownTimeoutRejected covers the -shutdown-timeout flag
// being rejected at parse time when the duration is malformed. Going
// through run() (rather than parseLogLevel-style helpers) makes sure
// the flag actually got registered with FlagSet.
func TestInvalidShutdownTimeoutRejected(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-shutdown-timeout=banana"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for malformed -shutdown-timeout, got nil")
	}
	if !strings.Contains(err.Error(), "shutdown-timeout") {
		t.Fatalf("expected shutdown-timeout flag error, got %v", err)
	}
}

// TestShutdownTimeoutInUsage guards the help text: -shutdown-timeout
// must be documented in the usage block so operators discover the
// drain knob via `tmux-mcp -help`.
func TestShutdownTimeoutInUsage(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	_ = run([]string{"-help"}, strings.NewReader(""), &stdout, &stderr)
	if !strings.Contains(stderr.String(), "-shutdown-timeout") {
		t.Fatalf("expected -shutdown-timeout in usage block, got %q", stderr.String())
	}
}

// TestMaxResponseBytesInUsage guards the help text: -max-response-bytes
// must be documented in the usage block so operators discover the
// response-size ceiling via `tmux-mcp -help`. Behavioural coverage for
// the wire-side replacement lives in
// internal/server/jsonrpc_test.go (TestOversizedResponse).
func TestMaxResponseBytesInUsage(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	_ = run([]string{"-help"}, strings.NewReader(""), &stdout, &stderr)
	if !strings.Contains(stderr.String(), "-max-response-bytes") {
		t.Fatalf("expected -max-response-bytes in usage block, got %q", stderr.String())
	}
}

// TestDebugLevelEmitsJSONLogs is a smoke test: with -log-level=debug
// (and no -log-format) the legacy auto-promotion kicks in and a
// malformed request line on stdin must produce a JSON-formatted slog
// record on stderr (and stdout must stay valid JSON-RPC).
func TestDebugLevelEmitsJSONLogs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// "not json\n" gets stdin EOF after one line, so Serve returns and
	// run() unwinds cleanly. The malformed line trips the "invalid
	// request" warn-level log, which is well above debug.
	err := run([]string{"-log-level=debug"}, strings.NewReader("not json\n"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v stderr=%q", err, stderr.String())
	}
	// stderr must contain at least one slog JSON record.
	gotLog := false
	for line := range strings.SplitSeq(stderr.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil {
			if _, ok := rec["level"]; ok {
				gotLog = true
				break
			}
		}
	}
	if !gotLog {
		t.Fatalf("expected JSON slog record on stderr, got %q", stderr.String())
	}
	// stdout should hold the JSON-RPC parse-error response and nothing else.
	body := strings.TrimSpace(stdout.String())
	if !strings.Contains(body, "-32700") {
		t.Fatalf("expected JSON-RPC parse error on stdout, got %q", body)
	}
}

// TestResolveLogFormat covers the matrix of (raw value, level, explicit)
// inputs that resolveLogFormat is responsible for: explicit values
// always win, the implicit default falls back to text except when
// -log-level=debug auto-promotes to JSON, and unknown values produce a
// wrapped errInvalidLogFormat sentinel.
func TestResolveLogFormat(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		lvl      slog.Level
		explicit bool
		want     logFormat
		wantErr  bool
	}{
		{"explicit-text", "text", slog.LevelInfo, true, logFormatText, false},
		{"explicit-json", "json", slog.LevelInfo, true, logFormatJSON, false},
		{"explicit-text-at-debug-stays-text", "text", slog.LevelDebug, true, logFormatText, false},
		{"explicit-json-at-info-stays-json", "json", slog.LevelInfo, true, logFormatJSON, false},
		{"implicit-default-info-is-text", "text", slog.LevelInfo, false, logFormatText, false},
		{"implicit-default-warn-is-text", "text", slog.LevelWarn, false, logFormatText, false},
		{"implicit-default-error-is-text", "text", slog.LevelError, false, logFormatText, false},
		{"implicit-default-debug-promotes-to-json", "text", slog.LevelDebug, false, logFormatJSON, false},
		{"case-insensitive-uppercase-text", "TEXT", slog.LevelInfo, true, logFormatText, false},
		{"case-insensitive-mixed-json", "Json", slog.LevelInfo, true, logFormatJSON, false},
		{"whitespace-trimmed", "  json  ", slog.LevelInfo, true, logFormatJSON, false},
		{"unknown-yaml-is-error", "yaml", slog.LevelInfo, true, "", true},
		{"empty-is-error", "", slog.LevelInfo, true, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveLogFormat(tc.raw, tc.lvl, tc.explicit)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveLogFormat(%q, %v, %v) = %q, nil; want errInvalidLogFormat",
						tc.raw, tc.lvl, tc.explicit, got)
				}
				if !errors.Is(err, errInvalidLogFormat) {
					t.Fatalf("err = %v; want wrap of errInvalidLogFormat", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveLogFormat(%q, %v, %v) unexpected err: %v",
					tc.raw, tc.lvl, tc.explicit, err)
			}
			if got != tc.want {
				t.Fatalf("resolveLogFormat(%q, %v, %v) = %q, want %q",
					tc.raw, tc.lvl, tc.explicit, got, tc.want)
			}
		})
	}
}

// TestNewLogHandler asserts the small dispatcher routes to the right
// slog handler implementation. The slog package types are concrete, so
// we can test by writing a record and inspecting the on-the-wire shape:
// JSON output starts with '{', text output is key=value form.
func TestNewLogHandler(t *testing.T) {
	t.Parallel()
	cases := []struct {
		format     logFormat
		wantPrefix string // first non-space byte
	}{
		{logFormatJSON, "{"},
		{logFormatText, "t"}, // text handler emits "time=…"
	}
	for _, tc := range cases {
		t.Run(string(tc.format), func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			h := newLogHandler(&buf, slog.LevelInfo, tc.format, false)
			slog.New(h).Info("hello", "k", "v")
			out := strings.TrimSpace(buf.String())
			if out == "" {
				t.Fatalf("expected log output, got empty buffer")
			}
			if !strings.HasPrefix(out, tc.wantPrefix) {
				t.Fatalf("format=%s: expected output to start with %q, got %q",
					tc.format, tc.wantPrefix, out)
			}
		})
	}
}

// TestNewLogHandlerLogSource is the focused unit test for the
// -log-source wiring. It pins three contracts:
//
//  1. With source=true, the JSON record carries a structured "source"
//     object ({"function","file","line"}) — that is the slog AddSource
//     emission shape, and operators rely on it for log-aggregation
//     pipelines that key off file/line.
//  2. The "source.file" path ends with this test file's basename, so we
//     know the AddSource walker captured the actual call site of
//     slog.Info rather than a frame from inside slog itself.
//  3. With source=false, no "source" key is emitted at all (so the
//     default config stays byte-identical to the legacy output).
func TestNewLogHandlerLogSource(t *testing.T) {
	t.Parallel()

	// source=true: the JSON record must carry a "source" object whose
	// "file" field ends with this test file's basename.
	t.Run("json-with-source", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		h := newLogHandler(&buf, slog.LevelInfo, logFormatJSON, true)
		slog.New(h).Info("hello", "k", "v")

		var rec map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
			t.Fatalf("expected one JSON record, got %q (err=%v)", buf.String(), err)
		}
		src, ok := rec["source"].(map[string]any)
		if !ok {
			t.Fatalf("expected source object in record, got %v", rec)
		}
		// AddSource always populates function/file/line — assert all three
		// rather than just one so a future slog change that drops a field
		// also trips this test.
		for _, k := range []string{"function", "file", "line"} {
			if _, ok := src[k]; !ok {
				t.Fatalf("expected source.%s, got %v", k, src)
			}
		}
		file, _ := src["file"].(string)
		if !strings.HasSuffix(file, "main_test.go") {
			t.Fatalf("expected source.file to end with main_test.go, got %q", file)
		}
	})

	// source=false: the JSON record must NOT carry a "source" key. This
	// guards the zero-overhead default — flipping AddSource on by
	// accident would inflate structured-log volume on every deployment.
	t.Run("json-without-source", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		h := newLogHandler(&buf, slog.LevelInfo, logFormatJSON, false)
		slog.New(h).Info("hello", "k", "v")

		var rec map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
			t.Fatalf("expected one JSON record, got %q (err=%v)", buf.String(), err)
		}
		if _, ok := rec["source"]; ok {
			t.Fatalf("expected no source key with source=false, got %v", rec)
		}
	})

	// Text handler also honours AddSource — assert the on-the-wire
	// "source=…" attribute appears so the flag is useful regardless of
	// the chosen format.
	t.Run("text-with-source", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		h := newLogHandler(&buf, slog.LevelInfo, logFormatText, true)
		slog.New(h).Info("hello", "k", "v")
		out := buf.String()
		if !strings.Contains(out, "source=") {
			t.Fatalf("expected text record to contain source=…, got %q", out)
		}
		if !strings.Contains(out, "main_test.go") {
			t.Fatalf("expected text record to mention main_test.go, got %q", out)
		}
	})
}

// TestLogSourceFlag_AcceptedAndDocumented confirms the -log-source
// flag is wired up end-to-end through run() (not just the helper) and
// that its help line ships in -help output. Behaviour for the
// underlying handler is covered in TestNewLogHandlerLogSource — here
// we just guard the CLI surface so a future rename trips a test.
func TestLogSourceFlag_AcceptedAndDocumented(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	err := run([]string{"-help"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil && err.Error() != "flag: help requested" {
		t.Fatalf("run(-help): unexpected error %v", err)
	}
	if !strings.Contains(stderr.String(), "-log-source") {
		t.Fatalf("expected -log-source in usage block, got %q", stderr.String())
	}

	// Smoke-test the flag is parsed (i.e. registered with FlagSet) by
	// running with -log-format=json -log-source=true and a malformed
	// stdin line. The resulting JSON slog record on stderr must carry
	// a "source" object — proving the flag flowed through to the
	// installed handler, not just that it was accepted by FlagSet.
	stdout.Reset()
	stderr.Reset()
	err = run([]string{"-log-format=json", "-log-source=true"},
		strings.NewReader("not json\n"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v stderr=%q", err, stderr.String())
	}
	sawSource := false
	for line := range strings.SplitSeq(stderr.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if _, ok := rec["source"]; ok {
			sawSource = true
			break
		}
	}
	if !sawSource {
		t.Fatalf("expected at least one JSON record with source key on stderr, got %q",
			stderr.String())
	}
}

// TestLogFormatTextEmitsTextLogs is the e2e companion to
// TestDebugLevelEmitsJSONLogs: when the operator passes -log-format=text
// explicitly (even at debug), the handler installed must be the text
// handler — confirmed by stderr lines that are NOT valid JSON.
func TestLogFormatTextEmitsTextLogs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"-log-level=debug", "-log-format=text"},
		strings.NewReader("not json\n"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v stderr=%q", err, stderr.String())
	}
	saw := false
	for line := range strings.SplitSeq(stderr.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Text handler emits "time=… level=… msg=…". A JSON record
		// starts with '{'; a text record does not.
		if strings.HasPrefix(line, "{") {
			t.Fatalf("expected text-format log line, got JSON: %q", line)
		}
		if strings.Contains(line, "level=") && strings.Contains(line, "msg=") {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("expected at least one text-format slog record on stderr, got %q", stderr.String())
	}
}

// TestLogFormatJSONOverridesAtInfo asserts the operator can opt in to
// JSON even when the auto-promotion would not have fired (i.e. at
// info level). This is the inverse of the legacy behaviour and the
// real reason the flag exists: log aggregation pipelines need a
// stable, parseable shape regardless of the configured level.
func TestLogFormatJSONOverridesAtInfo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"-log-format=json"}, strings.NewReader("not json\n"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v stderr=%q", err, stderr.String())
	}
	gotLog := false
	for line := range strings.SplitSeq(stderr.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil {
			if _, ok := rec["level"]; ok {
				gotLog = true
				break
			}
		}
	}
	if !gotLog {
		t.Fatalf("expected JSON slog record on stderr at info level, got %q", stderr.String())
	}
}

// TestInvalidLogFormatRejected makes sure run() surfaces a wrapped
// errInvalidLogFormat for unknown values, that it writes a
// "tmux-mcp: invalid -log-format …" diagnostic to stderr, and that it
// leaves stdout untouched so an MCP client never sees a stray frame
// during a misconfigured launch.
func TestInvalidLogFormatRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"-log-format=yaml"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for invalid -log-format, got nil")
	}
	if !errors.Is(err, errInvalidLogFormat) {
		t.Fatalf("expected errInvalidLogFormat wrap, got %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected stdout untouched on validation failure, got %q", stdout.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "tmux-mcp: invalid -log-format") {
		t.Fatalf("expected stderr to contain diagnostic, got %q", got)
	}
	if !strings.Contains(got, `"yaml"`) {
		t.Fatalf("expected stderr diagnostic to quote the bad value, got %q", got)
	}
}

// TestDryRun_Success exercises the happy path: with tmux on PATH and no
// other config knobs touched, -dry-run must walk the full bootstrap and
// emit a single tab-delimited "dry-run ok" line on stdout. The line
// shape mirrors -probe so callers can pattern-match on a stable prefix
// ("dry-run ok\ttmux=…\ttmux-mcp=…\n").
func TestDryRun_Success(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-dry-run"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("run(-dry-run): %v (stderr=%q)", err, stderr.String())
	}
	got := stdout.String()
	if !strings.HasPrefix(got, "dry-run ok\t") {
		t.Fatalf("expected stdout to start with %q, got %q", "dry-run ok\t", got)
	}
	if !strings.Contains(got, "\ttmux=") {
		t.Fatalf("expected stdout to carry tmux= field, got %q", got)
	}
	if !strings.Contains(got, "\ttmux-mcp=") {
		t.Fatalf("expected stdout to carry tmux-mcp= field, got %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("expected stdout to end with newline, got %q", got)
	}
	// One line, three fields, exactly — matches the -probe shape so
	// orchestrators can split on \t with confidence.
	line := strings.TrimSuffix(got, "\n")
	if parts := strings.Split(line, "\t"); len(parts) != 3 {
		t.Fatalf("expected 3 tab-separated fields, got %d in %q", len(parts), line)
	}
}

// TestDryRun_InvalidLogFormat pins the precedence rule: -log-format
// validation runs before any -dry-run bootstrap, so a bogus format value
// must short-circuit with the same errInvalidLogFormat path the
// non-dry-run flow uses. Otherwise an operator could discover a typo
// only after starting the JSON-RPC loop in production.
func TestDryRun_InvalidLogFormat(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-dry-run", "-log-format=xml"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for invalid -log-format with -dry-run, got nil")
	}
	if !errors.Is(err, errInvalidLogFormat) {
		t.Fatalf("expected errInvalidLogFormat wrap, got %v", err)
	}
	// Stdout must stay clean — the success line is reserved for actual
	// bootstrap success. A failed dry-run leaves stdout empty so a
	// shell wrapper can use stdout's contents alone to gate downstream
	// steps.
	if stdout.Len() != 0 {
		t.Fatalf("expected stdout untouched on validation failure, got %q", stdout.String())
	}
}

// TestDryRun_InvalidSocket confirms a startup error from
// tmuxctl.NewWithSocket (here: a parent directory that does not exist)
// is surfaced rather than swallowed. The whole point of -dry-run is to
// fail loudly when the config is wrong.
func TestDryRun_InvalidSocket(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	var stdout, stderr bytes.Buffer
	err := run(
		[]string{"-dry-run", "-socket=/nonexistent/dir/sock"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if err == nil {
		t.Fatal("expected error for missing socket parent, got nil")
	}
	// tmuxctl.NewWithSocket returns the "parent directory does not
	// exist" error verbatim — pin a phrase from it so a future
	// rephrasing trips this test instead of silently breaking the
	// startup-validation contract.
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected missing-parent error, got %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected stdout untouched on bootstrap failure, got %q", stdout.String())
	}
}

// countingReader wraps an io.Reader and tallies how many bytes were
// pulled out of it. Used by TestDryRun_DoesNotReadStdin to assert that
// run() never touches stdin on the dry-run path.
type countingReader struct {
	src  io.Reader
	read int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	r.read += n
	return n, err
}

// TestDryRun_DoesNotReadStdin is the load-bearing contract test: a
// dry-run must NEVER consume bytes from stdin. We wrap the supplied
// stdin in a countingReader so we can observe exactly how many bytes
// run() pulled — zero on the dry-run path. If dry-run accidentally
// fell through to server.Serve, the frame buffered in stdin would be
// drained and the counter would be non-zero.
func TestDryRun_DoesNotReadStdin(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	const frame = `{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n"
	cr := &countingReader{src: strings.NewReader(frame)}

	var stdout, stderr bytes.Buffer
	if err := run([]string{"-dry-run"}, cr, &stdout, &stderr); err != nil {
		t.Fatalf("run(-dry-run): %v (stderr=%q)", err, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "dry-run ok\t") {
		t.Fatalf("expected dry-run ok line, got %q", stdout.String())
	}
	if cr.read != 0 {
		t.Fatalf("dry-run consumed %d bytes from stdin; want 0", cr.read)
	}
	// Belt-and-braces: drain the underlying reader and confirm the
	// full frame is still there. If run() had taken even one byte
	// the strict-equality compare would fail.
	leftover, err := io.ReadAll(cr.src)
	if err != nil {
		t.Fatalf("read leftover stdin: %v", err)
	}
	if string(leftover) != frame {
		t.Fatalf("expected leftover stdin %q (untouched), got %q",
			frame, string(leftover))
	}
}

// TestDryRunFlag_AcceptedAndDocumented guards the operator-visible CLI
// surface: the flag must show up in -help and a bogus boolean form must
// be rejected at parse time so a typo in a unit file never silently
// disables the dry-run.
func TestDryRunFlag_AcceptedAndDocumented(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-help"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil && err.Error() != "flag: help requested" {
		t.Fatalf("run(-help): unexpected error %v", err)
	}
	if !strings.Contains(stderr.String(), "-dry-run") {
		t.Fatalf("expected -dry-run in usage block, got %q", stderr.String())
	}
}

// TestAllowlistFlag_UnknownAborts pins the operator-facing contract:
// when -allowlist contains a name no registered tool matches, run()
// must surface the "unknown tools in -allowlist" error before any
// stdin is consumed. This is the typo guard — silently disabling a
// tool because of a mistyped name would betray operator intent.
func TestAllowlistFlag_UnknownAborts(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	var stdout, stderr bytes.Buffer
	err := run([]string{"-allowlist=does_not_exist"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for unknown tool in -allowlist, got nil")
	}
	if !strings.Contains(err.Error(), "unknown tools") {
		t.Fatalf("expected error to mention unknown tools, got %v", err)
	}
	// stdout must stay clean — the JSON-RPC loop never opened, and a
	// shell wrapper relying on stdout being either empty or a clean
	// success line should not see diagnostic noise.
	if stdout.Len() != 0 {
		t.Fatalf("expected stdout untouched on validation failure, got %q", stdout.String())
	}
}

// TestAllowlistFlag_AcceptedAndDocumented confirms -allowlist is wired
// through to the parser (i.e. registered with FlagSet) and that its
// help line ships in the -help usage block. Behavioural coverage for
// the underlying filter lives in internal/server/tools_allowlist_test.go;
// here we just guard the CLI surface so a future rename trips a test.
func TestAllowlistFlag_AcceptedAndDocumented(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-help"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil && err.Error() != "flag: help requested" {
		t.Fatalf("run(-help): unexpected error %v", err)
	}
	if !strings.Contains(stderr.String(), "-allowlist") {
		t.Fatalf("expected -allowlist in usage block, got %q", stderr.String())
	}
}

// TestTmuxBinFlag_AcceptedAndDocumented guards the CLI surface for
// -tmux-bin: the flag must appear in the -help usage block so
// operators discover it without reading the source. Behavioural
// coverage for the underlying validation lives in
// internal/tmuxctl/tmuxctl_test.go (TestWithBinary_*).
func TestTmuxBinFlag_AcceptedAndDocumented(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-help"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil && err.Error() != "flag: help requested" {
		t.Fatalf("run(-help): unexpected error %v", err)
	}
	if !strings.Contains(stderr.String(), "-tmux-bin") {
		t.Fatalf("expected -tmux-bin in usage block, got %q", stderr.String())
	}
}

// TestTmuxBinFlag_RejectsRelative pins the operator-facing contract:
// a relative -tmux-bin value is refused at startup, so a typo in a
// systemd unit or container env var surfaces as a clean diagnostic
// instead of an obscure "fork/exec" failure once the working
// directory shifts. We skip when tmux is not on PATH because the
// validation runs after the version gate would otherwise resolve a
// real binary.
func TestTmuxBinFlag_RejectsRelative(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-tmux-bin=relative/tmux"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for relative -tmux-bin, got nil")
	}
	if !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("expected 'must be absolute' phrase, got %v", err)
	}
}

// TestTmuxBinFlag_RejectsNonexistent confirms the validation rejects
// a non-existent path with the documented "tmux binary %q not
// executable" diagnostic. Without this, an operator who fat-fingered
// the path would only discover the mistake once the first tmux
// invocation failed at runtime.
func TestTmuxBinFlag_RejectsNonexistent(t *testing.T) {
	t.Parallel()
	bogus := "/nonexistent-tmux-binary-path-for-flag-test"
	var stdout, stderr bytes.Buffer
	err := run([]string{"-tmux-bin=" + bogus}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for nonexistent -tmux-bin, got nil")
	}
	if !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("expected 'not executable' phrase, got %v", err)
	}
	if !strings.Contains(err.Error(), bogus) {
		t.Fatalf("expected error to quote the path %q, got %v", bogus, err)
	}
}

// TestTmuxBinFlag_EmptyKeepsPathBehaviour confirms that the default
// -tmux-bin="" preserves the legacy "resolve tmux from $PATH"
// behaviour. -dry-run takes the same code path as serving stdio
// short of opening the JSON-RPC loop, so a successful dry-run with
// no override is the cleanest end-to-end signal that the empty
// default did not regress.
func TestTmuxBinFlag_EmptyKeepsPathBehaviour(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-dry-run"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("run(-dry-run): %v (stderr=%q)", err, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "dry-run ok\t") {
		t.Fatalf("expected dry-run ok line, got %q", stdout.String())
	}
}

// TestTmuxBinEnvFallback covers the env-var path: when -tmux-bin is
// not passed but TMUX_MCP_TMUX_BIN is set to a relative value, run()
// must surface the same validation error rather than silently fall
// through to PATH. Mirrors TestSocketEnvFallback for -socket.
func TestTmuxBinEnvFallback(t *testing.T) {
	t.Setenv("TMUX_MCP_TMUX_BIN", "relative/tmux-from-env")
	var stdout, stderr bytes.Buffer
	err := run(nil, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for relative tmux bin from env")
	}
	if !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestTmuxBinFlag_WinsOverEnv pins the documented precedence: the
// flag overrides the env var. We seed a bogus env value (would fail
// validation if used) and pass an explicit empty flag value — the
// flag's empty string must take effect, falling back to PATH. With
// tmux on PATH and -dry-run, that combination should succeed.
func TestTmuxBinFlag_WinsOverEnv(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	t.Setenv("TMUX_MCP_TMUX_BIN", "/nonexistent/tmux-from-env")
	var stdout, stderr bytes.Buffer
	err := run(
		[]string{"-tmux-bin=", "-dry-run"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if err != nil {
		t.Fatalf("expected -tmux-bin= to override env var, got: %v stderr=%q",
			err, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "dry-run ok\t") {
		t.Fatalf("expected dry-run ok line, got %q", stdout.String())
	}
}

// TestSessionPrefixFlag_RejectsInvalid pins the operator-facing contract
// for -session-prefix: the validator must run before any tmux state is
// created, and any value the [server.ValidateSessionPrefix] function
// rejects (bad regex, trailing dash, no room for a session name) must
// surface as a clean startup error wrapping [errInvalidSessionPrefix].
// Stdout must stay clean on the failure path so a shell wrapper relying
// on stdout being either empty or a clean line never sees diagnostic
// noise. The table covers the documented rules so a future regression
// in either the validator or its wiring trips a single test.
func TestSessionPrefixFlag_RejectsInvalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		flag string
	}{
		{"trailing-dash", "-session-prefix=agent-"},
		{"whitespace", "-session-prefix=agent name_"},
		{"shell-meta", "-session-prefix=agent$_"},
		{"colon", "-session-prefix=agent:_"},
		{"slash", "-session-prefix=agent/_"},
		{"too-long", "-session-prefix=" + strings.Repeat("a", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			err := run([]string{tc.flag}, strings.NewReader(""), &stdout, &stderr)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.flag)
			}
			if !errors.Is(err, errInvalidSessionPrefix) {
				t.Fatalf("expected errInvalidSessionPrefix for %s, got %v", tc.flag, err)
			}
			if !strings.Contains(stderr.String(), "tmux-mcp: ") {
				t.Fatalf("expected stderr to carry diagnostic for %s, got %q", tc.flag, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected stdout untouched for %s, got %q", tc.flag, stdout.String())
			}
		})
	}
}

// TestSessionPrefixFlag_AcceptedAndDocumented confirms -session-prefix
// is wired through to the parser (i.e. registered with FlagSet) and
// that its help line ships in the -help usage block. Behavioural
// coverage for the runtime path lives in
// internal/server/tools_session_prefix_test.go; here we just guard the
// CLI surface so a future rename trips a test.
func TestSessionPrefixFlag_AcceptedAndDocumented(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"-help"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil && err.Error() != "flag: help requested" {
		t.Fatalf("run(-help): unexpected error %v", err)
	}
	if !strings.Contains(stderr.String(), "-session-prefix") {
		t.Fatalf("expected -session-prefix in usage block, got %q", stderr.String())
	}
}

// TestSessionPrefixFlag_EmptyAcceptedAtDryRun pins the back-compat
// contract: the empty default must NOT trip the validator, and a
// -dry-run with no -session-prefix must succeed end-to-end so existing
// deployments see no behaviour change.
func TestSessionPrefixFlag_EmptyAcceptedAtDryRun(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-dry-run"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("run(-dry-run): %v (stderr=%q)", err, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "dry-run ok\t") {
		t.Fatalf("expected dry-run ok line, got %q", stdout.String())
	}
}

// TestSessionPrefixFlag_ValidAcceptedAtDryRun confirms a valid
// -session-prefix value walks the entire bootstrap successfully, so
// the validator does not over-reject conventional separators
// ("agent_alice_", "agent_alice"). The dry-run path covers every
// startup hook the prefix depends on (tmux init, audit open, tool
// surface build) without committing to the JSON-RPC loop.
func TestSessionPrefixFlag_ValidAcceptedAtDryRun(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	for _, p := range []string{"agent_alice_", "agent_alice", "Agent42_", "a"} {
		t.Run(p, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run([]string{"-dry-run", "-session-prefix=" + p}, strings.NewReader(""), &stdout, &stderr)
			if err != nil {
				t.Fatalf("run(-dry-run -session-prefix=%q): %v (stderr=%q)", p, err, stderr.String())
			}
			if !strings.HasPrefix(stdout.String(), "dry-run ok\t") {
				t.Fatalf("expected dry-run ok line for prefix %q, got %q", p, stdout.String())
			}
		})
	}
}
