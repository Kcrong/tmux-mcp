//go:build linux

package tmuxctl

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// Linux ptmx ioctl constants. Hard-coded here so the test stays free of
// build-tag-only dependencies on golang.org/x/sys/unix — the rest of the
// package builds without that import. These values are stable across
// Linux versions (defined in <asm-generic/ioctls.h>): TIOCSPTLCK clears
// the slave pty lock so a child can attach, TIOCGPTN reads the pty
// number so we can construct the /dev/pts/<n> path the child opens.
const (
	tIOCSPTLCK = 0x40045431
	tIOCGPTN   = 0x80045430
)

// openPTYPair returns (master, slaveName) for a freshly-allocated pty.
// On error the master is closed before returning so callers can use
// `t.Fatalf` without leaking a fd.
func openPTYPair() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, "", err
	}
	var unlock int
	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL, master.Fd(), tIOCSPTLCK,
		uintptr(unsafe.Pointer(&unlock)),
	); errno != 0 {
		_ = master.Close()
		return nil, "", fmt.Errorf("TIOCSPTLCK: %w", errno)
	}
	var ptn uint32
	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL, master.Fd(), tIOCGPTN,
		uintptr(unsafe.Pointer(&ptn)),
	); errno != 0 {
		_ = master.Close()
		return nil, "", fmt.Errorf("TIOCGPTN: %w", errno)
	}
	return master, fmt.Sprintf("/dev/pts/%d", ptn), nil
}

// attachFakeClient spawns `tmux -S <socket> attach -t <session>` on a
// freshly-allocated pty so the controller's tmux server reports at
// least one attached client to list-clients. The helper:
//
//   - opens a private master/slave pty pair via openPTYPair;
//   - launches the tmux child with the slave as its controlling tty
//     (Setsid + Setctty so tmux sees a real terminal and does not
//     refuse to attach);
//   - drains the master fd in a goroutine so tmux's redraw chatter
//     does not back-pressure the child;
//   - polls list-clients until the new client shows up so the test
//     does not race the kernel scheduler;
//   - registers a t.Cleanup that closes the pty and reaps the child
//     so a panicked test does not leak a tmux process or fd.
//
// The polling deadline is short (a few seconds) because attach is a
// local IPC operation; a slower runner indicates a deeper problem the
// caller wants to see surfaced as a Fatal rather than silently
// succeeding without an actual client.
func attachFakeClient(t *testing.T, c *Controller, session string) {
	t.Helper()
	master, slaveName, err := openPTYPair()
	if err != nil {
		t.Fatalf("openPTYPair: %v", err)
	}
	slave, err := os.OpenFile(slaveName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		t.Fatalf("open slave %q: %v", slaveName, err)
	}

	cmd := exec.Command(c.bin, "-S", c.Socket(), "attach", "-t", session)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.Env = append(os.Environ(), "TERM=screen")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		// With Setsid+Setctty the kernel uses fd 0 of the child as the
		// controlling tty source — we already point stdin at the slave
		// so Ctty=0 is correct here.
		Ctty: 0,
	}
	if err := cmd.Start(); err != nil {
		_ = slave.Close()
		_ = master.Close()
		t.Fatalf("start tmux attach: %v", err)
	}
	// Drain the master so tmux's screen redraws don't fill the kernel
	// pty buffer and stall the child. We discard output — the test only
	// cares that the client exists, not what it sees.
	go func() { _, _ = io.Copy(io.Discard, master) }()

	// Wait for tmux to register the client. attach is fast (local IPC)
	// so a 5s budget is generous; a longer wait usually means tmux
	// itself failed to come up and we want a loud Fatal anyway.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		clients, lcErr := c.ListClients(ctx, "")
		cancel()
		if lcErr == nil && len(clients) > 0 {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			_ = slave.Close()
			_ = master.Close()
			if lcErr != nil {
				t.Fatalf("waiting for fake client: %v", lcErr)
			}
			t.Fatal("waiting for fake client: never registered")
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Cleanup(func() {
		// Send 'q' through the master so tmux's chooser-mode (if it
		// happens to be open) exits cleanly; then kill the attach
		// process unconditionally so we don't depend on the exact
		// chooser state.
		_, _ = master.Write([]byte("q"))
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = slave.Close()
		_ = master.Close()
	})
}
