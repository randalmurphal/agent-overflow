package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// directoryEntryLimit caps the number of entries returned from a single
// BrowseDirectory call. Beyond this, the listing sets Truncated=true and
// the frontend surfaces a "refine path" hint. Pagination is deliberately
// out of scope for v1 — directories with more than 500 entries are rare,
// and scrolling through a linear list of them is bad UX anyway.
const directoryEntryLimit = 500

// DirectoryListing is the structured result of a BrowseDirectory call.
// The frontend uses Parent for back-navigation, Separator to render paths
// for display, and Truncated to surface a "refine path" hint when a dir
// is larger than directoryEntryLimit.
type DirectoryListing struct {
	Path      string           `json:"path"`      // cleaned absolute path of the listed directory
	Parent    string           `json:"parent"`    // parent dir, or "" when at the filesystem root
	Separator string           `json:"separator"` // "/" on unix, "\\" on windows
	Entries   []DirectoryEntry `json:"entries"`
	Truncated bool             `json:"truncated"` // true when entry count exceeded directoryEntryLimit
}

// DirectoryEntry is one row inside a DirectoryListing.
// IsRepo is only populated for directory entries; files always report
// IsRepo=false regardless of name.
type DirectoryEntry struct {
	Name   string `json:"name"`   // basename, no trailing separator
	IsDir  bool   `json:"isDir"`  // resolved via stat (symlinks reflect target type)
	Hidden bool   `json:"hidden"` // name begins with "."
	IsRepo bool   `json:"isRepo"` // directory contains ".git" (dir OR file); false for non-dirs
}

// BrowseDirectory lists the contents of path for the project-picker UI.
//
// Normalization rules (keep the modal forgiving about input shape):
//   - "" and "~" resolve to the user's home directory.
//   - "~/sub" resolves to $HOME/sub.
//   - relative paths resolve against the process CWD via filepath.Abs.
//   - the final path is cleaned with filepath.Clean before stat.
//
// Symlinks are reported via stat (target-following), NOT lstat — a
// symlink pointing at a directory shows up as IsDir=true so the
// frontend can descend into it like any other directory.
//
// Hidden entries (leading ".") are included and flagged so the frontend
// can style or filter them.
//
// Ordering is compound: directories first (alphabetical), then files
// (alphabetical). This matches the AddProject modal's keyboard-nav
// expectation that arrow-down moves through folders before files.
//
// Results cap at directoryEntryLimit entries; Truncated=true signals
// the cap was hit so the UI can prompt the user to narrow the path.
func (a *App) BrowseDirectory(path string) (DirectoryListing, error) {
	resolved, err := resolveBrowsePath(path)
	if err != nil {
		return DirectoryListing{}, err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return DirectoryListing{}, fmt.Errorf("browse directory: %s: %w", resolved, err)
	}
	if !info.IsDir() {
		return DirectoryListing{}, fmt.Errorf("browse directory: %s is not a directory", resolved)
	}

	rawEntries, err := os.ReadDir(resolved)
	if err != nil {
		return DirectoryListing{}, fmt.Errorf("browse directory: read %s: %w", resolved, err)
	}

	entries := make([]DirectoryEntry, 0, len(rawEntries))
	truncated := false
	for _, raw := range rawEntries {
		if len(entries) >= directoryEntryLimit {
			truncated = true
			break
		}
		entries = append(entries, buildDirectoryEntry(resolved, raw))
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir // true (dir) sorts before false (file)
		}
		return entries[i].Name < entries[j].Name
	})

	return DirectoryListing{
		Path:      resolved,
		Parent:    parentForPath(resolved),
		Separator: string(os.PathSeparator),
		Entries:   entries,
		Truncated: truncated,
	}, nil
}

// resolveBrowsePath normalises the caller-supplied path into a cleaned
// absolute directory path. It does NOT stat — the caller does that after
// so the error message carries the resolved path.
func resolveBrowsePath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)

	// Home-expansion handling. The three empty/"~" spellings all point
	// at the home dir; "~/..." expands the prefix.
	if trimmed == "" || trimmed == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("browse directory: resolve home: %w", err)
		}
		return filepath.Clean(home), nil
	}
	if strings.HasPrefix(trimmed, "~/") || strings.HasPrefix(trimmed, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("browse directory: resolve home: %w", err)
		}
		trimmed = filepath.Join(home, trimmed[2:])
	}

	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("browse directory: resolve absolute path %s: %w", trimmed, err)
	}
	return filepath.Clean(abs), nil
}

// buildDirectoryEntry converts a DirEntry into the wire shape.
//
// IsDir reflects the *target* of a symlink: a symlink pointing at a
// directory is reported as IsDir=true so the frontend can descend into
// it the same way it does a plain directory. os.DirEntry.Info() and
// os.DirEntry.IsDir() are both lstat-based — they describe the symlink
// itself, not its target — so we do an explicit os.Stat on the full
// path to resolve through the link. On a broken symlink Stat fails,
// and we fall back to the lstat answer (which will report IsDir=false)
// as a best-effort signal to the UI.
func buildDirectoryEntry(parentDir string, raw os.DirEntry) DirectoryEntry {
	name := raw.Name()
	entry := DirectoryEntry{
		Name:   name,
		Hidden: strings.HasPrefix(name, "."),
	}

	fullPath := filepath.Join(parentDir, name)
	if info, err := os.Stat(fullPath); err == nil {
		entry.IsDir = info.Mode().IsDir()
	} else {
		entry.IsDir = raw.IsDir()
	}

	if entry.IsDir {
		entry.IsRepo = hasGitMarker(fullPath)
	}
	return entry
}

// hasGitMarker reports whether dir contains a ".git" entry, which can be
// either a directory (normal repo) or a file (git worktree pointer).
// Any stat success counts as a match; any error (including NotExist)
// means false. Exactly one os.Stat call per directory — the per-entry
// budget stays tight.
func hasGitMarker(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// parentForPath returns the directory to navigate to when the user
// presses "back". At the filesystem root (unix "/") or a Windows
// drive root (e.g. "C:\") there is no parent, so the field is "".
func parentForPath(cleanedAbs string) string {
	if isFilesystemRoot(cleanedAbs) {
		return ""
	}
	return filepath.Dir(cleanedAbs)
}

// isFilesystemRoot reports whether cleanedAbs is a filesystem root for
// the current platform. On unix that's exactly "/"; on Windows it's a
// drive letter root like "C:\" (filepath.VolumeName + separator with
// no tail). filepath.Dir returns the input unchanged when called on a
// root, so checking `filepath.Dir(p) == p` is the portable signal.
func isFilesystemRoot(cleanedAbs string) bool {
	return filepath.Dir(cleanedAbs) == cleanedAbs
}
