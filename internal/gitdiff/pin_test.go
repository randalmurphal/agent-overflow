package gitdiff

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/testutil"
)

func committedSides(t *testing.T, repo string) *Sides {
	t.Helper()
	head := headSHA(t, repo)
	return OpenedSides(repo, Pin{Old: head, New: head})
}

// Each requested path is one literal entry: a directory is its own entry,
// never its contents, pathspec magic and globs are names, spaces at either
// end are part of a name, and a path git would read differently is refused
// before git runs.
func TestEntriesReadOneLiteralPathEach(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	for name, content := range map[string]string{
		"a*b": "glob\n", "axb": "x\n", "dir/inner/deep.png": "deep\n",
		":(top)icon.png": "magic\n", " logo.png": "lead\n", "logo.png ": "trail\n",
	} {
		writeBytes(t, repo, name, []byte(content))
	}
	testutil.RunGit(t, repo, "add", "-A")
	testutil.RunGit(t, repo, "commit", "-qm", "files")
	sides := committedSides(t, repo)
	asked := []string{"a*b", "dir", "dir/inner/deep.png", "missing", ":(top)icon.png", " logo.png", "logo.png "}
	entries, err := sides.OldEntries(context.Background(), asked)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(asked) {
		t.Fatalf("entries %+v, want one per path", entries)
	}
	for path, size := range map[string]int64{"a*b": 5, "dir/inner/deep.png": 5, ":(top)icon.png": 6, " logo.png": 5, "logo.png ": 6} {
		if e := entries[path]; !e.IsFile() || e.Size != size || !IsFullOID(e.OID) {
			t.Errorf("%q = %+v, want a %d byte file", path, e, size)
		}
	}
	if e := entries["dir"]; !e.Exists || e.IsFile() || e.Mode != "040000" {
		t.Errorf("dir = %+v, want its own directory entry", e)
	}
	if e := entries["missing"]; e.Exists {
		t.Errorf("missing = %+v", e)
	}
	many := make([]string, MaxEntryPaths+1)
	for i := range many {
		many[i] = fmt.Sprintf("f%d", i)
	}
	for name, paths := range map[string][]string{
		"too many": many, "too long": {strings.Repeat("x", maxPathBytes+1)},
		"trailing slash": {"dir/"}, "parent": {"../x"}, "inner parent": {"dir/../x"},
		"absolute": {"/etc/passwd"}, "dot segment": {"dir/./inner"}, "double slash": {"dir//inner"},
		"backslash": {`dir\inner`}, "empty": {""}, "nul": {"a\x00b"},
	} {
		if _, err := sides.OldEntries(context.Background(), paths); err == nil {
			t.Errorf("%s: accepted %q", name, paths[0])
		}
	}
}

// A working-tree side pins each changed regular file by the snapshot's
// blob ID and size; an ignored file the diff leaves out is not pinned.
func TestWorktreeBlobsPinTheSnapshot(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	commitFile(t, repo, ".gitignore", "secret.png\n", "ignore")
	commitFile(t, repo, "gone.png", "gone\n", "gone")
	if err := os.Remove(filepath.Join(repo, "gone.png")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "README.txt", "changed\n")
	writeBytes(t, repo, "new.png", []byte{1, 2, 3})
	writeBytes(t, repo, "secret.png", []byte{4})

	sides, err := WorktreeSides(context.Background(), repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sides.Close() }()
	if pin := sides.Pin(); pin.New != "" || !IsFullOID(pin.Old) {
		t.Fatalf("pin %+v, want the HEAD commit and the working tree", pin)
	}
	blobs, truncated, err := sides.WorktreeBlobs(context.Background(), 10)
	if err != nil || truncated {
		t.Fatalf("WorktreeBlobs: %v %v", err, truncated)
	}
	got := map[string]WorktreeBlob{}
	for _, blob := range blobs {
		got[blob.Path] = blob
	}
	for path, content := range map[string][]byte{"README.txt": []byte("changed\n"), "new.png": {1, 2, 3}} {
		if b := got[path]; !b.IsFile() || b.Blob != BlobID(content, 40) || b.Size != int64(len(content)) {
			t.Errorf("%s pinned %+v, want %s at %d bytes", path, b, BlobID(content, 40), len(content))
		}
	}
	if _, ok := got["secret.png"]; ok {
		t.Error("an ignored file is pinned")
	}
	if b := got["gone.png"]; b.Mode != "000000" || b.IsFile() {
		t.Errorf("a deleted file pinned %+v, want mode 000000", b)
	}
}

