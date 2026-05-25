package tmuxctl

import (
	"context"
	"errors"
	"fmt"

	"github.com/Kcrong/tmux-mcp/internal/errs"
)

// SuspendClientOpts captures the optional knobs `tmux suspend-client`
// accepts. Today tmux only exposes `-t target-client`, but a struct
// keeps the boundary additive — a future PR adding (e.g.) a signal
// override would extend the struct rather than break every caller's
// argument list.
type SuspendClientOpts struct {
	// TargetClient names a specific client to suspend (`-t TARGET`).
	// Empty means "no -t flag", which lands tmux on its built-in
	// "current client" lookup. On the headless servers tmux-mcp owns
	// that lookup typically returns "no current client" stderr and the
	// boundary maps that to a clean no-op (see SuspendClient below).
	TargetClient string
}

// SuspendClient drives `tmux suspend-client [-t target-client]`. The
// command sends SIGTSTP to the named (or current) client process so
// the user can resume it with `fg` after running other shell
// commands. Useful for an agent that wants to politely yield the
// terminal back to its operator without tearing the session down — a
// strictly less destructive sibling of `detach_client`, which
// disconnects the client entirely.
//
// Flag mapping:
//   - opts.TargetClient != "" → -t TARGET-CLIENT (suspend that one client)
//   - opts.TargetClient == "" → no -t flag, tmux suspends the "current"
//     client (the one running the suspend-client invocation)
//
// On the headless tmux servers tmux-mcp typically owns there is no
// "current client" because nobody is attached. tmux signals that by
// emitting "no current client" stderr and exiting non-zero. Mapping
// that case onto a clean nil keeps a fire-and-forget suspend (e.g.
// "suspend whoever is watching this session, if anyone") looking like
// a clean success rather than an error the caller must substring-match.
//
// A named-but-missing client (`-t <name>` that tmux can't resolve)
// surfaces as a wrapped errs.ErrSessionNotFound so the dispatcher's
// existing CodeSessionNotFound path catches it — mirroring the
// contract list_clients / session_kill / detach_client uphold for
// "the target you named does not exist".
//
// The "no current client" / "can't find client" matchers are shared
// with detach_client (they live in detach_client.go alongside that
// tool's controller method) — the contract is identical between the
// two tools, and reusing the helpers keeps the recognition logic in
// one place. A future bind_client / refresh_client / lock_client tool
// would do the same.
func (c *Controller) SuspendClient(ctx context.Context, opts SuspendClientOpts) error {
	args := []string{"suspend-client"}
	if opts.TargetClient != "" {
		args = append(args, "-t", opts.TargetClient)
	}
	if _, err := c.run(ctx, args...); err != nil {
		// Headless server with nothing attached: tmux emits "no current
		// client" when suspend-client can't find anyone to suspend.
		// Map onto nil so a fire-and-forget suspend against an empty
		// roster looks like a clean success.
		if isNoCurrentClientMsg(err.Error()) {
			return nil
		}
		// Named-but-missing client: wrap into errs.ErrSessionNotFound
		// so the dispatcher's existing CodeSessionNotFound path catches
		// it. Guard against double-wrapping in case a future run() ever
		// starts recognising the client-name variant itself.
		if !errors.Is(err, errs.ErrSessionNotFound) && isClientMissingMsg(err.Error()) {
			return fmt.Errorf("%s: %w", err.Error(), errs.ErrSessionNotFound)
		}
		return err
	}
	return nil
}
