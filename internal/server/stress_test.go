//go:build stress

// Stress tests for the MCP tool surface. These deliberately push the
// session lifecycle, JSON-RPC dispatcher, and capture path harder than
// the normal test suite to surface goroutine, fd, and memory leaks.
//
// Build-tag protected so `go test ./...` ignores this file. Run via:
//
//	go test -tags=stress -timeout=15m -count=1 ./internal/server/... -run="TestStress"
//
// The companion workflow at .github/workflows/stress.yml runs this on
// schedule and on workflow_dispatch.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Knob defaults are sized to finish a full stress run in ~2 minutes on
// a GHA ubuntu-latest runner. Override via env when iterating locally.
const (
	defaultCreateKillIters    = 200
	defaultConcurrentWorkers  = 50
	defaultConcurrentPerWork  = 50
	defaultLongScrollbackSeq  = 5000
	defaultGoroutineSlackProc = 5
)

// envInt reads an integer override from the environment, falling back
// to def when unset or unparseable. Lets the workflow tune iteration
// counts without code changes.
func envInt(name string, def int) int {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		return def
	}
	return n
}

// memSampler periodically dumps runtime.MemStats and goroutine counts
// to stderr so the workflow log captures a full picture of pressure
// over the course of a test. Returns a stop function the caller must
// invoke before asserting on final goroutine counts; otherwise the
// sampler itself shows up as a stray goroutine.
func memSampler(t *testing.T, label string, every time.Duration) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(every)
		defer tick.Stop()
		dump := func(tag string) {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			fmt.Fprintf(os.Stderr,
				"[stress %s %s] goroutines=%d alloc=%dKB sys=%dKB heap_objs=%d gc=%d\n",
				label, tag,
				runtime.NumGoroutine(),
				m.Alloc/1024, m.Sys/1024,
				m.HeapObjects, m.NumGC,
			)
		}
		dump("start")
		for {
			select {
			case <-done:
				dump("end")
				return
			case <-tick.C:
				dump("tick")
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// stressCall is a thin wrapper around tools.Handle("tools/call", ...)
// that fails the test on rpcError. Mirrors the helper used in
// tools_test.go so individual cases stay readable.
func stressCall(t *testing.T, tools *Tools, ctx context.Context, name string, args any) any {
	t.Helper()
	params := mustJSON(t, map[string]any{"name": name, "arguments": args})
	res, rerr := tools.Handle(ctx, "tools/call", params)
	if rerr != nil {
		t.Fatalf("%s: %s", name, rerr.Message)
	}
	return res
}

// settleGoroutines drives a few GC + yield rounds so transient
// goroutines parked in syscalls or finalizers have a chance to exit
// before we sample runtime.NumGoroutine. Without this the leak
// assertion races against tmux child processes that just returned.
func settleGoroutines() {
	for i := 0; i < 5; i++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(50 * time.Millisecond)
	}
}

// TestStressSessionCreateKill hammers the create→kill cycle to flush
// out leaks in tmuxctl.Controller (sockets, child procs) and the
// snapshot store (per-session entries). Asserts that goroutine count
// is bounded after we tear everything down.
func TestStressSessionCreateKill(t *testing.T) {
	skipIfNoTmux(t)
	iters := envInt("STRESS_CREATE_KILL_ITERS", defaultCreateKillIters)
	slack := envInt("STRESS_GOROUTINE_SLACK", defaultGoroutineSlackProc)

	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	stop := memSampler(t, "create-kill", 5*time.Second)
	defer stop()

	settleGoroutines()
	startGo := runtime.NumGoroutine()
	t.Logf("starting create/kill stress: iters=%d goroutines=%d", iters, startGo)

	for i := 0; i < iters; i++ {
		name := fmt.Sprintf("ck-%d", i)
		stressCall(t, tools, ctx, "session_create", map[string]any{
			"name": name, "command": "/bin/sh", "width": 80, "height": 20,
		})
		stressCall(t, tools, ctx, "session_kill", map[string]any{"name": name})
	}

	settleGoroutines()
	endGo := runtime.NumGoroutine()
	delta := endGo - startGo
	t.Logf("create/kill stress complete: iters=%d goroutines start=%d end=%d delta=%d",
		iters, startGo, endGo, delta)

	if delta > slack {
		// Dump goroutine stacks to stderr so the workflow artifact has
		// something actionable when this fires.
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		fmt.Fprintf(os.Stderr, "[stress create-kill] goroutine dump:\n%s\n", buf[:n])
		t.Fatalf("goroutine leak suspected: start=%d end=%d delta=%d slack=%d",
			startGo, endGo, delta, slack)
	}
}

// TestStressConcurrentCalls fans out workers that each drive a long
// sequence of send_keys/capture against a shared session, exercising
// the dispatcher's locking and the controller's serialization. Any
// data race or fd leak should crash the test under -race.
func TestStressConcurrentCalls(t *testing.T) {
	skipIfNoTmux(t)
	workers := envInt("STRESS_CONCURRENT_WORKERS", defaultConcurrentWorkers)
	perWorker := envInt("STRESS_CONCURRENT_PER_WORKER", defaultConcurrentPerWork)

	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	stop := memSampler(t, "concurrent", 5*time.Second)
	defer stop()

	stressCall(t, tools, ctx, "session_create", map[string]any{
		"name": "cc", "command": "/bin/sh", "width": 120, "height": 40,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "cc"},
			}))
	})

	t.Logf("starting concurrent stress: workers=%d perWorker=%d", workers, perWorker)

	var sends, captures atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if i%2 == 0 {
					params := mustJSON(t, map[string]any{
						"name": "send_keys",
						"arguments": map[string]any{
							"session": "cc",
							"keys":    []string{fmt.Sprintf("# w%d-i%d", worker, i)},
						},
					})
					if _, rerr := tools.Handle(ctx, "tools/call", params); rerr != nil {
						t.Errorf("worker %d send_keys %d: %s", worker, i, rerr.Message)
						return
					}
					sends.Add(1)
				} else {
					params := mustJSON(t, map[string]any{
						"name": "capture",
						"arguments": map[string]any{
							"session": "cc",
						},
					})
					if _, rerr := tools.Handle(ctx, "tools/call", params); rerr != nil {
						t.Errorf("worker %d capture %d: %s", worker, i, rerr.Message)
						return
					}
					captures.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()

	expectedSends := int64(workers * ((perWorker + 1) / 2))
	expectedCaptures := int64(workers * (perWorker / 2))
	if sends.Load() != expectedSends {
		t.Errorf("sends: got %d, want %d", sends.Load(), expectedSends)
	}
	if captures.Load() != expectedCaptures {
		t.Errorf("captures: got %d, want %d", captures.Load(), expectedCaptures)
	}
	t.Logf("concurrent stress complete: sends=%d captures=%d",
		sends.Load(), captures.Load())
}

