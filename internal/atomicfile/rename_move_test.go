//go:build darwin || linux

package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

// withoutExclusiveRename is ops on a filesystem, such as NFS or 9p, that
// refuses the exclusive rename with an errno it uses for that.
func withoutExclusiveRename(ops fileOps) fileOps {
	ops.exclusiveRename = func(string, string) error { return syscall.ENOSYS }
	return ops
}

// staged is a new file holding content, for a move to publish.
func staged(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExclusiveMoveLinksAFileWhereTheRenameIsUnsupported(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	from, to := staged(t, dir, "staged", "x"), filepath.Join(dir, "published")

	leftover, err := exclusiveMove(withoutExclusiveRename(systemOps()), from, to)
	if err != nil || leftover != "" {
		t.Fatalf("exclusiveMove = %q, %v; want published with nothing left", leftover, err)
	}
	if data, err := os.ReadFile(to); err != nil || string(data) != "x" {
		t.Fatalf("published = %q, %v", data, err)
	}
	if _, err := os.Lstat(from); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staged name is still there: %v", err)
	}
}

// Linked, but the staged name cannot be removed: the file is published, so
// there is no error, and the leftover name is the result.
func TestExclusiveMoveAnswersTheStagedNameItCouldNotRemove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	from, to := staged(t, dir, "staged", "x"), filepath.Join(dir, "published")
	ops := withoutExclusiveRename(systemOps())
	ops.remove = func(string) error { return errors.New("injected removal failure") }

	leftover, err := exclusiveMove(ops, from, to)
	if err != nil || leftover != from {
		t.Fatalf("exclusiveMove = %q, %v; want published with %s left", leftover, err, from)
	}
	if data, err := os.ReadFile(to); err != nil || string(data) != "x" {
		t.Fatalf("published = %q, %v", data, err)
	}
}

// Only a filesystem that refuses the link too is ErrNoExclusiveMove; any
// other link failure is returned as itself, so the user gets the action it
// supports. Nothing is published either way.
func TestExclusiveMoveTellsARefusedLinkFromAFailedOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		linkErr   error
		noMove    bool
		wantErrno error
	}{
		{"refused", syscall.EPERM, true, nil},
		{"not supported", syscall.EOPNOTSUPP, true, nil},
		{"no space", syscall.ENOSPC, false, syscall.ENOSPC},
		{"no permission", syscall.EACCES, false, syscall.EACCES},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			from, to := staged(t, dir, "staged", "x"), filepath.Join(dir, "published")
			ops := withoutExclusiveRename(systemOps())
			ops.link = func(from, to string) error { return &os.LinkError{Op: "link", Old: from, New: to, Err: tc.linkErr} }

			leftover, err := exclusiveMove(ops, from, to)
			if err == nil || leftover != "" || errors.Is(err, ErrNoExclusiveMove) != tc.noMove {
				t.Fatalf("exclusiveMove = %q, %v; want unpublished, ErrNoExclusiveMove %v", leftover, err, tc.noMove)
			}
			if tc.wantErrno != nil && !errors.Is(err, tc.wantErrno) {
				t.Fatalf("exclusiveMove = %v, want the link's %v", err, tc.wantErrno)
			}
			if _, err := os.Lstat(to); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("something was published: %v", err)
			}
			if _, err := os.Lstat(from); err != nil {
				t.Fatalf("the staged file went: %v", err)
			}
		})
	}
}

// A directory is never linked: without the exclusive rename it cannot be
// moved, and the error says so.
func TestExclusiveMoveNeverLinksADirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	from, to := filepath.Join(dir, "stage"), filepath.Join(dir, "published")
	if err := os.Mkdir(from, 0o700); err != nil {
		t.Fatal(err)
	}
	ops := withoutExclusiveRename(systemOps())
	ops.link = func(string, string) error {
		t.Error("a directory was linked")
		return nil
	}

	_, err := renameNoReplace(ops, from, to)
	if !errors.Is(err, ErrNoExclusiveMove) || !strings.Contains(err.Error(), "moving the directory "+from) {
		t.Fatalf("renameNoReplace = %v, want ErrNoExclusiveMove for the directory", err)
	}
	if info, err := os.Stat(from); err != nil || !info.IsDir() {
		t.Fatalf("the staged directory after a refused move: %v", err)
	}
}

