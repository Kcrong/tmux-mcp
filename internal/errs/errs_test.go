package errs

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestCodeOf(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{
			name: "nil falls back to internal",
			err:  nil,
			want: CodeInternal,
		},
		{
			name: "plain error falls back to internal",
			err:  errors.New("something else broke"),
			want: CodeInternal,
		},
		{
			name: "ErrSessionNotFound direct",
			err:  ErrSessionNotFound,
			want: CodeSessionNotFound,
		},
		{
			name: "ErrSessionNotFound wrapped",
			err:  fmt.Errorf("%w: %q", ErrSessionNotFound, "alpha"),
			want: CodeSessionNotFound,
		},
		{
			name: "ErrSessionNotFound double-wrapped",
			err:  fmt.Errorf("kill: %w", fmt.Errorf("lookup: %w", ErrSessionNotFound)),
			want: CodeSessionNotFound,
		},
		{
			name: "ErrTmuxVersionUnsupported direct",
			err:  ErrTmuxVersionUnsupported,
			want: CodeTmuxVersionUnsupported,
		},
		{
			name: "ErrTmuxVersionUnsupported wrapped",
			err:  fmt.Errorf("%w: tmux %s", ErrTmuxVersionUnsupported, "2.6"),
			want: CodeTmuxVersionUnsupported,
		},
		{
			name: "ErrTimeout direct",
			err:  ErrTimeout,
			want: CodeTimeout,
		},
		{
			name: "ErrTimeout wrapped",
			err:  fmt.Errorf("wait_for_stable: %w after 5s", ErrTimeout),
			want: CodeTimeout,
		},
		{
			name: "context.Canceled",
			err:  context.Canceled,
			want: CodeContextCancelled,
		},
		{
			name: "context.DeadlineExceeded",
			err:  context.DeadlineExceeded,
			want: CodeContextCancelled,
		},
		{
			name: "context.Canceled wrapped",
			err:  fmt.Errorf("rpc: %w", context.Canceled),
			want: CodeContextCancelled,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CodeOf(tc.err); got != tc.want {
				t.Fatalf("CodeOf(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestCodes_Stable pins the wire codes. Bumping any of these is a
// breaking change for clients and must be done deliberately.
func TestCodes_Stable(t *testing.T) {
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"CodeInvalidParams", CodeInvalidParams, -32602},
		{"CodeInternal", CodeInternal, -32603},
		{"CodeSessionNotFound", CodeSessionNotFound, -32000},
		{"CodeTmuxVersionUnsupported", CodeTmuxVersionUnsupported, -32001},
		{"CodeTimeout", CodeTimeout, -32002},
		{"CodeContextCancelled", CodeContextCancelled, -32003},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("%s = %d, want %d (changing this is a wire-breaking change)",
					tc.name, tc.got, tc.want)
			}
		})
	}
}

// TestSentinels_Distinct guards against accidentally pointing two of the
// public sentinels at the same underlying error value.
func TestSentinels_Distinct(t *testing.T) {
	if errors.Is(ErrSessionNotFound, ErrTmuxVersionUnsupported) ||
		errors.Is(ErrSessionNotFound, ErrTimeout) ||
		errors.Is(ErrTmuxVersionUnsupported, ErrTimeout) {
		t.Fatal("sentinel errors must be distinct")
	}
}
