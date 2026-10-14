//go:build unix

package app

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// addUnixCases adds the unreadable files only Unix makes: a file of mode
// 000, and a FIFO the start must refuse without waiting for a writer.
func addUnixCases(cases map[string]homeBreak) {
	cases["no permission"] = homeBreak{func(t *testing.T, path string) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a file of mode 000")
		}
		writeHomeFile(t, filepath.Dir(path), filepath.Base(path), "secret\n")
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
	}, "permission denied"}
	cases["fifo"] = homeBreak{func(t *testing.T, path string) {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Skip("no FIFOs here:", err)
		}
	}, "not a regular file"}
}
