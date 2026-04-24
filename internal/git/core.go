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

// Worktree describes a git worktree attached to a repository.
type Worktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
	HEAD   string `json:"head"`
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

// CreateWorktree creates a new worktree backed by a new branch.
func (c *Core) CreateWorktree(cwd, path, branch string) error {
	return c.CreateWorktreeFromBranch(cwd, path, "", branch)
}

// CreateWorktreeFromBranch creates a new worktree backed by newBranch from an
// explicit base branch. Empty baseBranch lets git use the current HEAD.
func (c *Core) CreateWorktreeFromBranch(cwd, path, baseBranch, newBranch string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("git worktree path is required")
	}
	newBranch = strings.TrimSpace(newBranch)
	baseBranch = strings.TrimSpace(baseBranch)
	if newBranch == "" {
		return errors.New("git worktree branch is required")
	}
	if err := validateBranchName(newBranch); err != nil {
		return err
	}
	if baseBranch != "" {
		if err := validateBranchName(baseBranch); err != nil {
			return err
		}
	}

	args := []string{"worktree", "add", "-b", newBranch, path}
	if baseBranch != "" {
		args = append(args, baseBranch)
	}
	_, stderr, err := c.Execute(cwd, args...)
	if err != nil {
		message := strings.TrimSpace(stderr)
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("git worktree add failed: %s", message)
	}
	return nil
}

// RemoveWorktree removes a worktree from a repository.
func (c *Core) RemoveWorktree(cwd, path string) error {
	return c.RemoveWorktreeForce(cwd, path, false)
}

// RemoveWorktreeForce removes a worktree from a repository, optionally using
// git's force flag for app-owned cleanup paths.
func (c *Core) RemoveWorktreeForce(cwd, path string, force bool) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("git worktree path is required")
	}

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	_, stderr, err := c.Execute(cwd, args...)
	if err != nil {
		message := strings.TrimSpace(stderr)
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("git worktree remove failed: %s", message)
	}
	return nil
}

// ListWorktrees returns all worktrees attached to the repository.
func (c *Core) ListWorktrees(cwd string) ([]Worktree, error) {
	result, err := c.run(cwd, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	if result.exitCode != 0 {
		return nil, fmt.Errorf("git worktree list failed: %s", strings.TrimSpace(result.stderr))
	}
	return parseWorktreeList(result.stdout), nil
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
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, binary)
	for _, arg := range args {
		if strings.ContainsAny(arg, " \t\"'\\") {
			parts = append(parts, fmt.Sprintf("%q", arg))
		} else {
			parts = append(parts, arg)
		}
	}
	return strings.Join(parts, " ")
}

func parseWorktreeList(stdout string) []Worktree {
	var worktrees []Worktree
	current := Worktree{}

	flush := func() {
		if strings.TrimSpace(current.Path) == "" {
			return
		}
		worktrees = append(worktrees, current)
		current = Worktree{}
	}

	for _, rawLine := range strings.Split(stdout, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			flush()
			continue
		}

		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			current.Path = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
		case strings.HasPrefix(line, "HEAD "):
			current.HEAD = strings.TrimSpace(strings.TrimPrefix(line, "HEAD "))
		case strings.HasPrefix(line, "branch "):
			current.Branch = trimBranchRef(strings.TrimSpace(strings.TrimPrefix(line, "branch ")))
		}
	}

	flush()
	return worktrees
}

func trimBranchRef(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
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
