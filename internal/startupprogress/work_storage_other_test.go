//go:build !linux

package startupprogress

import "testing"

// storageDir is a directory whose writes the platform's I/O counter sees.
func storageDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