// Pinning stops git at its bound: a change list far past it costs the
// bound, not the list.
func TestWorktreeBlobsStopAtTheBound(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	for i := range 400 {
		writeBytes(t, repo, fmt.Sprintf("f%03d.png", i), []byte{byte(i)})
	}
	sides, err := WorktreeSides(context.Background(), repo, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sides.Close() }()
	blobs, truncated, err := sides.WorktreeBlobs(context.Background(), 3)
	if err != nil || !truncated || len(blobs) != 3 {
		t.Fatalf("limit 3 of 400: %d blobs, truncated %v, err %v", len(blobs), truncated, err)
	}

	// The parser asks git to stop at the record past the bound and keeps
	// nothing after it.
	var stops int
	records := &rawRecords{limit: 5, stop: func() { stops++ }}
	record := []byte(":000000 100644 " + strings.Repeat("0", 40) + " " + strings.Repeat("1", 40) + " A\x00path.png\x00")
	written := 0
	for written < 1_000_000 && stops == 0 {
		if n, err := records.Write(record); n != len(record) || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
		written++
	}
	if stops != 1 || written != 6 || len(records.blobs) != 5 || !records.truncated {
		t.Fatalf("stopped %d times after %d records holding %d, want once after 6 holding 5", stops, written, len(records.blobs))
	}
}

