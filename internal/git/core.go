package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	defaultTimeout        = 30 * time.Second
	defaultMaxOutputBytes = int64(1_000_000)

	// prLookupTTL is how long an open-PR lookup result stays cached.
	// PR state changes slowly (the user creates / closes / merges via
	// explicit actions), and our hot path (gitwatch refresh on every
	// fs-event-debounce) would otherwise shell `gh pr list` 4×/sec
	// during continuous file activity. A 30s ceiling caps that at ≤1
	// network round-trip per branch per 30s; explicit invalidation
	// after CreatePR keeps the freshly-opened PR visible immediately.
	prLookupTTL = 30 * time.Second

	// FetchStaleWindow is how long after a successful `git fetch` the
	// app considers remote-tracking refs fresh enough to skip another
	// background fetch. The branch picker calls MaybeFetchRemotes on
	// open; staleness shorter than this returns quickly without
	// spawning git.
	FetchStaleWindow = 5 * time.Minute
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

	// prCache memoizes lookupOpenPR results so a refresh storm (gitwatch
	// firing every fs-event-debounce) doesn't translate into a `gh pr
	// list` storm. Keyed on (cwd, branch); cleared per-cwd on
	// CreatePR. A process-global TTL'd cache is the documented
	// carve-out in internal/CLAUDE.md anti-patterns.
	//
	// RWMutex: the hot path (gitwatch refresh → lookupOpenPR) hits the
	// cache as a read most of the time; only cache misses + explicit
	// invalidation take the writer lock.
	prCacheMu sync.RWMutex
	prCache   map[string]prCacheEntry

	// forgeCache memoizes origin-URL classification per cwd. TTL'd via
	// forgeDetectionTTL (5 min); written by Core.Status each refresh
	// and Core.DetectForge on cache miss so both paths share warmup.
	// RWMutex for the same hot-read reason as prCacheMu.
	forgeCacheMu sync.RWMutex
	forgeCache   map[string]forgeCacheEntry

	// forges is the registered set of forge implementations indexed by
	// id ("github", "gitlab"). Populated in NewCore; not mutated after
	// construction so reads are lock-free.
	forges map[string]Forge

	// fetchCache records the last time MaybeFetchRemotes successfully
	// ran `git fetch` against a given canonical repo root. Keyed by the
	// repository top-level path so worktrees of the same repo share
	// freshness state (the refs are shared on disk). RWMutex matches
	// the other caches above.
	fetchCacheMu sync.RWMutex
	fetchCache   map[string]time.Time

	// nowFn returns the current time. Production uses time.Now; tests
	// override to drive TTL expiry deterministically.
	nowFn func() time.Time
}

type prCacheEntry struct {
	url       string
	number    int
	expiresAt time.Time
}

func prCacheKey(cwd, branch string) string {
	return cwd + "\x00" + branch
}

// NewCore returns a Core configured with the default timeout and output limit.
func NewCore() *Core {
	core := &Core{
		timeout:        defaultTimeout,
		maxOutputBytes: defaultMaxOutputBytes,
		prCache:        make(map[string]prCacheEntry),
		forgeCache:     make(map[string]forgeCacheEntry),
		fetchCache:     make(map[string]time.Time),
		nowFn:          time.Now,
	}
	core.forges = map[string]Forge{
		"github": &githubForge{core: core},
		"gitlab": &gitlabForge{core: core},
	}
	return core
}

// forgeFor returns the Forge for cwd's detected origin remote. Returns
// nullForge when the origin is missing or the host is not a supported
// forge — every nullForge operation returns ErrUnsupportedForge so
// the caller can surface a single "not supported" message.
func (c *Core) forgeFor(cwd string) Forge {
	id := c.DetectForge(cwd)
	if id == "" {
		return nullForge{}
	}
	if f, ok := c.forges[id]; ok {
		return f
	}
	return nullForge{}
}

// ForgeByID returns the registered Forge with the given id, or
// nullForge if no such forge is registered. Used by callers that
// already know the forge id (e.g., a frontend-supplied "github" /
// "gitlab" string from a parsed PR/MR URL) so they can skip detection.
func (c *Core) ForgeByID(id string) Forge {
	if f, ok := c.forges[id]; ok {
		return f
	}
	return nullForge{}
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

// AttachWorktree creates a new worktree at path pointing at an existing
// branch. Fails if the branch already has a worktree (git's own
// invariant — one branch checked out in one place at a time).
func (c *Core) AttachWorktree(cwd, path, branch string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("git worktree path is required")
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return errors.New("git worktree branch is required")
	}
	if err := validateBranchName(branch); err != nil {
		return err
	}
	_, stderr, err := c.Execute(cwd, "worktree", "add", "--", path, branch)
	if err != nil {
		message := strings.TrimSpace(stderr)
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("git worktree add failed: %s", message)
	}
	return nil
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

	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
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
