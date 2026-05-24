//go:build !windows

package tmuxctl

import (
	"os"
	"syscall"
)

// signalTable on Unix maps every name SignalNames() advertises to its
// concrete syscall.Signal. SIGUSR1 and SIGUSR2 are POSIX-only — the
// Windows build maintains a smaller table and rejects those two names
// at runtime via platformRejectSignal (see signal_windows.go).
var signalTable = map[string]os.Signal{
	"TERM": syscall.SIGTERM,
	"HUP":  syscall.SIGHUP,
	"INT":  syscall.SIGINT,
	"QUIT": syscall.SIGQUIT,
	"USR1": syscall.SIGUSR1,
	"USR2": syscall.SIGUSR2,
	"KILL": syscall.SIGKILL,
}

// platformRejectSignal is a no-op on Unix: every name in SignalNames()
// is resolvable via signalTable. The Windows variant uses this hook to
// surface a friendly "not supported on Windows" error for SIGUSR1 /
// SIGUSR2 so callers don't see a misleading whitelist-mismatch message.
func platformRejectSignal(string) error { return nil }