// TestStressLongScrollback writes a long stream of lines to a session
// and verifies the capture path's max_lines cap holds when the
// scrollback grows large. Guards against an unbounded buffer slipping
// through capCaptureBody.
func TestStressLongScrollback(t *testing.T) {
	skipIfNoTmux(t)
	seqN := envInt("STRESS_LONG_SCROLLBACK_SEQ", defaultLongScrollbackSeq)

	tools := newTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	stop := memSampler(t, "long-scrollback", 5*time.Second)
	defer stop()

	stressCall(t, tools, ctx, "session_create", map[string]any{
		"name": "ls", "command": "/bin/sh", "width": 80, "height": 24,
	})
	t.Cleanup(func() {
		_, _ = tools.Handle(context.Background(), "tools/call",
			mustJSON(t, map[string]any{
				"name":      "session_kill",
				"arguments": map[string]any{"name": "ls"},
			}))
	})

	stressCall(t, tools, ctx, "send_keys", map[string]any{
		"session": "ls",
		"keys":    []string{fmt.Sprintf("seq 1 %d", seqN), "Enter"},
	})
	_ = extractText(t, stressCall(t, tools, ctx, "wait_for_stable", map[string]any{
		"session": "ls", "quiet_ms": 600, "timeout_ms": 60000,
	}))

	// First: explicit small cap should always truncate cleanly.
	captureText := extractText(t, stressCall(t, tools, ctx, "capture", map[string]any{
		"session":   "ls",
		"mode":      "scrollback",
		"max_lines": 100,
	}))
	var capObj map[string]any
	if err := json.Unmarshal([]byte(captureText), &capObj); err != nil {
		t.Fatalf("decode capped capture: %v\nbody=%s", err, captureText)
	}
	body, _ := capObj["snapshot"].(string)
	lines := strings.Split(body, "\n")
	if len(lines) > 100 {
		t.Fatalf("capped scrollback exceeded max_lines: got %d lines", len(lines))
	}
	if truncated, _ := capObj["truncated"].(bool); !truncated {
		t.Fatalf("expected truncated=true with explicit cap")
	}

	// Then: default cap (max_lines=0) must not let us pull the entire
	// scrollback uncapped. Cap is defaultScrollbackMaxLines (5000).
	defaultCap := extractText(t, stressCall(t, tools, ctx, "capture", map[string]any{
		"session": "ls",
		"mode":    "scrollback",
	}))
	var defObj map[string]any
	if err := json.Unmarshal([]byte(defaultCap), &defObj); err != nil {
		t.Fatalf("decode default capture: %v\nbody=%s", err, defaultCap)
	}
	defBody, _ := defObj["snapshot"].(string)
	defLines := strings.Split(defBody, "\n")
	if len(defLines) > defaultScrollbackMaxLines {
		t.Fatalf("default cap leaked: got %d lines, want <= %d",
			len(defLines), defaultScrollbackMaxLines)
	}
	t.Logf("long scrollback stress complete: capped=%d default=%d",
		len(lines), len(defLines))
}
