package worktreesetup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agent-overflow/internal/safecopy"
	"agent-overflow/internal/workspacepath"
)

// copyEntries resolves each glob against the project root and copies every
// match into the worktree. A glob that matches nothing is an error, not a
// no-op: the recipe named a file it expected to exist, and a worktree missing
// it is broken in a way that only surfaces later.
func copyEntries(ctx context.Context, projectRoot, worktreeRoot string, patterns []string) error {
	for _, pattern := range patterns {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("worktree setup copy cancelled: %w", err)
		}
		rel, err := normalizeGlob(pattern)
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
			if pathHasGitComponent(matchRel) {
				continue
			}
			if err := validateSource(projectRoot, matchRel); err != nil {
				return fmt.Errorf("worktree setup copy %q: %w", pattern, err)
			}
			if err := copyPath(ctx, projectRoot, worktreeRoot, matchRel); err != nil {
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

func pathHasGitComponent(path string) bool {
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if part == ".git" {
			return true
		}
	}
	return false
}

// validateSource refuses a source reached through a symbolic link at ANY level.
// A link inside the project root can point anywhere on the host, so following
// one would let a recipe copy `/etc/shadow` into a worktree by naming a link
// the repository happens to contain.
func validateSource(root, relative string) error {
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

func normalizeGlob(pattern string) (string, error) {
	trimmed := strings.TrimSpace(pattern)
	if trimmed == "" || filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("path must be project-root-relative")
	}
	clean := filepath.Clean(trimmed)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the project root")
	}
	if pathHasGitComponent(clean) {
		return "", fmt.Errorf("copying .git metadata is not allowed")
	}
	return clean, nil
}

func copyPath(ctx context.Context, projectRoot, worktreeRoot, relative string) error {
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
		// A wildcard that matched the repository's own `.git` must not
		// overwrite the worktree's — that file is what makes it a worktree.
		if pathHasGitComponent(suffix) {
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
			if err := safecopy.ValidateDestination(worktreeRoot, destination); err != nil {
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
		return safecopy.File(projectRoot, suffix, worktreeRoot, suffix, info.Mode().Perm())
	})
}
