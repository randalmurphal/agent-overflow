package git

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/testutil"
)

func lfsPointerText(oid string, size int64) string {
	return fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, size)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeLFSObject writes data into the LFS store at store as oid, the way
// git-lfs lays it out, and answers the file's path.
func writeLFSObject(t *testing.T, store, oid string, data []byte) string {
	t.Helper()
	path := filepath.Join(store, "objects", oid[:2], oid[2:4], oid)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// lfsRepo is a repository whose head commits LFS pointers, with a local
// LFS store written by hand (no git-lfs).
type lfsRepo struct {
	dir, head, store string
	// png is the image the present pointer names, wrong the one whose
	// object's bytes do not hash to its pointer.
	png, wrong []byte
}

func newLFSRepo(t *testing.T) lfsRepo {
	t.Helper()
	repo := initGitRepo(t)
	r := lfsRepo{dir: repo, store: filepath.Join(repo, ".git", "lfs"), png: pngOf(t, 6, 5), wrong: pngOf(t, 9, 4)}
	pointer := func(path string, oid string, size int64) {
		writeFactFile(t, repo, path, []byte(lfsPointerText(oid, size)))
	}
	// Present and matching.
	oid := sha256Hex(r.png)
	writeLFSObject(t, r.store, oid, r.png)
	pointer("present.png", oid, int64(len(r.png)))
	// Present, but the pointer names another size.
	other := pngOf(t, 7, 3)
	writeLFSObject(t, r.store, sha256Hex(other), other)
	pointer("wrong-size.png", sha256Hex(other), int64(len(other))+1)
	// The right size, bytes that do not hash to the pointer.
	bad := strings.Repeat("c", 64)
	writeLFSObject(t, r.store, bad, r.wrong)
	pointer("wrong-hash.png", bad, int64(len(r.wrong)))
	// Not in the store.
	pointer("absent.png", strings.Repeat("d", 64), 100)
	// An object that is no image.
	text := []byte("just text\n")
	writeLFSObject(t, r.store, sha256Hex(text), text)
	pointer("text.png", sha256Hex(text), int64(len(text)))
	// Over the display cap: a PNG header, the rest sparse.
	over := strings.Repeat("e", 64)
	big := writeLFSObject(t, r.store, over, pngOf(t, 2, 2))
	if err := os.Truncate(big, attachment.DisplayImageMaxBytes+1); err != nil {
		t.Fatal(err)
	}
	pointer("over-cap.png", over, attachment.DisplayImageMaxBytes+1)
	// A symlink in place of the object.
	linked := pngOf(t, 4, 4)
	target := filepath.Join(t.TempDir(), "target.png")
	if err := os.WriteFile(target, linked, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(r.store, "objects", sha256Hex(linked)[:2], sha256Hex(linked)[2:4], sha256Hex(linked))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	pointer("link.png", sha256Hex(linked), int64(len(linked)))
	testutil.RunGit(t, repo, "add", ".")
	testutil.RunGit(t, repo, "commit", "-q", "-m", "pointers")
	r.head = strings.TrimSpace(runGitStdout(t, repo, "rev-parse", "HEAD"))
	return r
}

func (r lfsRepo) blob(t *testing.T, path string) string {
	t.Helper()
	return strings.TrimSpace(runGitStdout(t, r.dir, "rev-parse", r.head+":"+path))
}

// A pointer whose object is in the local store answers as that image under
// the pointer blob's id; any other pointer answers lfs, and one whose
// object is no image answers absent. The bytes are served verified.
func TestLocalFactsServeLocalLFSObjects(t *testing.T) {
	t.Parallel()
	r := newLFSRepo(t)
	core := NewCore()
	calls := countFactGit(core)
	rev := PRRevision{HeadSHA: r.head}
	paths := []string{"present.png", "wrong-size.png", "wrong-hash.png", "absent.png", "text.png", "over-cap.png", "link.png"}
	var keys []string
	for _, path := range paths {
		keys = append(keys, "image:head:"+path)
	}
	got := resolveLocal(t, core, r.dir, rev, keys...)
	lfs := func(path string) ForgeFact {
		return ForgeFact{Key: "image:head:" + path, Status: FactFound, OID: r.blob(t, path), LFS: true}
	}
	for _, want := range []ForgeFact{
		{Key: "image:head:present.png", Status: FactFound, OID: r.blob(t, "present.png"), Size: int64(len(r.png)), Width: 6, Height: 5, MIME: "image/png"},
		lfs("wrong-size.png"),
		// The header is read without the hash: found, its bytes refused.
		{Key: "image:head:wrong-hash.png", Status: FactFound, OID: r.blob(t, "wrong-hash.png"), Size: int64(len(r.wrong)), Width: 9, Height: 4, MIME: "image/png"},
		lfs("absent.png"),
		{Key: "image:head:text.png", Status: FactAbsent},
		lfs("over-cap.png"),
		lfs("link.png"),
	} {
		if got[want.Key] != want {
			t.Errorf("%s = %+v\nwant %+v", want.Key, got[want.Key], want)
		}
	}
	// One store lookup for the call's pointers.
	configs := 0
	for _, run := range calls.take() {
		if run == "config -z" {
			configs++
		}
	}
	if configs != 1 {
		t.Fatalf("%d pointers read lfs.storage %d times, want once", len(paths), configs)
	}

	present, err := core.BlobImage(t.Context(), r.dir, r.blob(t, "present.png"))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, present.Size)
	if err := core.ReadImage(t.Context(), r.dir, present, buf); err != nil || !bytes.Equal(buf, r.png) {
		t.Fatalf("ReadImage(present) = %v, %d bytes", err, len(buf))
	}
	wrong, err := core.BlobImage(t.Context(), r.dir, r.blob(t, "wrong-hash.png"))
	if err != nil {
		t.Fatal(err)
	}
	buf = make([]byte, wrong.Size)
	err = core.ReadImage(t.Context(), r.dir, wrong, buf)
	if !errors.Is(err, ErrLFSObjectMismatch) {
		t.Fatalf("ReadImage(wrong hash) = %v, want ErrLFSObjectMismatch", err)
	}
	if strings.Contains(err.Error(), "PNG") {
		t.Fatalf("the refusal names content: %v", err)
	}
	// An object that left the store after it was classified.
	if err := os.Remove(present.lfsPath); err != nil {
		t.Fatal(err)
	}
	if err := core.ReadImage(t.Context(), r.dir, present, make([]byte, present.Size)); !errors.Is(err, ErrNoLocalBlob) {
		t.Fatalf("ReadImage of a removed object = %v, want ErrNoLocalBlob", err)
	}
}

// lfs.storage names the store, relative to the common dir, and a linked
// worktree finds the repository's store through git's common dir.
func TestLocalFactsLFSStorageAndWorktrees(t *testing.T) {
	t.Parallel()
	r := newLFSRepo(t)
	worktree := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, r.dir, "worktree", "add", "-q", "--detach", worktree, r.head)
	key := "image:head:present.png"
	rev := PRRevision{HeadSHA: r.head}
	if got := resolveLocal(t, NewCore(), worktree, rev, key)[key]; got.LFS || got.Width != 6 {
		t.Fatalf("from a linked worktree = %+v, want the local object", got)
	}

	testutil.RunGit(t, r.dir, "config", "lfs.storage", "elsewhere")
	if got := resolveLocal(t, NewCore(), r.dir, rev, key)[key]; !got.LFS {
		t.Fatalf("with lfs.storage elsewhere the default store answered %+v", got)
	}
	writeLFSObject(t, filepath.Join(r.dir, ".git", "elsewhere"), sha256Hex(r.png), r.png)
	if got := resolveLocal(t, NewCore(), r.dir, rev, key)[key]; got.LFS || got.Width != 6 || got.Size != int64(len(r.png)) {
		t.Fatalf("with the object under lfs.storage = %+v", got)
	}
	absolute := t.TempDir()
	testutil.RunGit(t, r.dir, "config", "lfs.storage", absolute)
	writeLFSObject(t, absolute, sha256Hex(r.png), r.png)
	if got := resolveLocal(t, NewCore(), r.dir, rev, key)[key]; got.LFS || got.Width != 6 {
		t.Fatalf("with an absolute lfs.storage = %+v", got)
	}
}

