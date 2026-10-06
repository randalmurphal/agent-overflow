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
	"agent-overflow/internal/procutil"
)

// gitPipeWaitDelay is how long a run waits on an empty output pipe that a
// process git started still holds after git exits.
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
	var out, errBuf strings.Builder
	runErr := procutil.RunDrained(ctx, cmd, &out, &errBuf, gitPipeWaitDelay)
	stdout = out.String()
	stderr = errBuf.String()
	code, err = gitResult(ctx, runErr, allowNonZero, args, stderr)
	return stdout, stderr, code, err
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
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errBuf strings.Builder
	runErr := procutil.RunDrained(ctx, cmd, &out, &errBuf, gitPipeWaitDelay)
	stdout = out.String()
	stderr = errBuf.String()
	code, err = gitResult(ctx, runErr, allowNonZero, args, stderr)
	return stdout, stderr, code, err
}

// gitResult classifies a finished run. allowNonZero excuses only git's own
// exit status: a run the context ended is an error whatever git exited
// with, because a killed probe is not a negative answer.
func gitResult(ctx context.Context, runErr error, allowNonZero bool, args []string, stderr string) (int, error) {
	if runErr == nil {
		return 0, nil
	}
	code := 0
	if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
		code = exitErr.ExitCode()
		if allowNonZero && ctx.Err() == nil {
			return code, nil
		}
	}
	return code, fmt.Errorf("git %s: exit=%d: %s: %w",
		strings.Join(args, " "), code, strings.TrimSpace(stderr), runErr)
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
