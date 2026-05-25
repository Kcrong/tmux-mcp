//go:build !linux

package tmuxctl

import "testing"

// attachFakeClient is a stub for non-Linux builds; the happy-path test
// itself skips on these platforms via runtime.GOOS, so this function
// only exists to keep the symbol resolvable when the build tag for the
// real helper does not match.
func attachFakeClient(t *testing.T, _ *Controller, _ string) {
	t.Helper()
	t.Skip("attachFakeClient: pty helper only implemented on linux")
}
