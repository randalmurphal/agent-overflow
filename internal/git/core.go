package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultTimeout        = 30 * time.Second
	defaultMaxOutputBytes = int64(1_000_000)
)

type commandResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// Core wraps git command execution with timeouts and bounded output capture.
type Core struct {
	timeout        time.Duration
	maxOutputBytes int64
}

// NewCore returns a Core configured with the default timeout and output limit.
func NewCore() *Core {
	return &Core{
		timeout:        defaultTimeout,
		maxOutputBytes: defaultMaxOutputBytes,
	}
}

// Execute runs git with the provided arguments. Non-zero exits are returned as errors.
func (c *Core) Execute(cwd string, args ...string) (stdout, stderr string, err error) {
	result, err := c.run(cwd, args...)
	if err != nil {
		return "", "", err
	}
	if result.exitCode != 0 {
		return result.stdout, result.stderr, fmt.Errorf(
			"%s exited with code %d",
			formatCommand("git", args...),
			result.exitCode,
		)
	}
	return result.stdout, result.stderr, nil
}

func (c *Core) run(cwd string, args ...string) (commandResult, error) {
	return c.runBinary("git", cwd, args...)
}

func (c *Core) runBinary(binary, cwd string, args ...string) (commandResult, error) {
	timeout := c.timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxBytes := c.maxOutputBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxOutputBytes
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}

	stdoutBuf := newLimitedBuffer(maxBytes)
	stderrBuf := newLimitedBuffer(maxBytes)
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	err := cmd.Run()
	result := commandResult{
		stdout: stdoutBuf.String(),
		stderr: stderrBuf.String(),
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return result, fmt.Errorf("%s timed out after %s", formatCommand(binary, args...), timeout)
	}
	if stdoutBuf.Truncated() || stderrBuf.Truncated() {
		return result, fmt.Errorf(
			"%s output exceeded %d bytes",
			formatCommand(binary, args...),
			maxBytes,
		)
	}
	if err == nil {
		return result, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.exitCode = exitErr.ExitCode()
		return result, nil
	}

	return result, fmt.Errorf("%s failed: %w", formatCommand(binary, args...), err)
}

func formatCommand(binary string, args ...string) string {
	parts := append([]string{binary}, args...)
	return strings.Join(parts, " ")
}

type limitedBuffer struct {
	buf       bytes.Buffer
	maxBytes  int64
	truncated bool
}

func newLimitedBuffer(maxBytes int64) *limitedBuffer {
	return &limitedBuffer{maxBytes: maxBytes}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.maxBytes <= 0 {
		return len(p), nil
	}

	remaining := b.maxBytes - int64(b.buf.Len())
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}

	if int64(len(p)) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}

	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	return b.buf.String()
}

func (b *limitedBuffer) Truncated() bool {
	return b.truncated
}
