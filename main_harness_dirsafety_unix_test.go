//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// `make harness` derives its data root from the checkout, which puts a
// PREDICTABLE path under /tmp. A stranger who creates it first, mode
// 0777, hands the boot a $HOME on a tree they still control — and a
// writable $HOME is a writable .gitconfig, which is their command running
// as us the next time the harness shells out to git. refuseSymlink
// catches the link they might plant; this catches the directory.
//
// Only the mode half is simulable in a test: the process cannot create a
// directory owned by somebody else.
func TestPrepareHarnessRefusesAWorldWritableDataRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "planted")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Set the mode after creation: MkdirAll would have had it filtered by
	// the umask, which is the very thing that makes this not happen by
	// accident.
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o777 {
		t.Skipf("this filesystem would not hold mode 0777 (%v, %v)", err, info)
	}

	_, err := prepareHarness(cliFlags{dataDir: root})
	if err == nil || !strings.Contains(err.Error(), "world-writable") {
		t.Fatalf("prepareHarness on a 0777 data root: err = %v, want a writability refusal", err)
	}

	// The same root at 0700 is fine — the refusal is about the mode, not
	// about the directory having existed.
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod back: %v", err)
	}
	if err := refuseUnsafeHarnessDir(root); err != nil {
		t.Fatalf("a 0700 directory we own was refused: %v", err)
	}
}

// A data root the harness creates itself is 0700 regardless of the
// process umask: MkdirAll only ever subtracts, so an operator running
// under `umask 000` would otherwise get a world-writable $HOME the next
// boot then refuses.
func TestPrepareHarnessCreatesA0700DataRootUnderAPermissiveUmask(t *testing.T) {
	prev := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(prev) })

	root := filepath.Join(t.TempDir(), "fresh")
	if err := ensureHarnessPrivateDir(root); err != nil {
		t.Fatalf("ensureHarnessPrivateDir: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("created data root is mode %04o, want 0700", perm)
	}
	if err := refuseUnsafeHarnessDir(root); err != nil {
		t.Fatalf("a directory the harness just created was refused: %v", err)
	}
}
