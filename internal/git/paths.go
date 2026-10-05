package git

import "path/filepath"

// CanonicalPath resolves symlinks and cleans the path, for comparing
// filesystem paths that may go through symlinks (macOS /var and /tmp, a
// symlinked home or config dir). A path that does not exist resolves through
// its longest existing ancestor with the missing rest appended, so a
// directory that was just deleted still compares equal to every spelling it
// had while it existed.
func CanonicalPath(path string) string {
	path = filepath.Clean(path)
	rest := ""
	for current := path; ; {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		rest = filepath.Join(filepath.Base(current), rest)
		current = parent
	}
}

// SameFilesystemPath reports whether two paths refer to the same location after
// symlink resolution and cleaning.
func SameFilesystemPath(left, right string) bool {
	return CanonicalPath(left) == CanonicalPath(right)
}
