//go:build darwin || linux

package atomicfile

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// withoutExclusiveRename stands in for a filesystem, such as NFS or 9p,
// that refuses the platform's exclusive rename with the errno it answers.
func withoutExclusiveRename(t *testing.T) {
	t.Helper()
	exclusiveRename = func(string, string) error { return exclusiveRenameUnsupportedErrno }
	t.Cleanup(func() { exclusiveRename = platformExclusiveRename })
}

func TestWriteIfUnchangedCreatesAFileWithoutTheExclusiveRename(t *testing.T) {
	withoutExclusiveRename(t)
	path := filepath.Join(t.TempDir(), "config.toml")

	committed, err := WriteIfUnchanged(path, []byte("first"), nil)
	if err != nil || !committed {
		t.Fatalf("WriteIfUnchanged = %v, %v, want a committed create", committed, err)
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
	withoutExclusiveRename(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	writeRaw(t, path, "another writer")
	expectRefused(t, path, "another writer", nil)
}

func TestRenameNoReplaceWithoutTheExclusiveRenameHasExactlyOneFilePublisher(t *testing.T) {
	withoutExclusiveRename(t)
	root := t.TempDir()
	to := filepath.Join(root, "published")
	start := make(chan struct{})
	var workers sync.WaitGroup
	var successes atomic.Int32
	for i := range 16 {
		from := filepath.Join(root, fmt.Sprint(i))
		if err := os.WriteFile(from, []byte(fmt.Sprint(i)), 0o600); err != nil {
			t.Fatal(err)
		}
		workers.Go(func() {
			<-start
			err := RenameNoReplace(from, to)
			switch {
			case err == nil:
				successes.Add(1)
			case !errors.Is(err, os.ErrExist) || strings.Contains(err.Error(), "fallback"):
				t.Errorf("publisher %s: %v, want published or refused as existing", from, err)
			}
		})
	}
	close(start)
	workers.Wait()
	if successes.Load() != 1 {
		t.Fatalf("publishers: %d", successes.Load())
	}
	// The winner's staged name is gone; every loser keeps its file.
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 16 {
		t.Fatalf("entries after publishing: %d %v, want the published file and 15 refused", len(entries), err)
	}
}

func TestRenameNoReplaceWithoutTheExclusiveRenameSaysADirectoryCannotBePublished(t *testing.T) {
	withoutExclusiveRename(t)
	root := t.TempDir()
	from, to := filepath.Join(root, "stage"), filepath.Join(root, "published")
	if err := os.Mkdir(from, 0o700); err != nil {
		t.Fatal(err)
	}

	err := RenameNoReplace(from, to)
	if err == nil || !strings.Contains(err.Error(), "the filesystem refuses a rename that cannot replace") ||
		!strings.Contains(err.Error(), "a hard link, the fallback for a file, failed") {
		t.Fatalf("RenameNoReplace = %v, want both primitives named", err)
	}
	// A caller tells the two refusals apart by errors.Is, never by text:
	// the browser's downloads report "supports neither" from the rename's
	// errno.
	err = ExclusiveRename(from, to)
	var linkErr *os.LinkError
	if !errors.Is(err, exclusiveRenameUnsupportedErrno) || !errors.As(err, &linkErr) || linkErr.Op != "link" {
		t.Fatalf("ExclusiveRename = %#v, want it to wrap the rename's errno and the link's error", err)
	}
	if _, err := os.Lstat(to); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination after a refused publish: %v", err)
	}
	if info, err := os.Stat(from); err != nil || !info.IsDir() {
		t.Fatalf("staged directory after a refused publish: %v", err)
	}
}

// Only the filesystem's refusal of the exclusive rename falls back to a
// link; any other failure is the answer.
func TestRenameNoReplaceReturnsAnyOtherRenameFailure(t *testing.T) {
	failure := errors.New("injected rename failure")
	exclusiveRename = func(string, string) error { return failure }
	t.Cleanup(func() { exclusiveRename = platformExclusiveRename })
	root := t.TempDir()
	from, to := filepath.Join(root, "staged"), filepath.Join(root, "published")
	if err := os.WriteFile(from, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RenameNoReplace(from, to); !errors.Is(err, failure) {
		t.Fatalf("RenameNoReplace = %v, want the rename's failure", err)
	}
	if _, err := os.Lstat(to); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination after a failed rename: %v", err)
	}
}

// A published file whose staged name cannot be removed is published: the
// call succeeds and the leftover name is logged.
func TestRenameNoReplaceLogsAStagedNameItCouldNotRemove(t *testing.T) {
	withoutExclusiveRename(t)
	removeStagedName = func(string) error { return errors.New("injected removal failure") }
	t.Cleanup(func() { removeStagedName = os.Remove })
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	root := t.TempDir()
	from, to := filepath.Join(root, "staged"), filepath.Join(root, "published")
	if err := os.WriteFile(from, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RenameNoReplace(from, to); err != nil {
		t.Fatalf("RenameNoReplace = %v, want published", err)
	}
	if data, err := os.ReadFile(to); err != nil || string(data) != "x" {
		t.Fatalf("published = %q, %v", data, err)
	}
	if !strings.Contains(logged.String(), "staged name "+from+" remains: injected removal failure") {
		t.Fatalf("logged %q, want the leftover staged name", logged.String())
	}
}
