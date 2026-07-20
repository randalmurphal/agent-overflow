package gitdiff

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

var maxDiffOutputBytes int64 = 10 * 1024 * 1024

var errGitOutputTooLarge = errors.New("git output exceeded limit")

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
	if runErr == nil {
		return stdout, stderr, 0, nil
	}
	if errors.Is(runErr, exec.ErrWaitDelay) {
		return stdout, stderr, 0, fmt.Errorf("git %s: output pipes did not close before wait delay: %w",
			strings.Join(args, " "), runErr)
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
	if runErr == nil {
		return stdout, stderr, 0, nil
	}
	if errors.Is(runErr, exec.ErrWaitDelay) {
		return stdout, stderr, 0, fmt.Errorf("git %s: output pipes did not close before wait delay: %w",
			strings.Join(args, " "), runErr)
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

func runGitWithStdoutLimit(
	ctx context.Context,
	workspace string,
	extraEnv []string,
	allowNonZero bool,
	maxStdoutBytes int64,
	args ...string,
) (stdout, stderr string, code int, err error) {
	if maxStdoutBytes <= 0 {
		return runGit(ctx, workspace, extraEnv, allowNonZero, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workspace
	cmd.Env = gitEnv(extraEnv)
	cmd.WaitDelay = gitPipeWaitDelay
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", 0, fmt.Errorf("git %s: stdout pipe: %w", strings.Join(args, " "), err)
	}
	stderrFile, err := os.CreateTemp("", "agent-overflow-git-stderr-*")
	if err != nil {
		return "", "", 0, fmt.Errorf("git %s: stderr temp file: %w", strings.Join(args, " "), err)
	}
	stderrPath := stderrFile.Name()
	defer os.Remove(stderrPath)
	defer stderrFile.Close()
	readStderr := func() string {
		_ = stderrFile.Close()
		data, err := os.ReadFile(stderrPath)
		if err != nil {
			return ""
		}
		return string(data)
	}
	cmd.Stderr = stderrFile
	if err := cmd.Start(); err != nil {
		return "", "", 0, fmt.Errorf("git %s: start: %w", strings.Join(args, " "), err)
	}

	data, readErr := io.ReadAll(io.LimitReader(stdoutPipe, maxStdoutBytes+1))
	if int64(len(data)) > maxStdoutBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", readStderr(), 0, errGitOutputTooLarge
	}
	waitErr := cmd.Wait()
	stdout = string(data)
	stderr = readStderr()
	if readErr != nil {
		return stdout, stderr, 0, fmt.Errorf("git %s: read stdout: %w", strings.Join(args, " "), readErr)
	}
	if waitErr == nil {
		return stdout, stderr, 0, nil
	}
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		return stdout, stderr, 0, fmt.Errorf("git %s: output pipes did not close before wait delay: %w",
			strings.Join(args, " "), waitErr)
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
		code = exitErr.ExitCode()
		if allowNonZero {
			return stdout, stderr, code, nil
		}
	}
	return stdout, stderr, code, fmt.Errorf("git %s: exit=%d: %s",
		strings.Join(args, " "), code, strings.TrimSpace(stderr))
}

// gitEnv strips diff-driver overrides so user config can't inject an
// external command into automatic diff runs, then appends extraEnv.
func gitEnv(extraEnv []string) []string {
	env := make([]string, 0, len(os.Environ())+len(extraEnv)+2)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "GIT_EXTERNAL_DIFF=") || strings.HasPrefix(entry, "GIT_DIFF_OPTS=") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "GIT_EXTERNAL_DIFF=", "GIT_DIFF_OPTS=")
	return append(env, extraEnv...)
}
