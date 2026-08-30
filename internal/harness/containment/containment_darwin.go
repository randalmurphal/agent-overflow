//go:build darwin

package containment

import (
	"errors"
	"os/exec"
)

type darwinGroup struct {
	configured bool
}

// macOS has no supported cgroup/job-object equivalent, and current macOS
// kernels reject attempts to lower RLIMIT_DATA, RLIMIT_RSS, and RLIMIT_AS.
// The governor's exact-tree ceiling and host-floor watchdog are therefore the
// enforceable boundary on this platform. Configure still validates and owns
// the launch transition so callers cannot accidentally skip platform policy.
func Prepare(limit uint64) (Group, error) {
	if limit == 0 {
		return nil, errors.New("harness containment: memory limit must be positive")
	}
	if limit/1024 == 0 {
		return nil, errors.New("harness containment: memory limit is below one KiB")
	}
	return &darwinGroup{}, nil
}

func PrepareWithFallback(limit uint64) (Group, string, error) {
	group, err := Prepare(limit)
	if err != nil {
		return nil, "", err
	}
	return group, "watchdog-only-darwin", nil
}

func (g *darwinGroup) Configure(cmd *exec.Cmd) error {
	if cmd == nil {
		return errors.New("harness containment: nil command")
	}
	if g == nil {
		return errors.New("harness containment: nil group")
	}
	if g.configured {
		return errors.New("harness containment: command already configured")
	}
	if cmd.Path == "" || len(cmd.Args) == 0 {
		return errors.New("harness containment: command has no executable")
	}
	g.configured = true
	return nil
}

func (g *darwinGroup) Adopt(*exec.Cmd) error { return nil }
func (g *darwinGroup) Close() error          { return nil }
