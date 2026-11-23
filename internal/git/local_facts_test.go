package git

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/testutil"
)

// factGitCalls records the git commands a Core's facts reads run.
type factGitCalls struct {
	mu   sync.Mutex
	runs []string
}

func countFactGit(core *Core) *factGitCalls {
	calls := &factGitCalls{}
	core.beforeFactGit = func(args []string) {
		calls.mu.Lock()
		defer calls.mu.Unlock()
		run := args[0]
		if len(args) > 1 {
			run += " " + args[1]
		}
		calls.runs = append(calls.runs, run)
	}
	return calls
}

func (c *factGitCalls) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	runs := c.runs
	c.runs = nil
	slices.Sort(runs)
	return runs
}

func pngOf(t testing.TB, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeFactFile(t testing.TB, repo, path string, data []byte) {
	t.Helper()
	full := filepath.Join(repo, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	svgDoc      = `<svg xmlns="http://www.w3.org/2000/svg" width="10" height="12px"></svg>`
	fluidSVGDoc = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 12"></svg>`
)

// avifFixture is a real 8x3 AVIF (internal/attachment/testdata).
func avifFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../attachment/testdata/solid-8x3.avif")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

const lfsPointer = "version https://git-lfs.github.com/spec/v1\noid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\nsize 12345\n"

// factsRepo is a repository whose base commit holds base.png and whose
// head adds the files the local kinds read.
type factsRepo struct {
	dir        string
	base, head string
}

func newFactsRepo(t *testing.T) factsRepo {
	t.Helper()
	repo := initGitRepo(t)
	writeFactFile(t, repo, "docs/base.png", pngOf(t, 5, 4))
	testutil.RunGit(t, repo, "add", ".")
	testutil.RunGit(t, repo, "commit", "-q", "-m", "base commit")
	base := strings.TrimSpace(runGitStdout(t, repo, "rev-parse", "HEAD"))
	writeFactFile(t, repo, "docs/head.png", pngOf(t, 3, 2))
	writeFactFile(t, repo, "docs/icon.svg", []byte(svgDoc))
	writeFactFile(t, repo, "docs/fluid.svg", []byte(fluidSVGDoc))
	writeFactFile(t, repo, "docs/photo.avif", avifFixture(t))
	writeFactFile(t, repo, "docs/lfs.png", []byte(lfsPointer))
	writeFactFile(t, repo, "docs/notes.txt", []byte("not an image\n"))
	writeFactFile(t, repo, "code.go", []byte("package app\r\n\r\nfunc one() {}\nfunc two() {}\n"))
	writeFactFile(t, repo, "bin.dat", []byte("text\x00more\nlines\n"))
	writeFactFile(t, repo, "-dash.txt", []byte("dash\n"))
	testutil.RunGit(t, repo, "add", ".")
	testutil.RunGit(t, repo, "commit", "-q", "-m", "Head subject\nwrapped on\n\nbody text")
	head := strings.TrimSpace(runGitStdout(t, repo, "rev-parse", "HEAD"))
	return factsRepo{dir: repo, base: base, head: head}
}

func (r factsRepo) rev() PRRevision { return PRRevision{HeadSHA: r.head, BaseSHA: r.base} }

func (r factsRepo) blob(t *testing.T, commit, path string) string {
	t.Helper()
	return strings.TrimSpace(runGitStdout(t, r.dir, "rev-parse", commit+":"+path))
}

func resolveLocal(t *testing.T, core *Core, cwd string, rev PRRevision, keys ...string) map[string]ForgeFact {
	t.Helper()
	return resolveLocalAs(t, core, cwd, rev, false, keys...)
}

func resolveLocalAs(t *testing.T, core *Core, cwd string, rev PRRevision, fresh bool, keys ...string) map[string]ForgeFact {
	t.Helper()
	got, err := core.ResolveFacts(t.Context(), cwd, githubFactsPR, rev, keys, fresh)
	if err != nil {
		t.Fatalf("ResolveFacts: %v", err)
	}
	out := map[string]ForgeFact{}
	for i, fact := range got.Facts {
		if fact.Key != keys[i] {
			t.Fatalf("fact %d is %q, want %q", i, fact.Key, keys[i])
		}
		out[fact.Key] = fact
	}
	return out
}

// Every local kind answers from the repository at the revision, every
// key's object in one batch-check and the commits' subjects and lines'
// blobs in one batch read; a second call is answered from the cache with
// no git at all.
func TestLocalFactsAnswerEveryKind(t *testing.T) {
	t.Parallel()
	r := newFactsRepo(t)
	core := NewCore()
	calls := countFactGit(core)
	head := r.head
	keys := []string{
		"image:head:docs/head.png", "image:base:docs/base.png", "image:" + head + ":docs/head.png",
		"image:head:docs/icon.svg", "image:head:docs/fluid.svg", "image:head:docs/photo.avif", "image:head:docs/lfs.png", "image:head:docs/notes.txt",
		"image:head:docs/missing.png", "image:base:docs/head.png", "image:head:docs",
		"commit:" + head[:7], "commit:" + r.base, "commit:" + strings.Repeat("0", 12),
		"lines:" + head + ":code.go:1-3", "lines:" + head + ":code.go:4-4", "lines:" + head + ":code.go:4-5",
		"lines:" + head + ":bin.dat:1-1", "lines:" + head + ":-dash.txt:1-1",
		"image:" + strings.Repeat("e", 40) + ":docs/head.png",
	}
	got := resolveLocal(t, core, r.dir, r.rev(), keys...)
	headPNG, basePNG, lfs := r.blob(t, head, "docs/head.png"), r.blob(t, r.base, "docs/base.png"), r.blob(t, head, "docs/lfs.png")
	svg, fluid, avif := r.blob(t, head, "docs/icon.svg"), r.blob(t, head, "docs/fluid.svg"), r.blob(t, head, "docs/photo.avif")
	code := r.blob(t, head, "code.go")
	want := []ForgeFact{
		{Key: "image:head:docs/head.png", Status: FactFound, OID: headPNG, Size: int64(len(pngOf(t, 3, 2))), Width: 3, Height: 2, MIME: "image/png"},
		{Key: "image:base:docs/base.png", Status: FactFound, OID: basePNG, Size: int64(len(pngOf(t, 5, 4))), Width: 5, Height: 4, MIME: "image/png"},
		{Key: "image:" + head + ":docs/head.png", Status: FactFound, OID: headPNG, Size: int64(len(pngOf(t, 3, 2))), Width: 3, Height: 2, MIME: "image/png"},
		// An svg's root's size is its box; a viewBox alone is a fluid
		// ratio. An avif's primary item's size is its box.
		{Key: "image:head:docs/icon.svg", Status: FactFound, OID: svg, Size: int64(len(svgDoc)), Width: 10, Height: 12, MIME: "image/svg+xml"},
		{Key: "image:head:docs/fluid.svg", Status: FactFound, OID: fluid, Size: int64(len(fluidSVGDoc)), Width: 5, Height: 6, Fluid: true, MIME: "image/svg+xml"},
		{Key: "image:head:docs/photo.avif", Status: FactFound, OID: avif, Size: int64(len(avifFixture(t))), Width: 8, Height: 3, MIME: "image/avif"},
		// A pointer whose object is not in the local LFS store.
		{Key: "image:head:docs/lfs.png", Status: FactFound, OID: lfs, LFS: true},
		{Key: "image:head:docs/notes.txt", Status: FactAbsent},
		{Key: "image:head:docs/missing.png", Status: FactAbsent},
		{Key: "image:base:docs/head.png", Status: FactAbsent},
		{Key: "image:head:docs", Status: FactAbsent},
		{Key: "commit:" + head[:7], Status: FactFound, OID: head, Title: "Head subject wrapped on"},
		{Key: "commit:" + r.base, Status: FactFound, OID: r.base, Title: "base commit"},
		// A commit that is not local may arrive with a fetch.
		{Key: "commit:" + strings.Repeat("0", 12), Status: FactUnavailable},
		{Key: "lines:" + head + ":code.go:1-3", Status: FactFound, OID: code, Path: "code.go", Text: "package app\n\nfunc one() {}"},
		{Key: "lines:" + head + ":code.go:4-4", Status: FactFound, OID: code, Path: "code.go", Text: "func two() {}"},
		{Key: "lines:" + head + ":code.go:4-5", Status: FactAbsent},
		{Key: "lines:" + head + ":bin.dat:1-1", Status: FactAbsent},
		// A path that is an option to git is read as a path, never refused
		// by git: it answers absent without a run.
		{Key: "lines:" + head + ":-dash.txt:1-1", Status: FactAbsent},
		{Key: "image:" + strings.Repeat("e", 40) + ":docs/head.png", Status: FactUnavailable},
	}
	for _, w := range want {
		if g := got[w.Key]; !reflect.DeepEqual(g, w) {
			t.Errorf("%s = %+v\nwant %+v", w.Key, g, w)
		}
	}
	runs := calls.take()
	// One read of the local commits, one ls-tree per local commit a path
	// key names, one read of the local blobs, one batch-check, one batch
	// read, one header read per blob an image key names, and one config
	// read for the LFS pointer's store.
	if want := []string{"cat-file --batch", batchCheckRun, "cat-file blob", "cat-file blob", "cat-file blob", "cat-file blob", "cat-file blob", "cat-file blob", "cat-file blob", "config -z", entriesRun, entriesRun, commitsRun, blobsRun}; !slices.Equal(runs, want) {
		t.Fatalf("git runs = %q\nwant %q", runs, want)
	}

	// Cached: nothing runs, and every answer is the same. A head key and
	// its sha share one answer.
	again := resolveLocal(t, core, r.dir, r.rev(), keys...)
	for _, w := range want {
		if w.Status == FactUnavailable {
			continue
		}
		if !reflect.DeepEqual(again[w.Key], w) {
			t.Errorf("cached %s = %+v", w.Key, again[w.Key])
		}
	}
	// The unavailable keys are read again (an abbreviation's batch-check,
	// a commit that is not local), and the LFS pointer's object is looked
	// up again: its commit, its entry and whether its blob is local, the
	// blob's record being held.
	if runs, want := calls.take(), []string{batchCheckRun, entriesRun, commitsRun, blobsRun}; !slices.Equal(runs, want) {
		t.Fatalf("the cached call ran %q, want %q", runs, want)
	}
}

// oversized is data with the first box or chunk header it holds for kind
// (an avif's ispe, a png's IHDR) declaring 60000x60000, the png's CRC
// recomputed.
func oversized(t *testing.T, data []byte, kind string) []byte {
	t.Helper()
	out := bytes.Clone(data)
	at := bytes.Index(out, []byte(kind))
	if at < 0 {
		t.Fatalf("no %s in the fixture", kind)
	}
	sizeAt := at + 4
	if kind == "ispe" {
		// A full box: version and flags before the size.
		sizeAt += 4
	}
	binary.BigEndian.PutUint32(out[sizeAt:], 60000)
	binary.BigEndian.PutUint32(out[sizeAt+4:], 60000)
	if kind == "IHDR" {
		binary.BigEndian.PutUint32(out[at+17:], crc32.ChecksumIEEE(out[at:at+17]))
	}
	return out
}

// An image whose header declares a size past the pixel budget is no image
// a browser is handed, whatever its format: an avif answers as a png does.
func TestImageFactPastThePixelBudget(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	hugeAVIF, hugePNG := oversized(t, avifFixture(t), "ispe"), oversized(t, pngOf(t, 3, 2), "IHDR")
	writeFactFile(t, repo, "huge.avif", hugeAVIF)
	writeFactFile(t, repo, "huge.png", hugePNG)
	testutil.RunGit(t, repo, "add", ".")
	testutil.RunGit(t, repo, "commit", "-q", "-m", "huge")
	head := strings.TrimSpace(runGitStdout(t, repo, "rev-parse", "HEAD"))
	got := resolveLocal(t, NewCore(), repo, PRRevision{HeadSHA: head, BaseSHA: head}, "image:head:huge.png", "image:head:huge.avif")
	for _, key := range []string{"image:head:huge.png", "image:head:huge.avif"} {
		if want := (ForgeFact{Key: key, Status: FactAbsent}); !reflect.DeepEqual(got[key], want) {
			t.Errorf("%s = %+v, want %+v", key, got[key], want)
		}
	}
	for name, data := range map[string][]byte{"png": hugePNG, "avif": hugeAVIF} {
		if mime, err := DisplayImageMIME(data); !errors.Is(err, ErrNotDisplayImage) || !errors.Is(err, attachment.ErrPixelBudget) {
			t.Errorf("DisplayImageMIME(%s) = %q, %v; want ErrNotDisplayImage and ErrPixelBudget", name, mime, err)
		}
	}
}

// One call costs one batch-check however many keys it names.
func TestLocalFactsOneBatchCheckPerCall(t *testing.T) {
	t.Parallel()
	r := newFactsRepo(t)
	core := NewCore()
	calls := countFactGit(core)
	var keys []string
	for i := range 200 {
		keys = append(keys, fmt.Sprintf("lines:%s:code.go:%d-%d", r.head, 1+i%4, 1+i%4))
	}
	keys = append(keys, "commit:"+r.head[:9], "commit:"+r.base[:10])
	resolveLocal(t, core, r.dir, r.rev(), keys...)
	runs := calls.take()
	checks := 0
	for _, run := range runs {
		if strings.HasPrefix(run, "cat-file --batch-check") {
			checks++
		}
	}
	if checks != 1 || len(runs) != 5 {
		t.Fatalf("%d keys ran %q, want the three reads of what is local, one batch-check and one batch read", len(keys), runs)
	}
}

// Keys that cannot name a repository path, or ranges past the line cap,
// answer without running git.
func TestLocalFactsRefusePathsWithoutGit(t *testing.T) {
	t.Parallel()
	r := newFactsRepo(t)
	core := NewCore()
	calls := countFactGit(core)
	head := r.head
	want := map[string]string{
		"image:head:../escape.png": FactAbsent, "image:head:-x.png": FactAbsent, "image:head:/abs.png": FactAbsent,
		"image:head:a//b.png": FactAbsent, "lines:" + head + ":code.go:1-201": FactAbsent,
		"lines:" + head + ":code.go:3-2": FactUnavailable, "image:head:a\nb.png": FactUnavailable,
		"image:" + head[:12] + ":x.png": FactUnavailable, "commit:" + head[:6]: FactUnavailable, "commit:ABCDEF0": FactUnavailable,
	}
	got := resolveLocal(t, core, r.dir, r.rev(), slices.Collect(maps.Keys(want))...)
	for key, fact := range got {
		if fact.Status != want[key] {
			t.Errorf("%q = %+v, want %s", key, fact, want[key])
		}
	}
	if runs := calls.take(); len(runs) != 0 {
		t.Fatalf("refused keys ran git: %q", runs)
	}
	// Without a revision, head and base name nothing.
	if got := resolveLocal(t, core, r.dir, PRRevision{}, "image:head:docs/head.png"); got["image:head:docs/head.png"].Status != FactUnavailable {
		t.Fatalf("image at no head = %+v", got)
	}
	if runs := calls.take(); len(runs) != 0 {
		t.Fatalf("a key at no revision ran git: %q", runs)
	}
}

// An abbreviated id two commits share answers unavailable and is not
// held.
func TestLocalFactsAmbiguousCommitPrefix(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	tree := strings.TrimSpace(runGitStdout(t, repo, "rev-parse", "HEAD^{tree}"))
	body := func(i int) string {
		return fmt.Sprintf("tree %s\nauthor A <a@example.com> 0 +0000\ncommitter A <a@example.com> 0 +0000\n\nm%d\n", tree, i)
	}
	seen := map[string]int{}
	prefix, pair := "", [2]int{}
	for i := 0; prefix == ""; i++ {
		content := body(i)
		sum := sha1.Sum([]byte(fmt.Sprintf("commit %d\x00%s", len(content), content)))
		p := hex.EncodeToString(sum[:])[:7]
		if j, ok := seen[p]; ok {
			prefix, pair = p, [2]int{j, i}
		}
		seen[p] = i
	}
	for _, i := range pair {
		cmd := exec.Command("git", "hash-object", "-t", "commit", "-w", "--stdin")
		cmd.Dir, cmd.Stdin = repo, strings.NewReader(body(i))
		if out, err := cmd.CombinedOutput(); err != nil || !strings.HasPrefix(string(out), prefix) {
			t.Fatalf("hash-object: %v %s", err, out)
		}
	}
	core := NewCore()
	calls := countFactGit(core)
	for range 2 {
		got := resolveLocal(t, core, repo, PRRevision{}, "commit:"+prefix)
		if got["commit:"+prefix].Status != FactUnavailable {
			t.Fatalf("ambiguous %s = %+v", prefix, got)
		}
	}
	if runs := calls.take(); len(runs) != 2 {
		t.Fatalf("an ambiguous prefix was held: %q", runs)
	}
}

// localObjectIDs is the ids of the objects in repo's own store, listed
// without the promisor remote: no git fetches to list what it holds.
func localObjectIDs(t *testing.T, git, repo string) map[string]bool {
	t.Helper()
	cmd := exec.Command(git, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("listing %s's objects: %v", repo, err)
	}
	ids := map[string]bool{}
	for id := range strings.FieldsSeq(string(out)) {
		ids[id] = true
	}
	return ids
}

// In a partial clone, a path whose blob is not local answers unavailable
// without fetching it; a path the tree lacks still answers absent.
func TestLocalFactsPartialCloneDoesNotFetch(t *testing.T) {
	t.Parallel()
	r := newFactsRepo(t)
	testutil.RunGit(t, r.dir, "config", "uploadpack.allowFilter", "true")
	clone := filepath.Join(t.TempDir(), "clone")
	testutil.RunGit(t, t.TempDir(), "clone", "-q", "--no-checkout", "--filter=blob:none", "file://"+r.dir, clone)
	blob := r.blob(t, r.head, "docs/head.png")
	if localObjectIDs(t, "git", clone)[blob] {
		t.Fatal("the clone holds the blob")
	}
	core := NewCore()
	// What the blob is as an image is held from another checkout of the
	// repository: it is still not local in this one.
	if got := resolveLocal(t, core, r.dir, r.rev(), "image:head:docs/head.png"); got["image:head:docs/head.png"].Status != FactFound {
		t.Fatalf("the source's image = %+v", got)
	}
	got := resolveLocal(t, core, clone, r.rev(), "image:head:docs/head.png", "lines:"+r.head+":code.go:1-1", "image:head:docs/none.png", "commit:"+r.head[:8])
	for key, want := range map[string]string{
		"image:head:docs/head.png": FactUnavailable, "lines:" + r.head + ":code.go:1-1": FactUnavailable,
		"image:head:docs/none.png": FactAbsent, "commit:" + r.head[:8]: FactFound,
	} {
		if got[key].Status != want {
			t.Errorf("%s = %+v, want %s", key, got[key], want)
		}
	}
	if localObjectIDs(t, "git", clone)[blob] {
		t.Fatal("resolving the facts fetched the blob")
	}
}

// legacyGit installs a git that does not know GIT_NO_LAZY_FETCH: this
// machine's with the variable dropped. Asked for a promised object that is
// not local it starts a fetch, and with the transport refused it dies, as
// git 2.43 does with the variable set. It answers the real git's path.
func legacyGit(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	installMockGit(t, "#!/bin/sh\nunset GIT_NO_LAZY_FETCH\nexec '"+real+"' \"$@\"\n")
	return real
}

// On a git that fetches a promised object it is asked for, or dies with
// its whole batch when the fetch is refused, one object that is not local
// costs its own keys and nothing is fetched: cat-file is asked only for
// what the fetch-free reads found local. The clones' promisor remote
// would serve the fetch ("reachable") or is gone.
func TestLocalFactsAskGitOnlyForLocalObjects(t *testing.T) {
	type fixture struct {
		clone string
		rev   PRRevision
		// want is each key's status; notFetched the objects that are not
		// local before or after.
		want       map[string]string
		subject    string
		notFetched []string
	}
	source := func(t *testing.T) factsRepo {
		r := newFactsRepo(t)
		// Two paths of one blob.
		writeFactFile(t, r.dir, "docs/twin.svg", []byte(svgDoc))
		testutil.RunGit(t, r.dir, "add", ".")
		testutil.RunGit(t, r.dir, "commit", "-q", "-m", "twin")
		r.base, r.head = r.head, strings.TrimSpace(runGitStdout(t, r.dir, "rev-parse", "HEAD"))
		testutil.RunGit(t, r.dir, "config", "uploadpack.allowFilter", "true")
		testutil.RunGit(t, r.dir, "config", "uploadpack.allowAnySHA1InWant", "true")
		return r
	}
	fixtures := map[string]func(t *testing.T) fixture{
		// Every commit and tree is local, and of the blobs only the svg
		// two paths share.
		"blobs not local": func(t *testing.T) fixture {
			r := source(t)
			clone := filepath.Join(t.TempDir(), "clone")
			testutil.RunGit(t, t.TempDir(), "clone", "-q", "--no-checkout", "--filter=blob:none", "file://"+r.dir, clone)
			testutil.RunGit(t, clone, "cat-file", "-e", r.head+":docs/icon.svg")
			return fixture{clone: clone, rev: r.rev(), subject: "commit:" + r.head[:8], want: map[string]string{
				"image:head:docs/head.png": FactUnavailable, "lines:" + r.head + ":code.go:1-1": FactUnavailable,
				"image:base:docs/head.png": FactUnavailable, "image:head:docs/none.png": FactAbsent,
				"commit:" + r.head[:8]: FactFound, "commit:" + r.base: FactFound,
				"image:head:docs/icon.svg": FactFound, "image:head:docs/twin.svg": FactFound,
				// A directory named beside a path under it.
				"image:head:docs": FactAbsent,
			}, notFetched: []string{r.blob(t, r.head, "docs/head.png"), r.blob(t, r.head, "code.go")}}
		},
		// The head commit and what it holds are local (a checkout); the
		// base commit is promised and not local.
		"a commit not local": func(t *testing.T) fixture {
			r := source(t)
			clone := filepath.Join(t.TempDir(), "clone")
			testutil.RunGit(t, t.TempDir(), "clone", "-q", "--filter=object:type=tag", "file://"+r.dir, clone)
			return fixture{clone: clone, rev: r.rev(), subject: "commit:" + r.head, want: map[string]string{
				"commit:" + r.head: FactFound, "commit:" + r.base: FactUnavailable,
				"image:head:docs/icon.svg": FactFound, "image:head:docs/twin.svg": FactFound,
				"image:base:docs/head.png": FactUnavailable, "lines:" + r.base + ":code.go:1-1": FactUnavailable,
				"lines:" + r.head + ":code.go:1-1": FactFound,
			}, notFetched: []string{r.base}}
		},
	}
	for name, build := range fixtures {
		for _, remote := range []string{"reachable", "gone"} {
			t.Run(name+"/"+remote, func(t *testing.T) {
				f := build(t)
				if remote == "gone" {
					testutil.RunGit(t, f.clone, "remote", "set-url", "origin", "file://"+filepath.Join(t.TempDir(), "gone"))
				}
				real := legacyGit(t)
				for _, id := range f.notFetched {
					if localObjectIDs(t, real, f.clone)[id] {
						t.Fatalf("the clone holds %s", id)
					}
				}
				core := NewCore()
				var mu sync.Mutex
				checks := 0
				core.beforeFactGit = func(args []string) {
					mu.Lock()
					defer mu.Unlock()
					switch {
					case len(args) < 2:
					case strings.HasPrefix(args[1], "--batch-check"):
						checks++
					}
				}
				got := resolveLocal(t, core, f.clone, f.rev, slices.Collect(maps.Keys(f.want))...)
				for key, want := range f.want {
					if got[key].Status != want {
						t.Errorf("%s = %+v, want %s", key, got[key], want)
					}
				}
				if got[f.subject].Title == "" {
					t.Errorf("the head commit has no subject: %+v", got[f.subject])
				}
				for _, id := range f.notFetched {
					if localObjectIDs(t, real, f.clone)[id] {
						t.Errorf("resolving the facts fetched %s", id)
					}
				}
				if checks != 1 {
					t.Errorf("%d batch-checks ran, want 1", checks)
				}
			})
		}
	}
}

// Every facts read refuses every transport, so a git that does not know
// GIT_NO_LAZY_FETCH cannot fetch either: asked directly for a promised
// blob whose remote would serve it, it answers an error and the blob
// stays where it was.
func TestFactGitRefusesEveryTransport(t *testing.T) {
	r := newFactsRepo(t)
	testutil.RunGit(t, r.dir, "config", "uploadpack.allowFilter", "true")
	testutil.RunGit(t, r.dir, "config", "uploadpack.allowAnySHA1InWant", "true")
	clone := filepath.Join(t.TempDir(), "clone")
	testutil.RunGit(t, t.TempDir(), "clone", "-q", "--no-checkout", "--filter=blob:none", "file://"+r.dir, clone)
	blob := r.blob(t, r.head, "docs/head.png")
	real := legacyGit(t)
	if localObjectIDs(t, real, clone)[blob] {
		t.Fatal("the clone holds the blob")
	}
	if image, err := NewCore().BlobImage(t.Context(), clone, blob); err == nil {
		t.Errorf("BlobImage of a blob that is not local = %+v", image)
	}
	if localObjectIDs(t, real, clone)[blob] {
		t.Fatal("the read fetched the blob")
	}
}

// A header read reads at most the header of a large image, and at most
// factImageHeaderMax of one whose header it cannot find.
func TestBlobImageHeaderReadIsBounded(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	big := append(pngOf(t, 7, 9), bytes.Repeat([]byte{0xAB}, 6<<20)...)
	// A JPEG whose frame header sits past the read cap, behind APP
	// segments.
	jpeg := []byte{0xFF, 0xD8}
	for len(jpeg) < 3<<20 {
		jpeg = append(jpeg, 0xFF, 0xE1, 0xFF, 0xFF)
		jpeg = append(jpeg, bytes.Repeat([]byte{0}, 0xFFFF-2)...)
	}
	// An avif and a fluid svg behind 6 MiB: read to their first
	// factImageHead bytes, which hold an avif's meta and an svg's root.
	bigAVIF := append(avifFixture(t), bytes.Repeat([]byte{0xAB}, 6<<20)...)
	bigSVG := append([]byte(fluidSVGDoc), bytes.Repeat([]byte("<g/>"), 6<<18)...)
	writeFactFile(t, repo, "big.png", big)
	writeFactFile(t, repo, "big.avif", bigAVIF)
	writeFactFile(t, repo, "big.svg", bigSVG)
	writeFactFile(t, repo, "deep.jpg", jpeg)
	testutil.RunGit(t, repo, "add", ".")
	testutil.RunGit(t, repo, "commit", "-q", "-m", "images")
	core := NewCore()
	for _, tc := range []struct {
		path     string
		size     int
		maxRead  int64
		wantDims [2]int
		mime     string
		fluid    bool
	}{
		{"big.png", len(big), 64 << 10, [2]int{7, 9}, "image/png", false},
		{"big.avif", len(bigAVIF), factImageHead, [2]int{8, 3}, "image/avif", false},
		{"big.svg", len(bigSVG), factImageHead, [2]int{5, 6}, "image/svg+xml", true},
		{"deep.jpg", len(jpeg), factImageHeaderMax, [2]int{}, "", false},
	} {
		oid := strings.TrimSpace(runGitStdout(t, repo, "rev-parse", "HEAD:"+tc.path))
		image, _, n, err := core.readBlobImageCounted(t.Context(), repo, oid, int64(tc.size))
		if n > tc.maxRead {
			t.Errorf("%s: read %d of %d bytes, want at most %d", tc.path, n, tc.size, tc.maxRead)
		}
		if tc.wantDims == [2]int{} {
			if err == nil {
				t.Errorf("%s: no header in the cap read as %+v", tc.path, image)
			}
			continue
		}
		if err != nil || image.Width != tc.wantDims[0] || image.Height != tc.wantDims[1] || image.MIME != tc.mime || image.Fluid != tc.fluid {
			t.Errorf("%s = %+v, %v", tc.path, image, err)
		}
	}
	got := resolveLocal(t, core, repo, PRRevision{HeadSHA: strings.TrimSpace(runGitStdout(t, repo, "rev-parse", "HEAD"))}, "image:head:deep.jpg")
	if got["image:head:deep.jpg"].Status != FactUnavailable {
		t.Fatalf("an image whose header is past the cap = %+v", got)
	}
}

// BlobImage answers a local blob from one batch-check and one header
// read, then from its cache; it refuses ids that name no local blob and
// blobs over the display cap without reading them.
func TestCoreBlobImage(t *testing.T) {
	t.Parallel()
	r := newFactsRepo(t)
	core := NewCore()
	calls := countFactGit(core)
	oid := r.blob(t, r.head, "docs/head.png")
	for range 2 {
		image, err := core.BlobImage(t.Context(), r.dir, oid)
		if err != nil || !image.Displayable() || image.Width != 3 || image.Height != 2 {
			t.Fatalf("BlobImage = %+v, %v", image, err)
		}
	}
	if runs := calls.take(); len(runs) != 2 {
		t.Fatalf("two BlobImage calls ran %q, want one batch-check and one header read", runs)
	}
	for _, tc := range []struct {
		path string
		want BlobImage
	}{
		{"docs/fluid.svg", BlobImage{MIME: "image/svg+xml", Size: int64(len(fluidSVGDoc)), Width: 5, Height: 6, Fluid: true}},
		{"docs/photo.avif", BlobImage{MIME: "image/avif", Size: int64(len(avifFixture(t))), Width: 8, Height: 3}},
	} {
		tc.want.OID = r.blob(t, r.head, tc.path)
		if image, err := core.BlobImage(t.Context(), r.dir, tc.want.OID); err != nil || image != tc.want {
			t.Fatalf("BlobImage(%s) = %+v, %v; want %+v", tc.path, image, err, tc.want)
		}
	}
	if _, err := core.BlobImage(t.Context(), r.dir, "abc"); err == nil {
		t.Fatal("an abbreviated id was read")
	}
	if _, err := core.BlobImage(t.Context(), r.dir, r.head); err == nil {
		t.Fatal("a commit read as a blob")
	}
	if _, err := core.BlobImage(t.Context(), r.dir, strings.Repeat("e", 40)); err == nil {
		t.Fatal("a missing blob read")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := core.BlobImage(ctx, r.dir, r.blob(t, r.base, "docs/base.png")); err == nil {
		t.Fatal("a cancelled BlobImage answered")
	}
}

// One call answers forge and local keys together, in the request's order.
func TestResolveFactsMixedBatch(t *testing.T) {
	t.Parallel()
	r := newFactsRepo(t)
	f := newFactsForge(t, githubFactsFixture())
	keys := []string{"image:head:docs/head.png", "ref:acme/app#1", "commit:" + r.head[:7], "user:octo", "ref:acme/app#1", "unknown:x", "lines:" + r.head + ":code.go:4-4"}
	got, err := f.core.ResolveFacts(t.Context(), r.dir, githubFactsPR, r.rev(), keys, false)
	if err != nil {
		t.Fatal(err)
	}
	var statuses []string
	for i, fact := range got.Facts {
		if fact.Key != keys[i] {
			t.Fatalf("fact %d is %q, want %q", i, fact.Key, keys[i])
		}
		statuses = append(statuses, fact.Status)
	}
	if want := []string{FactFound, FactFound, FactFound, FactFound, FactFound, FactUnavailable, FactFound}; !slices.Equal(statuses, want) {
		t.Fatalf("statuses = %q, want %q", statuses, want)
	}
	if !reflect.DeepEqual(got.Facts[1], got.Facts[4]) || got.Facts[1].Title != "Issue one" {
		t.Fatalf("a repeated key answered %+v and %+v", got.Facts[1], got.Facts[4])
	}
	if reqs := f.requests(t, 0); len(reqs) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
}

// A lines read visits only the ranges that hold the line being read: 256
// ranges over a blob of a million one-byte lines cost the lines they
// capture, not the ranges times the lines.
func TestLinesSinkVisitsOnlyTheRangesHoldingTheLine(t *testing.T) {
	t.Parallel()
	const lines, write = 1_000_000, 4093
	blob := bytes.Repeat([]byte("a\n"), lines)
	if len(blob) > maxFactLinesBlobBytes {
		t.Fatalf("a %d byte blob is over the lines cap", len(blob))
	}
	sink := &linesSink{}
	add := func(start, end int) {
		sink.ranges = append(sink.ranges, &lineRange{at: len(sink.ranges), start: start, end: end})
	}
	// Added out of order, overlapping in pairs, the last two at the
	// blob's end and one line past it.
	for i := range 254 {
		start := 1 + (253-i)*3900
		if i%2 == 1 {
			start = sink.ranges[i-1].start + 50
		}
		add(start, start+maxFactLines-1)
	}
	add(lines-maxFactLines+1, lines)
	add(lines-maxFactLines+2, lines+1)
	captured := 0
	for _, r := range sink.ranges {
		captured += min(r.end, lines) - r.start + 1
	}
	// Writes of an odd size split lines between them, as git's pipe can.
	for p := blob; len(p) > 0; p = p[min(write, len(p)):] {
		sink.write(p[:min(write, len(p))])
	}
	for _, r := range sink.ranges {
		text, ok := sink.text(r)
		if r.end > lines {
			if ok {
				t.Errorf("range %d-%d past the end answered", r.start, r.end)
			}
			continue
		}
		if want := strings.TrimSuffix(strings.Repeat("a\n", r.end-r.start+1), "\n"); !ok || text != want {
			t.Errorf("range %d-%d = %d bytes, %v", r.start, r.end, len(text), ok)
		}
	}
	// One visit per captured line, and one more per range for a line a
	// write split.
	if bound := captured + (len(blob)/write+1)*len(sink.ranges); sink.visits > bound {
		t.Fatalf("%d ranges over %d lines made %d range visits, want at most %d", len(sink.ranges), lines, sink.visits, bound)
	}
}

// A found abbreviated commit id is held for the forge facts' freshness,
// since a later fetch can make it ambiguous; a full id is held for good.
func TestLocalFactsHoldAnAbbreviatedCommitForTheWindow(t *testing.T) {
	t.Parallel()
	r := newFactsRepo(t)
	core := NewCore()
	var offset atomic.Int64
	start := time.Now()
	core.nowFn = func() time.Time { return start.Add(time.Duration(offset.Load())) }
	calls := countFactGit(core)
	abbreviated, full := "commit:"+r.head[:12], "commit:"+r.head
	resolveLocal(t, core, r.dir, PRRevision{}, abbreviated, full)
	calls.take()

	offset.Store(int64(changingFactTTL - time.Second))
	resolveLocal(t, core, r.dir, PRRevision{}, abbreviated, full)
	if runs := calls.take(); len(runs) != 0 {
		t.Fatalf("within the window the commits ran %q", runs)
	}
	offset.Store(int64(changingFactTTL + time.Second))
	resolveLocal(t, core, r.dir, PRRevision{}, full)
	if runs := calls.take(); len(runs) != 0 {
		t.Fatalf("a full id was read again after the window: %q", runs)
	}
	got := resolveLocal(t, core, r.dir, PRRevision{}, abbreviated)
	if got[abbreviated].Status != FactFound || got[abbreviated].OID != r.head {
		t.Fatalf("abbreviated = %+v", got[abbreviated])
	}
	if runs := calls.take(); len(runs) != 2 {
		t.Fatalf("after the window the abbreviation ran %q, want a batch-check and a batch read", runs)
	}
}

// A reload reads again a local fact held for a window (an abbreviated
// commit id) and not one held for good (keys named by a full object id).
func TestFreshLocalFactsReadOnlyWhatCanChange(t *testing.T) {
	t.Parallel()
	r := newFactsRepo(t)
	core := NewCore()
	calls := countFactGit(core)
	forGood := []string{"commit:" + r.head, "image:" + r.head + ":docs/head.png", "lines:" + r.head + ":code.go:4-4"}
	abbreviated := "commit:" + r.head[:12]
	resolveLocal(t, core, r.dir, PRRevision{}, append(forGood, abbreviated)...)
	calls.take()

	got := resolveLocalAs(t, core, r.dir, PRRevision{}, true, forGood...)
	if runs := calls.take(); len(runs) != 0 {
		t.Fatalf("a reload read facts held for good: %q", runs)
	}
	if got[forGood[1]].Status != FactFound || got[forGood[1]].Width != 3 {
		t.Fatalf("a held image on a reload = %+v", got[forGood[1]])
	}
	got = resolveLocalAs(t, core, r.dir, PRRevision{}, true, abbreviated)
	if got[abbreviated].Status != FactFound || got[abbreviated].OID != r.head {
		t.Fatalf("abbreviated on a reload = %+v", got[abbreviated])
	}
	if runs := calls.take(); len(runs) != 2 {
		t.Fatalf("a reload of the abbreviation ran %q, want a batch-check and a batch read", runs)
	}
	resolveLocal(t, core, r.dir, PRRevision{}, abbreviated)
	if runs := calls.take(); len(runs) != 0 {
		t.Fatalf("the reload's answer is not held: an ordinary call ran %q", runs)
	}
}

// Paths whose bytes exceed the argv bound run as several ls-tree calls,
// and every path is still answered.
func TestPathEntriesSplitsArgvByPathBytes(t *testing.T) {
	t.Parallel()
	r := newFactsRepo(t)
	core := NewCore()
	calls := countFactGit(core)
	long := func(i int) string { return fmt.Sprintf("missing/%04d/", i) + strings.Repeat("x", 3000) }
	var paths, keys []string
	total := 0
	for i := range 30 {
		paths = append(paths, long(i))
		keys = append(keys, "image:head:"+long(i))
		total += len(long(i))
	}
	if total <= gitdiff.MaxEntryPathBytes || len(paths) > gitdiff.MaxEntryPaths {
		t.Fatalf("%d paths of %d bytes are within one run's bounds", len(paths), total)
	}
	entries, err := core.pathEntries(t.Context(), r.dir, r.head, append(slices.Clone(paths), "docs/head.png", "code.go"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]pathEntry{
		"docs/head.png": {kind: "blob", oid: r.blob(t, r.head, "docs/head.png")},
		"code.go":       {kind: "blob", oid: r.blob(t, r.head, "code.go")},
	}
	if !maps.Equal(entries, want) {
		t.Fatalf("entries = %v, want the two paths the tree holds", entries)
	}
	if runs := calls.take(); len(runs) < 2 {
		t.Fatalf("%d path bytes ran %q, want more than one ls-tree", total+len("docs/head.pngcode.go"), runs)
	}
	got := resolveLocal(t, core, r.dir, r.rev(), keys...)
	for _, key := range keys {
		if got[key].Status != FactAbsent {
			t.Errorf("%.40s... = %+v, want absent", key, got[key])
		}
	}
}

// A path answers as its own entry: a directory as a tree whether or not a
// path under it is named beside it, a submodule as a commit, and a path
// under a file or absent from the tree as nothing. Two paths of one blob
// both answer it, and both read as local.
func TestPathEntriesNameTheirOwnEntry(t *testing.T) {
	t.Parallel()
	r := newFactsRepo(t)
	writeFactFile(t, r.dir, "docs/twin.png", pngOf(t, 3, 2))
	writeFactFile(t, r.dir, "docs/deep/more/leaf.txt", []byte("leaf\n"))
	testutil.RunGit(t, r.dir, "add", ".")
	testutil.RunGit(t, r.dir, "update-index", "--add", "--cacheinfo", "160000,"+r.base+",module")
	testutil.RunGit(t, r.dir, "commit", "-q", "-m", "more")
	head := strings.TrimSpace(runGitStdout(t, r.dir, "rev-parse", "HEAD"))
	core := NewCore()
	png, leaf := r.blob(t, head, "docs/head.png"), r.blob(t, head, "docs/deep/more/leaf.txt")
	for name, tc := range map[string]struct {
		paths []string
		want  map[string]pathEntry
	}{
		"a directory alone": {[]string{"docs"}, map[string]pathEntry{"docs": {kind: "tree"}}},
		"a directory beside paths under it": {
			[]string{"docs", "docs/deep", "docs/head.png", "docs/twin.png", "docs/deep/more/leaf.txt", "docs/none.png", "docs/head.png/under", "module", "module/x"},
			map[string]pathEntry{
				"docs": {kind: "tree"}, "docs/deep": {kind: "tree"},
				"docs/head.png": {kind: "blob", oid: png}, "docs/twin.png": {kind: "blob", oid: png},
				"docs/deep/more/leaf.txt": {kind: "blob", oid: leaf},
				"module":                  {kind: "commit"},
			},
		},
		"a directory beside a path it lacks": {[]string{"docs", "docs/none.png"}, map[string]pathEntry{"docs": {kind: "tree"}}},
	} {
		entries, err := core.pathEntries(t.Context(), r.dir, head, tc.paths)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !maps.Equal(entries, tc.want) {
			t.Errorf("%s: entries = %v\nwant %v", name, entries, tc.want)
		}
	}
	missing := strings.Repeat("e", 40)
	local, err := core.localObjects(t.Context(), r.dir, []string{png, leaf, missing})
	if err != nil {
		t.Fatal(err)
	}
	if len(local) != 2 || !local[png] || !local[leaf] {
		t.Fatalf("local objects = %v, want %s and %s", local, png, leaf)
	}
	// An id that names a tree lists what the tree holds: the run fails.
	if local, err := core.localObjects(t.Context(), r.dir, []string{strings.TrimSpace(runGitStdout(t, r.dir, "rev-parse", head+"^{tree}"))}); err == nil {
		t.Fatalf("a tree's id answered %v", local)
	}
	got := resolveLocal(t, core, r.dir, PRRevision{HeadSHA: head}, "image:head:docs/head.png", "image:head:docs/twin.png", "image:head:docs", "image:head:module")
	for key, want := range map[string]string{
		"image:head:docs/head.png": FactFound, "image:head:docs/twin.png": FactFound,
		"image:head:docs": FactAbsent, "image:head:module": FactAbsent,
	} {
		if got[key].Status != want {
			t.Errorf("%s = %+v, want %s", key, got[key], want)
		}
	}
}

// Two directories that hold the same files are one tree to git: a path in
// each still answers.
func TestLocalFactsInDirectoriesOfOneTree(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	for _, dir := range []string{"a", "b"} {
		writeFactFile(t, repo, dir+"/one.txt", []byte("one\n"))
		writeFactFile(t, repo, dir+"/two.txt", []byte("two\n"))
	}
	testutil.RunGit(t, repo, "add", ".")
	testutil.RunGit(t, repo, "commit", "-q", "-m", "twins")
	head := strings.TrimSpace(runGitStdout(t, repo, "rev-parse", "HEAD"))
	one, two := "lines:"+head+":a/one.txt:1-1", "lines:"+head+":b/two.txt:1-1"
	got := resolveLocal(t, NewCore(), repo, PRRevision{}, one, two)
	if got[one].Text != "one" || got[two].Text != "two" {
		t.Fatalf("lines = %+v and %+v", got[one], got[two])
	}
}

// wideRepo is a repository whose directory "wide" holds n files of long
// names besides images i0.png to i5.png, and outside.txt at its root; it
// answers the repository and its head.
func wideRepo(t *testing.T, n int) (string, string) {
	t.Helper()
	repo := initGitRepo(t)
	writeFactFile(t, repo, "outside.txt", []byte("outside\n"))
	for i := range 6 {
		writeFactFile(t, repo, fmt.Sprintf("wide/i%d.png", i), pngOf(t, 3+i, 2))
	}
	testutil.RunGit(t, repo, "add", ".")
	blob := strings.TrimSpace(runGitStdout(t, repo, "rev-parse", ":outside.txt"))
	var index strings.Builder
	for i := range n {
		fmt.Fprintf(&index, "100644 %s\twide/%s%06d\n", blob, strings.Repeat("x", 230), i)
	}
	cmd := exec.Command("git", "update-index", "--index-info")
	cmd.Dir, cmd.Stdin = repo, strings.NewReader(index.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-index: %v %s", err, out)
	}
	testutil.RunGit(t, repo, "commit", "-q", "-m", "wide")
	return repo, strings.TrimSpace(runGitStdout(t, repo, "rev-parse", "HEAD"))
}

// A directory named beside a path under it makes git list the directory
// whole. However wide it is, its paths and the other paths of the call
// still answer.
func TestLocalFactsBesideAWideDirectory(t *testing.T) {
	t.Parallel()
	repo, head := wideRepo(t, 20000)
	rev := PRRevision{HeadSHA: head}
	outside := "lines:" + head + ":outside.txt:1-1"
	got := resolveLocal(t, NewCore(), repo, rev, "image:head:wide", "image:head:wide/none.png", "image:head:wide/i0.png", outside)
	for key, want := range map[string]string{
		"image:head:wide": FactAbsent, "image:head:wide/none.png": FactAbsent,
		"image:head:wide/i0.png": FactFound, outside: FactFound,
	} {
		if got[key].Status != want {
			t.Errorf("%s = %+v, want %s", key, got[key], want)
		}
	}
}

// What a call holds afterwards is its facts, not git's listing: six
// lookups that each made git list a wide directory hold none of it. It
// reads the process's heap, so it runs alone.
func TestLocalFactsHoldNoneOfGitsListing(t *testing.T) {
	repo, head := wideRepo(t, 6000)
	rev := PRRevision{HeadSHA: head}
	core := NewCore()
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return stats.HeapAlloc
	}
	before := heap()
	for i := range 6 {
		key := fmt.Sprintf("image:head:wide/i%d.png", i)
		if got := resolveLocal(t, core, repo, rev, "image:head:wide", key); got[key].Status != FactFound {
			t.Fatalf("%s = %+v", key, got[key])
		}
	}
	after := heap()
	runtime.KeepAlive(core)
	// Each listing is about 1.7 MiB.
	if after > before && after-before > 1<<20 {
		t.Fatalf("six lookups left %d bytes on the heap, want under 1 MiB", after-before)
	}
}
