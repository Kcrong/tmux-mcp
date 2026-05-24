//go:build windows

package tmuxctl

import (
	"fmt"
	"os"
	"syscall"
)

// signalTable on Windows omits SIGUSR1 / SIGUSR2 because those
// constants do not exist in package syscall on GOOS=windows — they
// are POSIX-only. SignalNames() still advertises them so the JSON-RPC
// schema is identical across platforms; platformRejectSignal turns a
// runtime request for either into a clean "not supported" error.
//
// In practice tmux itself does not run on Windows — the Windows build
// exists so users on a Windows host can still invoke `go install` /
// download a release binary and connect to a tmux server over ssh.
// Trying to fire USR1/USR2 against a remote tmux from a Windows
// caller would be ambiguous anyway: even if we mapped the names to
// some Windows event we'd have nothing to deliver them to.
var signalTable = map[string]os.Signal{
	"TERM": syscall.SIGTERM,
	"HUP":  syscall.SIGHUP,
	"INT":  syscall.SIGINT,
	"QUIT": syscall.SIGQUIT,
	"KILL": syscall.SIGKILL,
}

// platformRejectSignal returns a friendly diagnostic when a Windows
// caller asks for SIGUSR1 / SIGUSR2 — without this hook resolveSignal
// would just say "not in whitelist" while the whitelist (SignalNames)
// still lists those two names, which is confusing.
func platformRejectSignal(name string) error {
	switch name {
	case "USR1", "USR2":
		return fmt.Errorf("signal SIG%s not supported on Windows", name)
	}
	return nil
}
