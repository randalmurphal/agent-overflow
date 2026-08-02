package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/workflow/profile"
	workflowrunner "agent-overflow/internal/workflow/runner"
	"agent-overflow/internal/workspacepath"
)

const workflowSetupOutputTailBytes = 16 * 1024

// The two variables every `worktree_setup.run` command is given. A setup recipe
// has to be able to name both checkouts — "symlink .env back to the main
// checkout" is the canonical one — and it can name neither on its own: the
// worktree path is generated per item, and the project root is not the
// command's working directory. Without them the only expressible recipe is a
// `copy:` glob, which snapshots and then silently diverges.
//
// The contract is documented for authors in internal/workflow/profile/AGENTS.md,
// next to the `worktree_setup` authoring format.
const (
	workflowSetupProjectRootEnv  = "AO_PROJECT_ROOT"
	workflowSetupWorktreePathEnv = "AO_WORKTREE_PATH"
)

func runWorkflowWorktreeSetup(ctx context.Context, projectRoot, worktreeRoot string, setup profile.WorktreeSetup) error {
	if err := copyWorkflowSetupEntries(ctx, projectRoot, worktreeRoot, setup.Copy); err != nil {
		return err
	}
	timeout := time.Duration(0)
	var err error
	if setup.Timeout == "" {
		timeout, err = time.ParseDuration(profile.DefaultWorktreeSetupTimeout)
	} else {
		timeout, err = time.ParseDuration(string(setup.Timeout))
	}
	if err != nil || timeout <= 0 {
		return fmt.Errorf("invalid worktree setup timeout %q", setup.Timeout)
	}
	commandEnv, err := workflowSetupEnv(projectRoot, worktreeRoot)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, argv := range setup.Run {
		if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
			return fmt.Errorf("worktree setup command has no executable")
		}
		tail := newWorkflowTailBuffer(workflowSetupOutputTailBytes)
		command := exec.CommandContext(runCtx, argv[0], argv[1:]...)
		command.Dir = worktreeRoot
		command.Env = commandEnv
		command.Stdout = tail
		command.Stderr = tail
		configureWorkflowCommand(command)
		if commandErr := command.Run(); commandErr != nil {
			output := strings.TrimSpace(tail.String())
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("worktree setup command %s timed out after %s; output tail: %s", workflowrunner.FormatArgv(argv), timeout, output)
			}
			return fmt.Errorf("worktree setup command %s failed: %w; output tail: %s", workflowrunner.FormatArgv(argv), commandErr, output)
		}
	}
	return nil
}

// workflowSetupEnv renders the environment for every setup command: the app's
// own environment so PATH and the user's toolchain survive, plus the two
// checkout paths.
//
// Both are absolutised here rather than trusted from the caller, so the
// contract "these are absolute" holds for whatever a project row or worktree
// record happens to contain. filepath.Abs resolves against the same working
// directory exec resolves a relative command.Dir against, so the variables can
// never name a different tree than the command actually runs in.
//
// They are appended last on purpose: os/exec keeps the final occurrence of a
// duplicated key, so an AO_PROJECT_ROOT the app itself inherited (launching the
// app from inside an agent session is normal) cannot shadow the real one.
func workflowSetupEnv(projectRoot, worktreeRoot string) ([]string, error) {
	absoluteProjectRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve project root %q for worktree setup: %w", projectRoot, err)
	}
	absoluteWorktreeRoot, err := filepath.Abs(worktreeRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree path %q for worktree setup: %w", worktreeRoot, err)
	}
	return append(os.Environ(),
		workflowSetupProjectRootEnv+"="+absoluteProjectRoot,
		workflowSetupWorktreePathEnv+"="+absoluteWorktreeRoot,
	), nil
}

func copyWorkflowSetupEntries(ctx context.Context, projectRoot, worktreeRoot string, patterns []string) error {
	for _, pattern := range patterns {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("worktree setup copy cancelled: %w", err)
		}
		rel, err := normalizeWorkflowGlob(pattern)
		if err != nil {
			return fmt.Errorf("worktree setup copy %q: %w", pattern, err)
		}
		matches, err := filepath.Glob(filepath.Join(projectRoot, rel))
		if err != nil {
			return fmt.Errorf("worktree setup copy %q: %w", pattern, err)
		}
		if len(matches) == 0 {
			return fmt.Errorf("worktree setup copy %q matched no files", pattern)
		}
		sort.Strings(matches)
		copied := 0
		for _, source := range matches {
			matchRel, err := filepath.Rel(projectRoot, source)
			if err != nil {
				return fmt.Errorf("worktree setup copy %q: %w", pattern, err)
			}
			if workflowPathHasGitComponent(matchRel) {
				continue
			}
			if err := validateWorkflowSource(projectRoot, matchRel); err != nil {
				return fmt.Errorf("worktree setup copy %q: %w", pattern, err)
			}
			if err := copyWorkflowSetupPath(ctx, projectRoot, worktreeRoot, matchRel); err != nil {
				return fmt.Errorf("worktree setup copy %q: %w", pattern, err)
			}
			copied++
		}
		if copied == 0 {
			return fmt.Errorf("worktree setup copy %q matched no safe files", pattern)
		}
	}
	return nil
}

