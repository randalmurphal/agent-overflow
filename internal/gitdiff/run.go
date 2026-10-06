package gitdiff

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"agent-overflow/internal/appimage"
)

var gitPipeWaitDelay = time.Second

// runGit runs `git <args>` with the given extra env vars. allowNonZero lets
// the caller handle exit codes without this helper treating them as errors —
// useful for probes (`rev-parse --verify`) and for `diff --no-index` which
// exits 1 when files differ.
func runGit(
	ctx context.Context,
	workspace string,
	extraEnv []string,
	allowNonZero bool,
	args ...string,
) (stdout, stderr string, code int, err error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workspace
	cmd.Env = gitEnv(extraEnv)
	cmd.WaitDelay = gitPipeWaitDelay
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	stdout = out.String()
	stderr = errBuf.String()
	// ErrWaitDelay means git exited successfully while a child it started
	// still held a pipe; git's own output is complete.
	if runErr == nil || errors.Is(runErr, exec.ErrWaitDelay) {
		return stdout, stderr, 0, nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
		code = exitErr.ExitCode()
		if allowNonZero {
			return stdout, stderr, code, nil
		}
	}
	return stdout, stderr, code, fmt.Errorf("git %s: exit=%d: %s",
		strings.Join(args, " "), code, strings.TrimSpace(stderr))
}

func runGitWithStdin(
	ctx context.Context,
	workspace string,
	extraEnv []string,
	stdin []byte,
	allowNonZero bool,
	args ...string,
) (stdout, stderr string, code int, err error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workspace
	cmd.Env = gitEnv(extraEnv)
	cmd.WaitDelay = gitPipeWaitDelay
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	stdout = out.String()
	stderr = errBuf.String()
	// ErrWaitDelay means git exited successfully while a child it started
	// still held a pipe; git's own output is complete.
	if runErr == nil || errors.Is(runErr, exec.ErrWaitDelay) {
		return stdout, stderr, 0, nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
		code = exitErr.ExitCode()
		if allowNonZero {
			return stdout, stderr, code, nil
		}
	}
	return stdout, stderr, code, fmt.Errorf("git %s: exit=%d: %s",
		strings.Join(args, " "), code, strings.TrimSpace(stderr))
}

// gitEnv strips diff-driver overrides so user config can't inject an
// external command into automatic diff runs, then appends extraEnv. The
// inherited base is scrubbed of AppImage launch artifacts like every
// other child environment (see internal/appimage).
func gitEnv(extraEnv []string) []string {
	base := appimage.Scrub(os.Environ())
	env := make([]string, 0, len(base)+len(extraEnv)+2)
	for _, entry := range base {
		if strings.HasPrefix(entry, "GIT_EXTERNAL_DIFF=") || strings.HasPrefix(entry, "GIT_DIFF_OPTS=") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "GIT_EXTERNAL_DIFF=", "GIT_DIFF_OPTS=")
	return append(env, extraEnv...)
}
