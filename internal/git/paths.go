package git

import "path/filepath"

// CanonicalPath resolves symlinks and cleans the path, suitable for comparing
// filesystem paths that may go through /tmp symlinks on macOS.
func CanonicalPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

// SameFilesystemPath reports whether two paths refer to the same location after
// symlink resolution and cleaning.
func SameFilesystemPath(left, right string) bool {
	return CanonicalPath(left) == CanonicalPath(right)
}
