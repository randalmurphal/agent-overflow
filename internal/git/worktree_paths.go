package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SanitizeWorktreePathSegment turns a branch name into a filesystem-safe
// directory segment for use under the worktrees base dir. Slashes,
// backslashes, and whitespace are replaced with `-`; leading/trailing
// dots and dashes are stripped. Returns "worktree" if nothing remains
// (e.g. branch is empty or only meta-chars).
func SanitizeWorktreePathSegment(branch string) string {
	replacer := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		" ", "-",
		"\t", "-",
	)
	sanitized := strings.Trim(replacer.Replace(strings.TrimSpace(branch)), ".-")
	if sanitized == "" {
		return "worktree"
	}
	return sanitized
}

// DefaultWorktreesBaseDir returns the conventional "<repo>-worktrees"
// sibling of projectPath. Callers may override this with a per-config
// directory; the helper is the default when no override applies.
func DefaultWorktreesBaseDir(projectPath string) string {
	repoName := filepath.Base(projectPath)
	return filepath.Join(
		filepath.Dir(projectPath),
		repoName+"-worktrees",
	)
}

// UniqueWorktreePath returns path if no entry exists there, otherwise
// path-1 / path-2 / ... up to path-99; if even those collide it falls
// back to path-<unix-millis> so the caller never blocks on a name
// clash. A stat error other than NotExist is propagated so the caller
// can decide whether to surface it as a toast or a hard failure.
func UniqueWorktreePath(path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return path, nil
		}
		return "", fmt.Errorf("check worktree path %s: %w", path, err)
	}
	for suffix := 1; suffix < 100; suffix++ {
		candidate := fmt.Sprintf("%s-%d", path, suffix)
		if _, err := os.Stat(candidate); err != nil {
			if os.IsNotExist(err) {
				return candidate, nil
			}
			return "", fmt.Errorf("check worktree path %s: %w", candidate, err)
		}
	}
	return fmt.Sprintf("%s-%d", path, time.Now().UnixMilli()), nil
}
