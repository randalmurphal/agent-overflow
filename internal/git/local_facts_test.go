package git

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"image"
	"image/png"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

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

const svgDoc = `<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"></svg>`

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
	got, err := core.ResolveFacts(t.Context(), cwd, githubFactsPR, rev, keys)
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
		"image:head:docs/icon.svg", "image:head:docs/lfs.png", "image:head:docs/notes.txt",
		"image:head:docs/missing.png", "image:base:docs/head.png", "image:head:docs",
		"commit:" + head[:7], "commit:" + r.base, "commit:" + strings.Repeat("0", 12),
		"lines:" + head + ":code.go:1-3", "lines:" + head + ":code.go:4-4", "lines:" + head + ":code.go:4-5",
		"lines:" + head + ":bin.dat:1-1", "lines:" + head + ":-dash.txt:1-1",
		"image:" + strings.Repeat("e", 40) + ":docs/head.png",
	}
	got := resolveLocal(t, core, r.dir, r.rev(), keys...)
	headPNG, basePNG, lfs := r.blob(t, head, "docs/head.png"), r.blob(t, r.base, "docs/base.png"), r.blob(t, head, "docs/lfs.png")
	svg := r.blob(t, head, "docs/icon.svg")
	code := r.blob(t, head, "code.go")
	want := []ForgeFact{
		{Key: "image:head:docs/head.png", Status: FactFound, OID: headPNG, Size: int64(len(pngOf(t, 3, 2))), Width: 3, Height: 2, MIME: "image/png"},
		{Key: "image:base:docs/base.png", Status: FactFound, OID: basePNG, Size: int64(len(pngOf(t, 5, 4))), Width: 5, Height: 4, MIME: "image/png"},
		{Key: "image:" + head + ":docs/head.png", Status: FactFound, OID: headPNG, Size: int64(len(pngOf(t, 3, 2))), Width: 3, Height: 2, MIME: "image/png"},
		{Key: "image:head:docs/icon.svg", Status: FactFound, OID: svg, Size: int64(len(svgDoc)), MIME: "image/svg+xml"},
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
		if g := got[w.Key]; g != w {
			t.Errorf("%s = %+v\nwant %+v", w.Key, g, w)
		}
	}
	runs := calls.take()
	// One batch-check, one batch read, one header read per blob an image
	// key names, and one ls-tree per commit with a path the batch-check
	// called missing.
	if want := []string{"cat-file --batch", "cat-file --batch-check=%(objectname) %(objecttype) %(objectsize)", "cat-file blob", "cat-file blob", "cat-file blob", "cat-file blob", "cat-file blob", "ls-tree -z", "ls-tree -z"}; !slices.Equal(runs, want) {
		t.Fatalf("git runs = %q\nwant %q", runs, want)
	}

	// Cached: nothing runs, and every answer is the same. A head key and
	// its sha share one answer.
	again := resolveLocal(t, core, r.dir, r.rev(), keys...)
	for _, w := range want {
		if w.Status == FactUnavailable {
			continue
		}
		if again[w.Key] != w {
			t.Errorf("cached %s = %+v", w.Key, again[w.Key])
		}
	}
	if runs := calls.take(); len(runs) != 1 || runs[0] != "cat-file --batch-check=%(objectname) %(objecttype) %(objectsize)" {
		t.Fatalf("the cached call ran %q, want only the batch-check of the unavailable keys", runs)
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
	if checks != 1 || len(runs) != 2 {
		t.Fatalf("%d keys ran %q, want one batch-check and one batch read", len(keys), runs)
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

// In a partial clone, a path whose blob is not local answers unavailable
// without fetching it; a path the tree lacks still answers absent.
func TestLocalFactsPartialCloneDoesNotFetch(t *testing.T) {
	t.Parallel()
	r := newFactsRepo(t)
	testutil.RunGit(t, r.dir, "config", "uploadpack.allowFilter", "true")
	clone := filepath.Join(t.TempDir(), "clone")
	testutil.RunGit(t, t.TempDir(), "clone", "-q", "--no-checkout", "--filter=blob:none", "file://"+r.dir, clone)
	blob := r.blob(t, r.head, "docs/head.png")
	local := func() bool {
		cmd := exec.Command("git", "cat-file", "-e", blob)
		cmd.Dir, cmd.Env = clone, append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
		return cmd.Run() == nil
	}
	if local() {
		t.Fatal("the clone holds the blob")
	}
	core := NewCore()
	got := resolveLocal(t, core, clone, r.rev(), "image:head:docs/head.png", "lines:"+r.head+":code.go:1-1", "image:head:docs/none.png", "commit:"+r.head[:8])
	for key, want := range map[string]string{
		"image:head:docs/head.png": FactUnavailable, "lines:" + r.head + ":code.go:1-1": FactUnavailable,
		"image:head:docs/none.png": FactAbsent, "commit:" + r.head[:8]: FactFound,
	} {
		if got[key].Status != want {
			t.Errorf("%s = %+v, want %s", key, got[key], want)
		}
	}
	if local() {
		t.Fatal("resolving the facts fetched the blob")
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
	writeFactFile(t, repo, "big.png", big)
	writeFactFile(t, repo, "deep.jpg", jpeg)
	testutil.RunGit(t, repo, "add", ".")
	testutil.RunGit(t, repo, "commit", "-q", "-m", "images")
	core := NewCore()
	for _, tc := range []struct {
		path     string
		size     int
		maxRead  int64
		wantDims [2]int
	}{
		{"big.png", len(big), 64 << 10, [2]int{7, 9}},
		{"deep.jpg", len(jpeg), factImageHeaderMax, [2]int{}},
	} {
		oid := strings.TrimSpace(runGitStdout(t, repo, "rev-parse", "HEAD:"+tc.path))
		image, n, err := core.readBlobImageCounted(t.Context(), repo, oid, int64(tc.size))
		if n > tc.maxRead {
			t.Errorf("%s: read %d of %d bytes, want at most %d", tc.path, n, tc.size, tc.maxRead)
		}
		if tc.wantDims == [2]int{} {
			if err == nil {
				t.Errorf("%s: no header in the cap read as %+v", tc.path, image)
			}
			continue
		}
		if err != nil || image.Width != tc.wantDims[0] || image.Height != tc.wantDims[1] || image.MIME != "image/png" {
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
	got, err := f.core.ResolveFacts(t.Context(), r.dir, githubFactsPR, r.rev(), keys)
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
	if got.Facts[1] != got.Facts[4] || got.Facts[1].Title != "Issue one" {
		t.Fatalf("a repeated key answered %+v and %+v", got.Facts[1], got.Facts[4])
	}
	if reqs := f.requests(t, 0); len(reqs) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
}
