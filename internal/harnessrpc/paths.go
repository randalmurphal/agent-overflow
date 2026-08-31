package harnessrpc

import "path/filepath"

// SameCanonicalPath compares paths after symlink resolution and cleaning,
// falling back to lexical comparison when either path does not exist yet.
func SameCanonicalPath(a, b string) bool {
	canonicalA, errA := filepath.EvalSymlinks(a)
	canonicalB, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return canonicalA == canonicalB
}