func TestExclusiveMoveRefusesAnExistingDestinationThroughTheLink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	from, to := staged(t, dir, "staged", "ours"), staged(t, dir, "published", "theirs")

	leftover, err := exclusiveMove(withoutExclusiveRename(systemOps()), from, to)
	if !errors.Is(err, os.ErrExist) || errors.Is(err, ErrNoExclusiveMove) || leftover != "" {
		t.Fatalf("exclusiveMove = %q, %v; want refused as existing", leftover, err)
	}
	if data, _ := os.ReadFile(to); string(data) != "theirs" {
		t.Fatalf("destination = %q, want theirs", data)
	}
}

// Only the filesystem's refusal of the exclusive rename falls back to a
// link; any other failure is the answer.
func TestExclusiveMoveReturnsAnyOtherRenameFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	from, to := staged(t, dir, "staged", "x"), filepath.Join(dir, "published")
	failure := errors.New("injected rename failure")
	ops := systemOps()
	ops.exclusiveRename = func(string, string) error { return failure }
	ops.link = func(string, string) error {
		t.Error("linked after a rename failure that is not a refusal")
		return nil
	}

	if _, err := exclusiveMove(ops, from, to); !errors.Is(err, failure) {
		t.Fatalf("exclusiveMove = %v, want the rename's failure", err)
	}
}

func TestExclusiveMoveWithoutTheRenameHasExactlyOneFilePublisher(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	to := filepath.Join(root, "published")
	ops := withoutExclusiveRename(systemOps())
	start := make(chan struct{})
	var workers sync.WaitGroup
	var successes atomic.Int32
	for i := range 16 {
		from := staged(t, root, fmt.Sprint(i), fmt.Sprint(i))
		workers.Go(func() {
			<-start
			_, err := renameNoReplace(ops, from, to)
			switch {
			case err == nil:
				successes.Add(1)
			case !errors.Is(err, os.ErrExist) || errors.Is(err, ErrNoExclusiveMove):
				t.Errorf("publisher %s: %v, want published or refused as existing", from, err)
			}
		})
	}
	close(start)
	workers.Wait()
	if successes.Load() != 1 {
		t.Fatalf("publishers: %d", successes.Load())
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 16 {
		t.Fatalf("entries after publishing: %d %v, want the published file and 15 refused", len(entries), err)
	}
}

func TestWriteIfUnchangedCreatesAFileWithoutTheExclusiveRename(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	committed, err := writeIfUnchanged(withoutExclusiveRename(systemOps()), path, []byte("first"), nil)
	if err != nil || !committed {
		t.Fatalf("writeIfUnchanged = %v, %v, want a committed create", committed, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "first" {
		t.Fatalf("config = %q, %v", data, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, %v, want 0600", info.Mode().Perm(), err)
	}
	assertNoStagedFiles(t, filepath.Dir(path))
}

func TestWriteIfUnchangedWithoutTheExclusiveRenameRefusesAFileThatAppeared(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	ops := withoutExclusiveRename(systemOps())
	// The file appears after the early check, while the candidate is
	// staged, so the link is what refuses it.
	ops.stage = func(target string, data []byte) (string, error) {
		writeRaw(t, target, "another writer")
		return stage(target, data)
	}
	committed, err := writeIfUnchanged(ops, path, []byte("ours"), nil)
	if err != nil || committed {
		t.Fatalf("writeIfUnchanged = %v, %v, want refused", committed, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "another writer" {
		t.Fatalf("config = %q, want the other writer's", data)
	}
	assertNoStagedFiles(t, filepath.Dir(path))
}

// A create published by a link whose staged copy could not be removed: the
// write tries the removal once more, and only a copy that still remains is
// reported, with the write committed.
func TestWriteIfUnchangedRemovesOrReportsTheStagedCopyALinkLeft(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		failures int
		reported bool
	}{
		{"removed on the retry", 1, false},
		{"still there", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.toml")
			ops := withoutExclusiveRename(systemOps())
			failures := tc.failures
			ops.remove = func(name string) error {
				if failures > 0 {
					failures--
					return errors.New("injected removal failure")
				}
				return os.Remove(name)
			}

			committed, err := writeIfUnchanged(ops, path, []byte("first"), nil)
			if !committed {
				t.Fatalf("writeIfUnchanged = %v, %v, want committed", committed, err)
			}
			if data, _ := os.ReadFile(path); string(data) != "first" {
				t.Fatalf("config = %q", data)
			}
			if !tc.reported {
				if err != nil {
					t.Fatalf("writeIfUnchanged = %v, want no error once the copy went", err)
				}
				assertNoStagedFiles(t, filepath.Dir(path))
				return
			}
			if err == nil || !strings.Contains(err.Error(), path+" is written, but its staged copy "+path+".tmp-") {
				t.Fatalf("writeIfUnchanged = %v, want the staged copy reported", err)
			}
		})
	}
}
