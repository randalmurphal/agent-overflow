package git

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	// A fluid svg: its fact carries the ratio as fluid.
	fluid := []byte(fluidSVGDoc)
	writeLFSObject(t, r.store, sha256Hex(fluid), fluid)
	pointer("fluid.svg", sha256Hex(fluid), int64(len(fluid)))
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
	paths := []string{"present.png", "fluid.svg", "wrong-size.png", "wrong-hash.png", "absent.png", "text.png", "over-cap.png", "link.png"}
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
		{Key: "image:head:fluid.svg", Status: FactFound, OID: r.blob(t, "fluid.svg"), Size: int64(len(fluidSVGDoc)), Width: 5, Height: 6, Fluid: true, MIME: "image/svg+xml"},
		lfs("wrong-size.png"),
		// The header is read without the hash: found, its bytes refused.
		{Key: "image:head:wrong-hash.png", Status: FactFound, OID: r.blob(t, "wrong-hash.png"), Size: int64(len(r.wrong)), Width: 9, Height: 4, MIME: "image/png"},
		lfs("absent.png"),
		{Key: "image:head:text.png", Status: FactAbsent},
		lfs("over-cap.png"),
		lfs("link.png"),
	} {
		if !reflect.DeepEqual(got[want.Key], want) {
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

// lfsLookups counts a Core's facts git runs and the LFS objects its
// lookups open to classify, with the clock stopped.
type lfsLookups struct {
	calls  *factGitCalls
	reads  atomic.Int64
	offset atomic.Int64
}

func countLFSLookups(core *Core) *lfsLookups {
	l := &lfsLookups{calls: countFactGit(core)}
	start := time.Now()
	core.nowFn = func() time.Time { return start.Add(time.Duration(l.offset.Load())) }
	core.beforeLFSObjectRead = func(string) { l.reads.Add(1) }
	return l
}

// expect fails unless the lookups since the last call ran exactly runs
// (sorted) and opened reads objects.
func (l *lfsLookups) expect(t *testing.T, step string, reads int64, runs ...string) {
	t.Helper()
	if got := l.calls.take(); !slices.Equal(got, runs) {
		t.Errorf("%s ran %q, want %q", step, got, runs)
	}
	if got := l.reads.Swap(0); got != reads {
		t.Errorf("%s opened %d LFS objects, want %d", step, got, reads)
	}
}

const batchCheckRun = "cat-file --batch-check=%(objectname) %(objecttype) %(objectsize)"

// Whether a pointer's object is local is decided on every lookup, so a
// pull or a prune shows on the next one with the clock stopped. What the
// pointer is, the store's directory, and a present object's header are
// held: a repeat lookup runs no `git cat-file blob` and no `git config`
// and reads no object bytes. A fact read through a pointer is never held,
// so each call costs one batch-check to find the key's blob.
func TestLocalFactsLFSObjectIsLookedUpEveryTime(t *testing.T) {
	t.Parallel()
	r := newLFSRepo(t)
	core := NewCore()
	l := countLFSLookups(core)
	pulled := pngOf(t, 8, 3)
	writeFactFile(t, r.dir, "pulled.png", []byte(lfsPointerText(sha256Hex(pulled), int64(len(pulled)))))
	testutil.RunGit(t, r.dir, "add", "pulled.png")
	testutil.RunGit(t, r.dir, "commit", "-qm", "pulled")
	r.head = strings.TrimSpace(runGitStdout(t, r.dir, "rev-parse", "HEAD"))
	rev := PRRevision{HeadSHA: r.head}
	key, blob := "image:head:pulled.png", r.blob(t, "pulled.png")
	notLocal := ForgeFact{Key: key, Status: FactFound, OID: blob, LFS: true}
	found := ForgeFact{Key: key, Status: FactFound, OID: blob, Size: int64(len(pulled)), Width: 8, Height: 3, MIME: "image/png"}
	lookup := func(step string, want ForgeFact) {
		t.Helper()
		if got := resolveLocal(t, core, r.dir, rev, key)[key]; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: fact = %+v\nwant %+v", step, got, want)
		}
		image, err := core.BlobImage(t.Context(), r.dir, blob)
		switch {
		case err != nil:
			t.Fatalf("%s: BlobImage: %v", step, err)
		case image.LFS != want.LFS || (!want.LFS && image.Size != want.Size) || image.Width != want.Width || image.Height != want.Height || image.MIME != want.MIME:
			t.Fatalf("%s: BlobImage = %+v, want the fact %+v", step, image, want)
		case !want.LFS && image.LFSObject() != sha256Hex(pulled):
			t.Fatalf("%s: BlobImage reads object %q", step, image.LFSObject())
		}
	}

	lookup("not pulled", notLocal)
	l.expect(t, "the first lookup", 0, batchCheckRun, "cat-file blob", "config -z")
	lookup("not pulled, again", notLocal)
	l.expect(t, "a repeat lookup of a pointer not local", 0, batchCheckRun)

	path := writeLFSObject(t, r.store, sha256Hex(pulled), pulled)
	lookup("pulled", found)
	l.expect(t, "the lookup after the pull", 1, batchCheckRun)
	lookup("pulled, again", found)
	l.expect(t, "a repeat lookup of a present object", 0, batchCheckRun)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	lookup("pruned", notLocal)
	l.expect(t, "the lookup after the prune", 0, batchCheckRun)
	// Pulled again: another file, so its header is read again.
	path = writeLFSObject(t, r.store, sha256Hex(pulled), pulled)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	lookup("pulled again", found)
	l.expect(t, "the lookup after a second pull", 1, batchCheckRun)
}

// A file of the pointer's size that is not its object is classified by
// what it holds; the object that replaces it is classified again, not
// answered from what the first file was.
func TestLocalLFSObjectReplacedIsClassifiedAgain(t *testing.T) {
	t.Parallel()
	r := newLFSRepo(t)
	core := NewCore()
	object := pngOf(t, 8, 3)
	writeFactFile(t, r.dir, "fixed.png", []byte(lfsPointerText(sha256Hex(object), int64(len(object)))))
	testutil.RunGit(t, r.dir, "add", "fixed.png")
	testutil.RunGit(t, r.dir, "commit", "-qm", "fixed")
	r.head = strings.TrimSpace(runGitStdout(t, r.dir, "rev-parse", "HEAD"))
	blob := r.blob(t, "fixed.png")
	path := writeLFSObject(t, r.store, sha256Hex(object), bytes.Repeat([]byte("x"), len(object)))
	first := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, first, first); err != nil {
		t.Fatal(err)
	}
	if image, err := core.BlobImage(t.Context(), r.dir, blob); err != nil || image.Displayable() {
		t.Fatalf("a corrupt object = %+v, %v; want no image", image, err)
	}
	if err := os.WriteFile(path, object, 0o644); err != nil {
		t.Fatal(err)
	}
	second := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, second, second); err != nil {
		t.Fatal(err)
	}
	image, err := core.BlobImage(t.Context(), r.dir, blob)
	if err != nil || image.MIME != "image/png" || image.Width != 8 || image.Height != 3 {
		t.Fatalf("the object that replaced it = %+v, %v; want an 8x3 png", image, err)
	}
}

