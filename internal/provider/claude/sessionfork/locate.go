package sessionfork

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)


// ErrSessionFileNotFound is returned when the JSONL for sessionID can't
// be located on disk (neither in the workspace's project dir nor in any
// other project dir under ~/.claude/projects/).
var ErrSessionFileNotFound = errors.New("sessionfork: session file not found")

// LocateSessionFile resolves the on-disk path of a Claude session JSONL.
//
// Claude stores sessions at ~/.claude/projects/<slug>/<sessionID>.jsonl
// where slug is derived from the workspace's CANONICAL absolute path
// (symlinks resolved): replace each path separator with '-' and prepend
// a leading '-'. On macOS, /tmp resolves to /private/tmp so the slug is
// `-private-tmp-<...>` not `-tmp-<...>`.
//
// If the file isn't where we expect, fall back to scanning every project
// dir under ~/.claude/projects/ — sessions can migrate when a workspace
// is moved.
func LocateSessionFile(sessionID, workspacePath string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("sessionfork: empty sessionID")
	}
	// Defensive: reject sessionIDs that could break out of the
	// projects dir via path components. Production callers always pass
	// a UUID read from SQLite (server-set), but this helper is exposed
	// at package boundary so a future caller could pass user-influenced
	// input. Cheaper to validate once than audit every call site.
	if strings.ContainsAny(sessionID, "/\\") || strings.Contains(sessionID, "..") {
		return "", fmt.Errorf("sessionfork: sessionID contains path separator or traversal: %q", sessionID)
	}

	projectsDir, err := projectsDir()
	if err != nil {
		return "", err
	}

	// Primary lookup: compute the slug for the workspace's canonical path.
	if workspacePath != "" {
		canonical, err := filepath.EvalSymlinks(workspacePath)
		if err == nil {
			abs, err := filepath.Abs(canonical)
			if err == nil {
				slug := projectSlug(abs)
				candidate := filepath.Join(projectsDir, slug, sessionID+".jsonl")
				if fileExists(candidate) {
					return candidate, nil
				}
			}
		}
	}

	// Fallback: scan all project dirs.
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		// No projects dir at all = no session files. Treat as a normal
		// not-found rather than a hard error, so callers can fall back
		// gracefully (e.g. revert truncates the timeline even when
		// Claude state is gone from disk).
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrSessionFileNotFound, sessionID)
		}
		return "", fmt.Errorf("sessionfork: read projects dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(projectsDir, e.Name(), sessionID+".jsonl")
		if fileExists(candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("%w: %s", ErrSessionFileNotFound, sessionID)
}

// projectsDir returns ~/.claude/projects, resolving the user's home dir
// from $HOME (or os.UserHomeDir as fallback).
func projectsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("sessionfork: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// projectSlug encodes a canonical absolute path the way Claude does:
// replace separators with '-' and prepend '-'. Idempotent on the leading
// dash.
func projectSlug(absPath string) string {
	// Replace every os.PathSeparator with '-'. On unix that's '/'.
	slug := strings.ReplaceAll(absPath, string(os.PathSeparator), "-")
	if !strings.HasPrefix(slug, "-") {
		slug = "-" + slug
	}
	return slug
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Size() > 0
}
