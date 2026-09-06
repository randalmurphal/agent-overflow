package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRenameNoReplaceKeepsExistingEmptyDirectoryAndSymlink(t *testing.T) {
	for _, kind := range []string{"empty directory", "file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			from, to := filepath.Join(root, "from"), filepath.Join(root, "to")
			if err := os.Mkdir(from, 0o700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "empty directory":
				if err := os.Mkdir(to, 0o700); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(to, []byte("existing owner"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("from", to); err != nil {
					t.Skip(err)
				}
			}
			before, err := os.Lstat(to)
			if err != nil {
				t.Fatal(err)
			}
			if err := RenameNoReplace(from, to); err == nil {
				t.Fatal("overwrote another destination")
			}
			after, err := os.Lstat(to)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("changed destination: %v", err)
			}
			if _, err := os.Stat(from); err != nil {
				t.Fatal("removed source after conflict")
			}
		})
	}
}

func TestRenameNoReplaceHasExactlyOneConcurrentPublisher(t *testing.T) {
	root := t.TempDir()
	to := filepath.Join(root, "published")
	start := make(chan struct{})
	var workers sync.WaitGroup
	var successes atomic.Int32
	for i := range 16 {
		from := filepath.Join(root, fmt.Sprint(i))
		if err := os.Mkdir(from, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(from, "identity"), []byte(fmt.Sprint(i)), 0o600); err != nil {
			t.Fatal(err)
		}
		workers.Go(func() {
			<-start
			if RenameNoReplace(from, to) == nil {
				successes.Add(1)
			}
		})
	}
	close(start)
	workers.Wait()
	if successes.Load() != 1 {
		t.Fatalf("publishers: %d", successes.Load())
	}
	if _, err := os.ReadFile(filepath.Join(to, "identity")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 16 {
		t.Fatalf("lost another publisher's files: %d %v", len(entries), err)
	}
}