// A working-tree read serves exactly the pinned blob: same bytes or
// nothing, never a file rewritten since, whatever its size.
func TestReadWorktreeFileProvesThePinnedBlob(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	content := []byte("pinned image bytes")
	writeBytes(t, repo, "img/a.png", content)
	sides := OpenedSides(repo, Pin{Old: headSHA(t, repo)})
	blob := BlobID(content, 40)
	buf := make([]byte, len(content))
	if err := sides.ReadWorktreeFile("img/a.png", blob, buf); err != nil || string(buf) != string(content) {
		t.Fatalf("read %q, err %v", buf, err)
	}
	writeBytes(t, repo, "img/a.png", []byte("PINNED image bytes"))
	if err := sides.ReadWorktreeFile("img/a.png", blob, make([]byte, len(content))); !errors.Is(err, ErrChangedSinceRead) {
		t.Fatalf("same size, other bytes: %v", err)
	}
	writeBytes(t, repo, "img/a.png", append(content, '!'))
	if err := sides.ReadWorktreeFile("img/a.png", blob, make([]byte, len(content))); !errors.Is(err, ErrChangedSinceRead) {
		t.Fatalf("grown: %v", err)
	}
	if err := os.Remove(filepath.Join(repo, "img/a.png")); err != nil {
		t.Fatal(err)
	}
	if err := sides.ReadWorktreeFile("img/a.png", blob, make([]byte, len(content))); !errors.Is(err, ErrChangedSinceRead) {
		t.Fatalf("deleted: %v", err)
	}
	if err := os.Mkdir(filepath.Join(repo, "img/a.png"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sides.ReadWorktreeFile("img/a.png", blob, make([]byte, len(content))); !errors.Is(err, ErrChangedSinceRead) {
		t.Fatalf("a directory now: %v", err)
	}
}

// A symlink swapped in after the path was checked, at the leaf or at a
// parent, cannot lead the read outside the workspace, even to a file
// holding exactly the pinned bytes.
func TestReadWorktreeFileStaysInsideTheWorkspace(t *testing.T) {
	t.Parallel()
	content := []byte("pinned image bytes")
	blob := BlobID(content, 40)
	for name, swap := range map[string]func(t *testing.T, repo, outside string){
		"leaf": func(t *testing.T, repo, outside string) {
			if err := os.Remove(filepath.Join(repo, "img/a.png")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "a.png"), filepath.Join(repo, "img/a.png")); err != nil {
				t.Fatal(err)
			}
		},
		"parent": func(t *testing.T, repo, outside string) {
			if err := os.RemoveAll(filepath.Join(repo, "img")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(repo, "img")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := testutil.InitGitRepo(t)
			outside := t.TempDir()
			writeBytes(t, outside, "a.png", content)
			writeBytes(t, repo, "img/a.png", content)
			sides := OpenedSides(repo, Pin{Old: headSHA(t, repo)})
			sides.beforeWorktreeOpen = func(string) { swap(t, repo, outside) }
			if err := sides.ReadWorktreeFile("img/a.png", blob, make([]byte, len(content))); err == nil {
				t.Fatal("a symlink swapped in before the open was followed outside the workspace")
			}
		})
	}
}

// ReadBlob writes exactly the buffer the caller reserved and refuses a
// blob of another size; git's output never grows the buffer.
func TestReadBlobFillsExactlyTheReservedBuffer(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	sides := committedSides(t, repo)
	entries, err := sides.OldEntries(context.Background(), []string{"README.txt"})
	if err != nil {
		t.Fatal(err)
	}
	entry := entries["README.txt"]
	buf := make([]byte, entry.Size)
	if err := sides.ReadBlob(context.Background(), entry.OID, buf); err != nil || string(buf) != "hello\n" {
		t.Fatalf("read %q, err %v", buf, err)
	}
	short := make([]byte, entry.Size-1, entry.Size+16)
	if err := sides.ReadBlob(context.Background(), entry.OID, short); err == nil {
		t.Fatal("a short buffer was accepted")
	}
	if len(short) != int(entry.Size-1) {
		t.Fatalf("the buffer grew to %d", len(short))
	}
	if err := sides.ReadBlob(context.Background(), entry.OID, make([]byte, entry.Size+1)); err == nil {
		t.Fatal("a long buffer was accepted")
	}
}

// core.abbrev plays no part: pins and entries are full IDs.
func TestPinsIgnoreCoreAbbrev(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "config", "core.abbrev", "4")
	commitFile(t, repo, "a.txt", "a\n", "a")
	sides, err := CommitSides(context.Background(), repo, headSHA(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	pin := sides.Pin()
	if !IsFullOID(pin.Old) || !IsFullOID(pin.New) {
		t.Fatalf("pin %+v is not full IDs", pin)
	}
	entries, err := OpenedSides(repo, pin).NewEntries(context.Background(), []string{"a.txt"})
	if err != nil || !IsFullOID(entries["a.txt"].OID) {
		t.Fatalf("entry %+v, err %v", entries["a.txt"], err)
	}
}

// The snapshot hashes a changed file's bytes as they are on disk, and so
// does the read that proves it: a CRLF file under core.autocrlf, and a
// SHA-256 repository, are served, not refused as changed.
func TestWorktreeBlobIDsAreTheBytesOnDisk(t *testing.T) {
	t.Parallel()
	autocrlf := testutil.InitGitRepo(t)
	testutil.RunGit(t, autocrlf, "config", "core.autocrlf", "true")
	commitFile(t, autocrlf, "diagram.svg", "<svg>\n</svg>\n", "svg")

	sha256Repo := t.TempDir()
	if err := testutil.RunGitAllowError(sha256Repo, "init", "-q", "--object-format=sha256"); err != nil {
		t.Fatalf("this git cannot create a SHA-256 repository: %v", err)
	}
	testutil.RunGit(t, sha256Repo, "config", "user.name", "Agent Overflow")
	testutil.RunGit(t, sha256Repo, "config", "user.email", "agent-overflow@example.com")
	commitFile(t, sha256Repo, "diagram.svg", "<svg/>\n", "svg")

	for name, tc := range map[string]struct {
		repo    string
		content []byte
		hexLen  int
	}{
		"autocrlf": {autocrlf, []byte("<svg>\r\n<g/>\r\n</svg>\r\n"), 40},
		"sha256":   {sha256Repo, []byte("<svg><g/></svg>\n"), 64},
	} {
		writeBytes(t, tc.repo, "diagram.svg", tc.content)
		sides, err := WorktreeSides(context.Background(), tc.repo, t.TempDir())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		blobs, _, err := sides.WorktreeBlobs(context.Background(), 10)
		if closeErr := sides.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil || len(blobs) != 1 || blobs[0].Blob != BlobID(tc.content, tc.hexLen) {
			t.Fatalf("%s: pinned %+v (err %v), want the ID of the bytes on disk", name, blobs, err)
		}
		buf := make([]byte, blobs[0].Size)
		if err := OpenedSides(tc.repo, sides.Pin()).ReadWorktreeFile("diagram.svg", blobs[0].Blob, buf); err != nil {
			t.Fatalf("%s: read: %v", name, err)
		}
	}
}

func TestHasFilterReadsTheFilterAttribute(t *testing.T) {
	t.Parallel()
	repo := testutil.InitGitRepo(t)
	commitFile(t, repo, ".gitattributes", "*.png filter=lfs diff=lfs merge=lfs -text\nplain.png -filter\n", "attributes")
	sides := OpenedSides(repo, Pin{Old: headSHA(t, repo)})
	for path, want := range map[string]bool{"a.png": true, "dir/b.png": true, "plain.png": false, "c.svg": false} {
		if got, err := sides.HasFilter(context.Background(), path); err != nil || got != want {
			t.Errorf("HasFilter(%s) = %v, %v; want %v", path, got, err, want)
		}
	}
}

func TestIsLFSPointer(t *testing.T) {
	t.Parallel()
	pointer := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize 12345\n"
	if !IsLFSPointer([]byte(pointer)) {
		t.Fatal("a pointer is not recognized")
	}
	for name, data := range map[string]string{
		"an image":       "\x89PNG\r\n\x1a\n",
		"a long file":    pointer + strings.Repeat("x", MaxLFSPointerBytes),
		"another prefix": "version https://example.com/spec/v1\n",
	} {
		if IsLFSPointer([]byte(data)) {
			t.Errorf("%s is taken for a pointer", name)
		}
	}
}
