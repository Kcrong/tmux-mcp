package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// slowAcquireThreshold is the wait duration above which a single acquire
// emits a single slog.Warn. Below it, blocked acquires are silent so a
// briefly bursty client doesn't flood the logs — the warning only fires
// when the queue depth actually starts to hurt latency.
const slowAcquireThreshold = 100 * time.Millisecond

// callLimiter caps the number of in-flight tool calls. It is a thin
// wrapper around a buffered channel used as a counting semaphore: the
// channel's capacity is the concurrency ceiling, and each Acquire writes
// a token (blocking when the channel is full) that Release later
// removes. A nil receiver, or one constructed with limit <= 0, behaves
// as a no-op so callers can wire the limiter into the dispatcher
// unconditionally and let the flag default to "off".
//
// The limiter is intentionally unaware of which method is being gated —
// the dispatcher decides which frames pass through it. See Serve() in
// jsonrpc.go for the policy (currently: only tools/call frames).
type callLimiter struct {
	// sem is the buffered semaphore. cap(sem) == configured limit;
	// len(sem) is the current in-flight count, which doubles as the
	// queue-depth metric we surface in the slow-acquire warning.
	sem chan struct{}
}

// newCallLimiter returns a callLimiter capped at limit concurrent
// holders. When limit <= 0 the returned value is nil — every method on
// a nil callLimiter is a no-op, which preserves the pre-flag behaviour
// (unbounded goroutines) without forcing every call site to branch on
// the flag value.
func newCallLimiter(limit int) *callLimiter {
	if limit <= 0 {
		return nil
	}
	return &callLimiter{sem: make(chan struct{}, limit)}
}

// Acquire blocks until a slot is available or ctx is cancelled.
//
// Back-pressure is the explicit policy: the dispatcher waits rather
// than rejecting, because rejecting would surprise an MCP client that
// has no concept of "server busy" and would treat the failure as the
// tool itself misbehaving. Honest queueing keeps observability simple
// (slow tool call → slow response) and preserves the contract.
//
// On a nil receiver, Acquire is an immediate success — that's the
// "limit unlimited" case.
//
// When the wait exceeds slowAcquireThreshold the limiter emits one
// slog.Warn with the request method and the queue depth (current
// in-flight count), so operators can spot a saturated server without
// being drowned in a log line per blocked frame.
//
// Cancellation: if ctx fires while waiting, Acquire returns a
// context-shaped error wrapped around errs.ErrTimeout so the
// dispatcher can map it to the existing CodeTimeout JSON-RPC code via
// errs.CodeOf. The caller MUST NOT call Release() in that case — no
// token was placed.
func (l *callLimiter) Acquire(ctx context.Context, method string) error {
	if l == nil {
		return nil
	}
	// Fast path: a slot is available right now. Skip the timer setup
	// and the warn-on-slow bookkeeping so the common case (uncontended)
	// stays cheap.
	select {
	case l.sem <- struct{}{}:
		return nil
	default:
	}
	// Slow path: wait, and warn once if the wait crosses the threshold.
	started := time.Now()
	warn := time.NewTimer(slowAcquireThreshold)
	defer warn.Stop()
	// warnC is the timer's receive channel; we nil it out after the
	// warning fires so the select stops considering it (a nil channel
	// blocks forever in a select). This avoids the trick of re-arming
	// the timer with a fake huge duration just to keep the case alive.
	warnC := warn.C
	for {
		select {
		case l.sem <- struct{}{}:
			return nil
		case <-ctx.Done():
			// Wrap ErrTimeout so the dispatcher's existing
			// errs.CodeOf path maps it to CodeTimeout. We propagate
			// ctx.Err() too so the caller can still distinguish
			// cancellation cause via errors.Is(_, context.Canceled).
			return fmt.Errorf("%w: %s blocked %s waiting for concurrency slot: %w",
				errs.ErrTimeout, method, time.Since(started).Round(time.Millisecond), ctx.Err())
		case <-warnC:
			// One warning per slow acquire. The queue depth is
			// len(sem) — how many slots are currently held — which
			// is the most useful single number for "is the server
			// saturated?". Capacity is included so operators don't
			// have to cross-reference the flag value.
			slog.Warn("rpc concurrency wait",
				"method", method,
				"waited_ms", slowAcquireThreshold.Milliseconds(),
				"in_flight", len(l.sem),
				"limit", cap(l.sem),
			)
			// Mute the warn case for the rest of this acquire — the
			// budget is one warn line per blocked acquire. A nil
			// channel in a select case is never selectable, so the
			// loop now reduces to "wait for slot OR cancellation".
			warnC = nil
		}
	}
}

// Release returns a previously acquired slot. It must only be called by
// a goroutine that observed a nil error from Acquire — calling Release
// without a matching Acquire would underflow the semaphore (drain a
// token that was never placed) and let extra goroutines slip past the
// limit on the next round. A nil receiver is a no-op, mirroring
// Acquire's "limit unlimited" handling.
func (l *callLimiter) Release() {
	if l == nil {
		return
	}
	select {
	case <-l.sem:
	default:
		// This branch is unreachable when Acquire/Release are paired
		// correctly. We deliberately do NOT silently absorb the
		// imbalance: dropping a Release here would leak a slot
		// permanently, eventually deadlocking every future Acquire.
		// Panicking surfaces the pairing bug at the call site instead
		// of letting it manifest as a mysterious hang in production.
		panic("server: callLimiter.Release called without matching Acquire")
	}
}