// A failed close of an LFS object fails the read that opened it, and a
// classification whose close failed is not held.
func TestLocalLFSCloseFailureFailsTheRead(t *testing.T) {
	t.Parallel()
	r := newLFSRepo(t)
	core := NewCore()
	l := countLFSLookups(core)
	errClose := errors.New("close failed")
	var fail atomic.Bool
	core.lfsObjectClosed = func(err error) error {
		if fail.Load() {
			return errors.Join(err, errClose)
		}
		return err
	}
	blob := r.blob(t, "present.png")
	fail.Store(true)
	if image, err := core.BlobImage(t.Context(), r.dir, blob); !errors.Is(err, errClose) {
		t.Fatalf("BlobImage with a failed close = %+v, %v", image, err)
	}
	fail.Store(false)
	image, err := core.BlobImage(t.Context(), r.dir, blob)
	if err != nil || image.Width != 6 {
		t.Fatalf("BlobImage = %+v, %v", image, err)
	}
	l.expect(t, "the classifications", 2, batchCheckRun, "cat-file blob", "config -z")
	fail.Store(true)
	if err := core.ReadImage(t.Context(), r.dir, image, make([]byte, image.Size)); !errors.Is(err, errClose) {
		t.Fatalf("ReadImage with a failed close = %v", err)
	}
	fail.Store(false)
	if err := core.ReadImage(t.Context(), r.dir, image, make([]byte, image.Size)); err != nil {
		t.Fatalf("ReadImage = %v", err)
	}
}

// The store's directory is held per workspace for changingFactTTL: within
// it a changed lfs.storage costs no `git config`, after it the change is
// honored.
func TestLocalFactsLFSStoreDirectoryIsHeldForTheWindow(t *testing.T) {
	t.Parallel()
	r := newLFSRepo(t)
	core := NewCore()
	l := countLFSLookups(core)
	key, blob := "image:head:present.png", r.blob(t, "present.png")
	rev := PRRevision{HeadSHA: r.head}
	if got := resolveLocal(t, core, r.dir, rev, key)[key]; got.LFS || got.Width != 6 {
		t.Fatalf("present = %+v", got)
	}
	l.expect(t, "the first lookup", 1, batchCheckRun, "cat-file blob", "config -z")

	testutil.RunGit(t, r.dir, "config", "lfs.storage", "elsewhere")
	l.offset.Store(int64(changingFactTTL - time.Second))
	if got := resolveLocal(t, core, r.dir, rev, key)[key]; got.LFS || got.Width != 6 {
		t.Fatalf("within the window = %+v, want the held store's object", got)
	}
	if image, err := core.BlobImage(t.Context(), r.dir, blob); err != nil || image.LFS {
		t.Fatalf("BlobImage within the window = %+v, %v", image, err)
	}
	l.expect(t, "within the window", 0, batchCheckRun)

	l.offset.Store(int64(changingFactTTL))
	if got := resolveLocal(t, core, r.dir, rev, key)[key]; !got.LFS {
		t.Fatalf("after the window = %+v, want lfs.storage's empty store", got)
	}
	if image, err := core.BlobImage(t.Context(), r.dir, blob); err != nil || !image.LFS {
		t.Fatalf("BlobImage after the window = %+v, %v", image, err)
	}
	l.expect(t, "after the window", 0, batchCheckRun, "config -z")
}
