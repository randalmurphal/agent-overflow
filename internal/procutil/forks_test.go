package procutil

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Not parallel: it takes the process-wide fork lock.
func TestHoldForksBlocksAForkUntilReleased(t *testing.T) {
	release := HoldForks()
	if syscall.ForkLock.TryLock() {
		syscall.ForkLock.Unlock()
		release()
		t.Fatal("a fork could start while forks were held off")
	}
	second := HoldForks()
	second()
	release()
	if !syscall.ForkLock.TryLock() {
		t.Fatal("a fork still could not start after the release")
	}
	syscall.ForkLock.Unlock()
}

// The fill runs while the new file is open for writing, inside the hold: a
// process start must not be able to begin then, and can once the install
// returns. A failure of the install ends the test with its error. Not
// parallel: it takes the process-wide fork lock.
func TestInstallHoldsForksWhileTheFileIsOpenForWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	err := install(path, 0o755, func(file *os.File) error {
		if syscall.ForkLock.TryLock() {
			syscall.ForkLock.Unlock()
			return errors.New("a process could start while the executable was open for writing")
		}
		_, err := file.WriteString("#!/bin/sh\nexit 0\n")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !syscall.ForkLock.TryLock() {
		t.Fatal("process starts were still held off after the install")
	}
	syscall.ForkLock.Unlock()
}

func TestInstallExecutableWritesTheBytesWithTheMode(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tool")
	if err := InstallExecutable(path, []byte("#!/bin/sh\nexit 0\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	assertInstalled(t, path, "#!/bin/sh\nexit 0\n", 0o750)
}

func TestCopyExecutableCopiesTheFileAndFeedsTheDigest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	if err := os.WriteFile(src, []byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "tool")
	digest := sha256.New()
	if err := CopyExecutable(path, src, 0o755, digest); err != nil {
		t.Fatal(err)
	}
	assertInstalled(t, path, "binary", 0o755)
	if want := sha256.Sum256([]byte("binary")); !bytes.Equal(digest.Sum(nil), want[:]) {
		t.Fatalf("digest = %x, want %x", digest.Sum(nil), want)
	}
	if err := CopyExecutable(filepath.Join(dir, "other"), src, 0o755, nil); err != nil {
		t.Fatal(err)
	}
	assertInstalled(t, filepath.Join(dir, "other"), "binary", 0o755)
}

func assertInstalled(t *testing.T, path, want string, perm os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("installed %q, %v; want %q", data, err, want)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != perm {
		t.Fatalf("installed mode = %v, %v; want %v", info, err, perm)
	}
}

// A hard link or a symlink at the path is replaced, and the file it named
// keeps its bytes.
func TestInstallExecutableReplacesALinkWithoutWritingThroughIt(t *testing.T) {
	t.Parallel()
	for _, link := range []struct {
		name string
		make func(target, path string) error
	}{
		{"hard link", os.Link},
		{"symlink", os.Symlink},
	} {
		t.Run(link.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			shared := filepath.Join(dir, "shared")
			if err := os.WriteFile(shared, []byte("shared"), 0o644); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "tool")
			if err := link.make(shared, path); err != nil {
				t.Fatal(err)
			}
			if err := InstallExecutable(path, []byte("own"), 0o755); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(shared); err != nil || string(data) != "shared" {
				t.Fatalf("the linked file holds %q, %v; want it unchanged", data, err)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "own" {
				t.Fatalf("the path holds %q, %v", data, err)
			}
			if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("the path is %v, %v; want a regular file", info, err)
			}
		})
	}
}

// A failed fill leaves nothing beside the path.
func TestInstallRemovesItsFileWhenTheFillFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	broken := errors.New("fill failed")
	err := install(filepath.Join(dir, "tool"), 0o755, func(file *os.File) error {
		if _, err := file.WriteString("partial"); err != nil {
			return err
		}
		return broken
	})
	if !errors.Is(err, broken) {
		t.Fatalf("err = %v, want %v", err, broken)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("left %v, %v; want nothing", entries, err)
	}
}
