package git

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

func TestTransferObjectsPreserveStagingAndPrivateCommits(t *testing.T) {
	for _, sharedBase := range []bool{true, false} {
		t.Run(map[bool]string{true: "existing clone", false: "missing objects"}[sharedBase], func(t *testing.T) {
			ctx := context.Background()
			core := NewCore()
			source := testutil.InitGitRepo(t)
			baseline, _, err := core.Execute(source, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			destination := t.TempDir()
			if sharedBase {
				testutil.RunGit(t, destination, "clone", "--no-checkout", source, ".")
			} else {
				testutil.RunGit(t, destination, "init")
			}
			if err := os.WriteFile(filepath.Join(source, "private.txt"), []byte("not on the other computer\n"), 0600); err != nil {
				t.Fatal(err)
			}
			testutil.RunGit(t, source, "add", ".")
			testutil.RunGit(t, source, "commit", "-m", "private commit")
			staged := bytes.Repeat([]byte{0, 1, 2, 3, 255}, 2000)
			if err := os.WriteFile(filepath.Join(source, "binary data.bin"), staged, 0600); err != nil {
				t.Fatal(err)
			}
			testutil.RunGit(t, source, "rm", "README.txt")
			testutil.RunGit(t, source, "add", ".")
			if err := os.WriteFile(filepath.Join(source, "binary data.bin"), []byte("unstaged edit"), 0600); err != nil {
				t.Fatal(err)
			}
			head, _, err := core.Execute(source, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			beforeRefs, _, _ := core.Execute(source, "show-ref")
			beforeIndex, err := os.ReadFile(filepath.Join(source, ".git", "index"))
			if err != nil {
				t.Fatal(err)
			}
			index, err := core.ReadTransferIndex(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			have := []string{strings.Repeat("f", 40)} // Unknown commits are harmless hints.
			if sharedBase {
				have = append(have, strings.TrimSpace(baseline))
			}
			var pack bytes.Buffer
			if err := core.WriteTransferObjects(ctx, source, strings.TrimSpace(head), index, have, &pack); err != nil {
				t.Fatal(err)
			}
			if err := core.ImportTransferObjects(ctx, destination, &pack); err != nil {
				t.Fatal(err)
			}
			testutil.RunGit(t, destination, "update-ref", "HEAD", strings.TrimSpace(head))
			if err := core.RestoreTransferIndex(ctx, destination, index); err != nil {
				t.Fatal(err)
			}
			testutil.RunGit(t, destination, "checkout-index", "-a", "-f")
			actual, err := os.ReadFile(filepath.Join(destination, "binary data.bin"))
			if err != nil || !bytes.Equal(actual, staged) {
				t.Fatalf("staged blob was lost: bytes %d %v", len(actual), err)
			}
			if _, err := os.Stat(filepath.Join(destination, "README.txt")); !os.IsNotExist(err) {
				t.Fatal("staged deletion was lost")
			}
			after, err := core.ReadTransferIndex(ctx, destination)
			if err != nil || !reflect.DeepEqual(index, after) {
				t.Fatalf("index changed: %+v %v", after, err)
			}
			afterRefs, _, _ := core.Execute(source, "show-ref")
			afterIndex, err := os.ReadFile(filepath.Join(source, ".git", "index"))
			if err != nil || !bytes.Equal(beforeIndex, afterIndex) || beforeRefs != afterRefs {
				t.Fatal("snapshot changed source refs or index")
			}
		})
	}
}

func TestTransferIndexRejectsInconsistentSnapshotsBeforeWriting(t *testing.T) {
	oid := strings.Repeat("a", 40)
	entry := func(path string) TransferIndexEntry { return TransferIndexEntry{Mode: "100644", OID: oid, Path: path} }
	for name, entries := range map[string][]TransferIndexEntry{
		"traversal": {entry("../outside")}, "case": {entry("A"), entry("a")},
		"prefix": {entry("a"), entry("a-"), entry("a/b")}, "wrong object": {{Mode: "100644", OID: "--all", Path: "a"}},
		"mode": {{Mode: "040000", OID: oid, Path: "a"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := NewCore().RestoreTransferIndex(context.Background(), "does-not-exist", entries); err == nil {
				t.Fatal("invalid index reached git")
			}
		})
	}
}
