package procutil

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
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

// The source is a pipe the test writes, so the install cannot close its
// file until the test closes the pipe: once the first write has been read,
// the file is open for writing, and a process start must not be able to
// begin. Not parallel: it takes the process-wide fork lock.
func TestInstallExecutableHoldsForksWhileTheFileIsOpenForWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	src, feed := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- InstallExecutable(path, src, 0o755) }()
	if _, err := feed.Write([]byte("#!/bin/sh\n")); err != nil { // returns once the install has read it
		t.Fatal(err)
	}
	if syscall.ForkLock.TryLock() {
		syscall.ForkLock.Unlock()
		_ = feed.CloseWithError(errors.New("test failed"))
		<-done
		t.Fatal("a process could start while the executable was open for writing")
	}
	if _, err := feed.Write([]byte("exit 0\n")); err != nil {
		t.Fatal(err)
	}
	if err := feed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !syscall.ForkLock.TryLock() {
		t.Fatal("process starts were still held off after the install")
	}
	syscall.ForkLock.Unlock()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "#!/bin/sh\nexit 0\n" {
		t.Fatalf("installed %q, %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("installed mode = %v, %v; want 0755", info, err)
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
			if err := InstallExecutable(path, strings.NewReader("own"), 0o755); err != nil {
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

// A failed install leaves nothing beside the path.
func TestInstallExecutableRemovesItsFileWhenTheSourceFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	broken := errors.New("source failed")
	err := InstallExecutable(filepath.Join(dir, "tool"), io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(broken)), 0o755)
	if !errors.Is(err, broken) {
		t.Fatalf("err = %v, want %v", err, broken)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("left %v, %v; want nothing", entries, err)
	}
}