func workflowPathHasGitComponent(path string) bool {
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if part == ".git" {
			return true
		}
	}
	return false
}

func validateWorkflowSource(root, relative string) error {
	if _, err := workspacepath.NormalizeRelative(relative); err != nil {
		return err
	}
	current := root
	for _, part := range strings.Split(filepath.Clean(relative), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect source %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("source %q traverses a symbolic link", current)
		}
	}
	return nil
}

func normalizeWorkflowGlob(pattern string) (string, error) {
	trimmed := strings.TrimSpace(pattern)
	if trimmed == "" || filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("path must be project-root-relative")
	}
	clean := filepath.Clean(trimmed)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the project root")
	}
	if workflowPathHasGitComponent(clean) {
		return "", fmt.Errorf("copying .git metadata is not allowed")
	}
	return clean, nil
}

func copyWorkflowSetupPath(ctx context.Context, projectRoot, worktreeRoot, relative string) error {
	relative, err := workspacepath.NormalizeRelative(relative)
	if err != nil {
		return err
	}
	sourceRoot := filepath.Join(projectRoot, relative)
	return filepath.WalkDir(sourceRoot, func(source string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("worktree setup copy cancelled: %w", err)
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("source %q is a symbolic link", source)
		}
		suffix, err := filepath.Rel(projectRoot, source)
		if err != nil {
			return err
		}
		if workflowPathHasGitComponent(suffix) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		destination := filepath.Join(worktreeRoot, suffix)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if err := validateWorkflowDestination(worktreeRoot, destination); err != nil {
				return err
			}
			root, err := os.OpenRoot(worktreeRoot)
			if err != nil {
				return err
			}
			mkdirErr := root.MkdirAll(suffix, info.Mode().Perm())
			return errors.Join(mkdirErr, root.Close())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("source %q is not a regular file", source)
		}
		return copyWorkflowFile(projectRoot, suffix, worktreeRoot, suffix, info.Mode().Perm())
	})
}

func copyWorkflowFile(sourceRootPath, sourceRelative, destinationRootPath, destinationRelative string, mode fs.FileMode) (resultErr error) {
	destination := filepath.Join(destinationRootPath, destinationRelative)
	if err := validateWorkflowDestination(destinationRootPath, destination); err != nil {
		return err
	}
	sourceRoot, err := os.OpenRoot(sourceRootPath)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, sourceRoot.Close()) }()
	input, err := sourceRoot.Open(sourceRelative)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, input.Close()) }()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file")
	}
	destinationRoot, err := os.OpenRoot(destinationRootPath)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, destinationRoot.Close()) }()
	if err := destinationRoot.MkdirAll(filepath.Dir(destinationRelative), appPrivateDirPerm); err != nil {
		return err
	}
	tempRelative := ".ao-copy-" + uuid.NewString()
	temp, err := destinationRoot.OpenFile(tempRelative, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() { _ = destinationRoot.Remove(tempRelative) }()
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := io.Copy(temp, input); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return destinationRoot.Rename(tempRelative, destinationRelative)
}

func validateWorkflowDestination(root, destination string) error {
	relative, err := filepath.Rel(root, destination)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("destination escapes its managed root")
	}
	current := root
	for _, part := range strings.Split(filepath.Dir(relative), string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect destination parent %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("destination parent %q is a symbolic link", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("destination parent %q is not a directory", current)
		}
	}
	return nil
}

// workflowTailBuffer keeps the last `limit` bytes written to it. Command output
// is unbounded and its useful end is the tail, so a phase or setup hook never
// buffers a whole stream.
type workflowTailBuffer struct {
	mu    sync.Mutex
	limit int
	total int64
	data  []byte
}

func newWorkflowTailBuffer(limit int) *workflowTailBuffer {
	return &workflowTailBuffer{limit: limit, data: make([]byte, 0, limit)}
}

func (b *workflowTailBuffer) Write(payload []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(payload)
	b.total += int64(written)
	if b.limit <= 0 {
		return written, nil
	}
	if len(payload) >= b.limit {
		b.data = append(b.data[:0], payload[len(payload)-b.limit:]...)
		return written, nil
	}
	overflow := len(b.data) + len(payload) - b.limit
	if overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, payload...)
	return written, nil
}

func (b *workflowTailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// Truncated reports whether writes exceeded the retained tail.
func (b *workflowTailBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total > int64(len(b.data))
}
