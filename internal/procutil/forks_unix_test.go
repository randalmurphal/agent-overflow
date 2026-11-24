//go:build !windows

package procutil

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A source that is not a regular file is refused before process starts are
// held off: a FIFO would block the copy with them held.
func TestCopyExecutableRefusesASourceThatIsNotARegularFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(src, 0o600); err != nil {
		t.Fatal(err)
	}
	err := CopyExecutable(filepath.Join(dir, "tool"), src, 0o755, nil)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v, want a refusal of the FIFO", err)
	}
}

// A destination name as long as a name can be installs: the new file's
// name does not grow with it.
func TestInstallExecutableInstallsTheLongestName(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), strings.Repeat("n", 255))
	if err := InstallExecutable(path, []byte("long"), 0o755); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "long" {
		t.Fatalf("installed %q, %v", data, err)
	}
}