// A pointer whose object is not local is held no longer than the forge
// facts' freshness, in the facts cache and the blob image cache, so an
// object a `git lfs pull` brings is shown.
func TestLocalFactsLFSAnswerIsReadAgainAfterTheWindow(t *testing.T) {
	t.Parallel()
	r := newLFSRepo(t)
	core := NewCore()
	var offset atomic.Int64
	start := time.Now()
	core.nowFn = func() time.Time { return start.Add(time.Duration(offset.Load())) }
	calls := countFactGit(core)
	key := "image:head:absent.png"
	rev := PRRevision{HeadSHA: r.head}
	if got := resolveLocal(t, core, r.dir, rev, key)[key]; !got.LFS {
		t.Fatalf("absent = %+v", got)
	}
	calls.take()
	// The object arrives: a hand-written pull of a new pointer's object.
	pulled := pngOf(t, 8, 3)
	writeFactFile(t, r.dir, "absent.png", []byte(lfsPointerText(sha256Hex(pulled), int64(len(pulled)))))
	testutil.RunGit(t, r.dir, "commit", "-qam", "repoint")
	r.head = strings.TrimSpace(runGitStdout(t, r.dir, "rev-parse", "HEAD"))
	rev = PRRevision{HeadSHA: r.head}
	if got := resolveLocal(t, core, r.dir, rev, key)[key]; !got.LFS {
		t.Fatalf("a pointer to an object not pulled = %+v", got)
	}
	writeLFSObject(t, r.store, sha256Hex(pulled), pulled)
	calls.take()

	offset.Store(int64(changingFactTTL - time.Second))
	if got := resolveLocal(t, core, r.dir, rev, key)[key]; !got.LFS {
		t.Fatalf("within the window = %+v, want the held answer", got)
	}
	if runs := calls.take(); len(runs) != 0 {
		t.Fatalf("within the window ran %q", runs)
	}
	offset.Store(int64(2*changingFactTTL + time.Second))
	if got := resolveLocal(t, core, r.dir, rev, key)[key]; got.LFS || got.Width != 8 || got.Height != 3 {
		t.Fatalf("after the window = %+v, want the pulled object", got)
	}
	runs := calls.take()
	if !slices.Contains(runs, "config -z") || !slices.Contains(runs, "cat-file blob") {
		t.Fatalf("after the window ran %q, want the pointer and the store read again", runs)
	}
	// BlobImage answers from the same refreshed cache.
	image, err := core.BlobImage(t.Context(), r.dir, r.blob(t, "absent.png"))
	if err != nil || image.LFS || image.Width != 8 {
		t.Fatalf("BlobImage = %+v, %v", image, err)
	}
}
