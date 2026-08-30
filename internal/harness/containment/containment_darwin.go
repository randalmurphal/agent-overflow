//go:build darwin

package containment

import (
	"errors"
	"os/exec"
	"strconv"
)

type darwinGroup struct {
	limit      uint64
	configured bool
}

// macOS has no cgroup/job-object equivalent. RLIMIT_DATA is inherited by the
// backend and every descendant without blocking their normal runtime address
// space reservations. It is per-process rather than aggregate, so the
// governor's host-floor watchdog remains mandatory for total pressure.
// The shell execs the backend in the same pid, preserving bootstrap identity.
func Prepare(limit uint64) (Group, error) {
	if limit == 0 {
		return nil, errors.New("harness containment: memory limit must be positive")
	}
	if limit/1024 == 0 {
		return nil, errors.New("harness containment: memory limit is below one KiB")
	}
	return &darwinGroup{limit: limit}, nil
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
	args := append([]string(nil), cmd.Args...)
	// `ulimit -d` is implemented by the shell using setrlimit(RLIMIT_DATA).
	// exec preserves the original backend path, argv, environment, stdio, and
	// pid. A shell failure exits before the backend can print a bootstrap line.
	kib := strconv.FormatUint(g.limit/1024, 10)
	script := `limit="$1"; shift; ulimit -d "$limit" || { printf '%s\n' 'harness containment: setrlimit(RLIMIT_DATA) failed' >&2; exit 125; }; exec "$@"`
	prefix := []string{"sh", "-c", script, "agent-overflow-memory-limit", kib}
	cmd.Path = "/bin/sh"
	cmd.Args = append(prefix, args...)
	g.configured = true
	return nil
}

func (g *darwinGroup) Adopt(*exec.Cmd) error { return nil }
func (g *darwinGroup) Close() error          { return nil }
