package tmuxctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// minTmuxMajor and minTmuxMinor define the lowest supported tmux version.
// Older releases lack flags this package depends on (e.g. new-session -x/-y).
const (
	minTmuxMajor = 3
	minTmuxMinor = 0
)

// tmuxVersion is the parsed result of `tmux -V`.
type tmuxVersion struct {
	major int
	minor int
	// dev is true for unnumbered/dev builds (e.g. "tmux master",
	// "tmux next-3.5"). They are assumed to be at least minimum.
	dev bool
	// raw is the original string for diagnostics.
	raw string
}

// versionRE matches the numeric "<major>.<minor>" portion of a tmux version
// string, optionally preceded by a "next-" tag and optionally followed by a
// trailing letter (e.g. "3.4a"). Examples it accepts:
//
//	"3.4", "3.4a", "next-3.5", "next-3.5a"
var versionRE = regexp.MustCompile(`(?:next-)?(\d+)\.(\d+)[a-z]?`)

// parseTmuxVersion parses the output of `tmux -V`.
//
// Recognised forms:
//
//	"tmux 3.4"        -> {3, 4}
//	"tmux 3.4a"       -> {3, 4}
//	"tmux next-3.5"   -> {3, 5}
//	"tmux master"     -> dev build, treated as new enough
//	"tmux openbsd-7"  -> dev build, treated as new enough
//
// Anything that contains no recognisable token is returned as an error.
func parseTmuxVersion(out string) (tmuxVersion, error) {
	s := strings.TrimSpace(out)
	if s == "" {
		return tmuxVersion{}, errors.New("empty tmux -V output")
	}
	v := tmuxVersion{raw: s}
	// Trim a leading "tmux " banner if present; tmux normally prints it
	// but some forks may not.
	rest := strings.TrimPrefix(s, "tmux ")
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return tmuxVersion{}, fmt.Errorf("unrecognised tmux -V output: %q", s)
	}
	// Dev tags without a numeric component (e.g. "master") just pass.
	if m := versionRE.FindStringSubmatch(rest); m != nil {
		major, err := strconv.Atoi(m[1])
		if err != nil {
			return tmuxVersion{}, fmt.Errorf("parse major in %q: %w", s, err)
		}
		minor, err := strconv.Atoi(m[2])
		if err != nil {
			return tmuxVersion{}, fmt.Errorf("parse minor in %q: %w", s, err)
		}
		v.major = major
		v.minor = minor
		// "next-X.Y" is an unreleased build — treat as a dev version so
		// callers can decide to be lenient.
		if strings.Contains(rest, "next-") {
			v.dev = true
		}
		return v, nil
	}
	// No "<n>.<n>" anywhere. This is a dev/master build — let it through.
	v.dev = true
	return v, nil
}

// atLeast reports whether v is at least major.minor. Dev builds always pass.
func (v tmuxVersion) atLeast(major, minor int) bool {
	if v.dev {
		return true
	}
	if v.major != major {
		return v.major > major
	}
	return v.minor >= minor
}

// String renders a short numeric form for error messages.
func (v tmuxVersion) String() string {
	if v.dev && v.major == 0 && v.minor == 0 {
		return v.raw
	}
	return fmt.Sprintf("%d.%d", v.major, v.minor)
}

// checkTmuxVersion runs `tmux -V` on bin and verifies it meets the minimum
// supported version. The returned error is suitable for surfacing to the
// user — it names the offending version and points at upgrade commands.
func checkTmuxVersion(ctx context.Context, bin string) error {
	cmd := exec.CommandContext(ctx, bin, "-V")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("tmux -V failed: %s", msg)
	}
	// Some tmux builds print the banner on stderr; combine just in case.
	out := stdout.String()
	if strings.TrimSpace(out) == "" {
		out = stderr.String()
	}
	v, err := parseTmuxVersion(out)
	if err != nil {
		return err
	}
	if !v.atLeast(minTmuxMajor, minTmuxMinor) {
		return fmt.Errorf(
			"tmux %d.%d+ required (found %s); upgrade with "+
				"apt-get install tmux / brew upgrade tmux",
			minTmuxMajor, minTmuxMinor, v,
		)
	}
	return nil
}
