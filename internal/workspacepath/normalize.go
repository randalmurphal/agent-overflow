package workspacepath

import (
	"fmt"
	"path/filepath"
	"strings"
)

// NormalizeRelative validates a user-supplied workspace-relative
// path and returns the OS-cleaned form. The result is guaranteed:
//
//   - non-empty
//   - not an absolute path
//   - not `.` or `..`
//   - does not begin with a parent-directory traversal segment
//
// Callers can safely `filepath.Join(workspaceRoot, normalized)` on a
// successful return without escaping the root. Errors are
// user-facing-friendly — they prefix `workspace path` so the calling
// binding can surface them verbatim.
func NormalizeRelative(relativePath string) (string, error) {
	trimmed := strings.TrimSpace(relativePath)
	if trimmed == "" {
		return "", fmt.Errorf("workspace path is required")
	}
	if filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("workspace path must be relative")
	}

	normalized := filepath.Clean(trimmed)
	parentPrefix := ".." + string(filepath.Separator)
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, parentPrefix) {
		return "", fmt.Errorf("workspace path must stay within the workspace root")
	}
	return normalized, nil
}
