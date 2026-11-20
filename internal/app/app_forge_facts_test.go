package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/reviewimage"
	"agent-overflow/internal/testutil"
)

// blobImageRepo is a repository holding one wide PNG, a text file, an LFS
// pointer and a blob just over the display cap, with their blob ids.
type blobImageRepo struct {
	dir                      string
	wide, text, lfs, overCap string
	wideBytes                []byte
}

func newBlobImageRepo(t *testing.T, overCap bool) blobImageRepo {
	t.Helper()
	repo := testutil.InitGitRepo(t)
	r := blobImageRepo{dir: repo, wideBytes: sizedPNG(t, 1000, 200)}
	writeRepoBytes(t, repo, "wide.png", r.wideBytes)
	writeRepoBytes(t, repo, "notes.txt", []byte("not an image\n"))
	writeRepoBytes(t, repo, "lfs.png", []byte("version https://git-lfs.github.com/spec/v1\noid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\nsize 12345\n"))
	if overCap {
		big := append(sizedPNG(t, 4, 4), make([]byte, attachment.DisplayImageMaxBytes)...)
		writeRepoBytes(t, repo, "big.png", big)
	}
	testutil.RunGit(t, repo, "add", "-A")
	testutil.RunGit(t, repo, "commit", "-qm", "images")
	r.wide, r.text, r.lfs = revParseIn(t, repo, "HEAD:wide.png"), revParseIn(t, repo, "HEAD:notes.txt"), revParseIn(t, repo, "HEAD:lfs.png")
	if overCap {
		r.overCap = revParseIn(t, repo, "HEAD:big.png")
	}
	return r
}

// countBlobSteps counts the reads and derivations GetGitBlobImage makes.
func countBlobSteps(app *App, during func(ctx context.Context, step string)) (reads, derives *atomic.Int32) {
	reads, derives = &atomic.Int32{}, &atomic.Int32{}
	app.gitBlobHook = func(ctx context.Context, step string) {
		switch step {
		case "read":
			reads.Add(1)
		case "derive":
			derives.Add(1)
		}
		if during != nil {
			during(ctx, step)
		}
	}
	return reads, derives
}

// A blob is served at its own size and at two derived tiers from one read,
// over the byte route, each derivative its tier wide.
func TestGetGitBlobImageServesTiersFromOneRead(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	r := newBlobImageRepo(t, false)
	ref := testWorkspaceRef(t, app, r.dir)
	reads, derives := countBlobSteps(app, nil)

	got, err := app.GetGitBlobImage(context.Background(), ref, r.wide, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := LocalImage{URL: got.URL, MimeType: "image/png", Width: 1000, Height: 200, OriginalWidth: 1000, OriginalHeight: 200, OriginalBytes: int64(len(r.wideBytes))}
	if got != want {
		t.Fatalf("GetGitBlobImage = %+v, want %+v", got, want)
	}
	if resp, body := getBytes(t, base, got.URL); resp.StatusCode != http.StatusOK || !bytes.Equal(body, r.wideBytes) {
		t.Fatalf("GET = %d with %d bytes, want the blob", resp.StatusCode, len(body))
	}
	for _, width := range []int{300, 400} {
		got, err := app.GetGitBlobImage(context.Background(), ref, r.wide, width)
		if err != nil {
			t.Fatal(err)
		}
		tier := attachment.DeriveTier(width)
		if !got.Derived || got.Width != tier || got.OriginalWidth != 1000 || got.OriginalBytes != int64(len(r.wideBytes)) {
			t.Fatalf("width %d = %+v, want a %d wide derivative", width, got, tier)
		}
		resp, body := getBytes(t, base, got.URL)
		cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
		if resp.StatusCode != http.StatusOK || err != nil || cfg.Width != tier {
			t.Fatalf("width %d served %d, %v, %d wide", width, resp.StatusCode, err, cfg.Width)
		}
	}
	// Again at every tier: nothing is read or derived.
	for _, width := range []int{0, 300, 400} {
		if _, err := app.GetGitBlobImage(context.Background(), ref, r.wide, width); err != nil {
			t.Fatal(err)
		}
	}
	if reads.Load() != 1 || derives.Load() != 2 {
		t.Fatalf("reads = %d, derivations = %d, want 1 and 2", reads.Load(), derives.Load())
	}
}

// Two concurrent calls at one tier share one read and one derivation.
func TestGetGitBlobImageConcurrentCallsDeriveOnce(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	r := newBlobImageRepo(t, false)
	ref := testWorkspaceRef(t, app, r.dir)
	deriving := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	reads, derives := countBlobSteps(app, func(_ context.Context, step string) {
		if step == "derive" {
			once.Do(func() { close(deriving) })
			<-release
		}
	})
	errs := make(chan error, 2)
	call := func() {
		got, err := app.GetGitBlobImage(context.Background(), ref, r.wide, 300)
		if err == nil && !got.Derived {
			err = fmt.Errorf("not derived: %+v", got)
		}
		errs <- err
	}
	go call()
	<-deriving
	go call()
	// The second call reaches the derivation in flight, not a finished one.
	time.Sleep(50 * time.Millisecond)
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if reads.Load() != 1 || derives.Load() != 1 {
		t.Fatalf("reads = %d, derivations = %d, want one each", reads.Load(), derives.Load())
	}
}

// What is no displayable image, or over the cap, is refused before any
// read, with an error that names none of its content, and nothing is
// held.
func TestGetGitBlobImageRefusesWithoutReading(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	r := newBlobImageRepo(t, true)
	ref := testWorkspaceRef(t, app, r.dir)
	reads, _ := countBlobSteps(app, nil)
	for _, tc := range []struct {
		name, oid string
		want      error
	}{
		{"text", r.text, gitops.ErrNotDisplayImage},
		{"lfs", r.lfs, gitops.ErrLFSPointer},
		{"over cap", r.overCap, gitops.ErrBlobTooLarge},
		{"missing", strings.Repeat("e", 40), gitops.ErrNoLocalBlob},
		{"commit", revParseIn(t, r.dir, "HEAD"), gitops.ErrNoLocalBlob},
	} {
		_, err := app.GetGitBlobImage(context.Background(), ref, tc.oid, 0)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "not an image\n") {
			t.Errorf("%s: the error carries content: %v", tc.name, err)
		}
	}
	if _, err := app.GetGitBlobImage(context.Background(), ref, "abc", 0); err == nil {
		t.Error("an abbreviated id was served")
	}
	if reads.Load() != 0 || app.reviewDiffImages().Held() != 0 {
		t.Fatalf("refusals read %d blobs and hold %d bytes", reads.Load(), app.reviewDiffImages().Held())
	}
}

// A call whose context ends stops git's read and holds nothing; the next
// call reads afresh.
func TestGetGitBlobImageCancellationStopsTheRead(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	r := newBlobImageRepo(t, false)
	ref := testWorkspaceRef(t, app, r.dir)
	ctx, cancel := context.WithCancel(context.Background())
	reads, _ := countBlobSteps(app, func(_ context.Context, step string) {
		if step == "read" {
			cancel()
		}
	})
	if _, err := app.GetGitBlobImage(ctx, ref, r.wide, 0); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("a cancelled read answered %v", err)
	}
	if held := app.reviewDiffImages().Held(); held != 0 {
		t.Fatalf("a cancelled read holds %d bytes", held)
	}
	if reads.Load() != 1 {
		t.Fatalf("reads = %d", reads.Load())
	}
	again, _ := countBlobSteps(app, nil)
	if _, err := app.GetGitBlobImage(context.Background(), ref, r.wide, 0); err != nil {
		t.Fatal(err)
	}
	if again.Load() != 1 {
		t.Fatalf("the call after a cancelled one read %d times", again.Load())
	}
}

// Many distinct blobs stay within the review image store's budget, the
// least recently used evicted and read again when asked for.
func TestGetGitBlobImageStaysWithinTheStoreBudget(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	repo := testutil.InitGitRepo(t)
	const blobs = 40
	var largest int64
	for i := range blobs {
		data := sizedPNG(t, 200+i, 150)
		largest = max(largest, int64(len(data)))
		writeRepoBytes(t, repo, filepath.Join("img", fmt.Sprintf("%d.png", i)), data)
	}
	testutil.RunGit(t, repo, "add", "-A")
	testutil.RunGit(t, repo, "commit", "-qm", "many")
	budget := 4 * largest
	app.reviewDiffImageOnce.Do(func() { app.reviewDiffImageStore = reviewimage.New(budget, time.Hour, time.Second) })
	ref := testWorkspaceRef(t, app, repo)
	reads, _ := countBlobSteps(app, nil)
	oid := func(i int) string { return revParseIn(t, repo, fmt.Sprintf("HEAD:img/%d.png", i)) }
	for i := range blobs {
		if _, err := app.GetGitBlobImage(context.Background(), ref, oid(i), 0); err != nil {
			t.Fatal(err)
		}
		if held := app.reviewDiffImages().Held(); held > budget {
			t.Fatalf("after %d blobs the store holds %d bytes, budget %d", i+1, held, budget)
		}
	}
	if _, err := app.GetGitBlobImage(context.Background(), ref, oid(0), 0); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != blobs+1 {
		t.Fatalf("reads = %d, want each blob once and the evicted first again", reads.Load())
	}
}

// SaveGitBlobImage writes the blob's bytes into Downloads under the name
// given, never over a file already there and never outside the directory,
// and refuses what GetGitBlobImage refuses without writing anything.
func TestSaveGitBlobImageWritesTheBlobAndRefusesWhatIsNotShown(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	app.downloadsIsolated = true
	r := newBlobImageRepo(t, true)
	ref := testWorkspaceRef(t, app, r.dir)
	downloads := filepath.Join(app.configDir, "downloads")

	first, err := app.SaveGitBlobImage(context.Background(), ref, r.wide, "wide.png")
	if err != nil {
		t.Fatal(err)
	}
	if first != filepath.Join(downloads, "wide.png") {
		t.Fatalf("saved to %q, want %q", first, filepath.Join(downloads, "wide.png"))
	}
	if saved, err := os.ReadFile(first); err != nil || !bytes.Equal(saved, r.wideBytes) {
		t.Fatalf("saved file = %v (%d bytes), want the blob's %d bytes", err, len(saved), len(r.wideBytes))
	}
	second, err := app.SaveGitBlobImage(context.Background(), ref, r.wide, "wide.png")
	if err != nil || second != filepath.Join(downloads, "wide (2).png") {
		t.Fatalf("second save = %q, %v; want the collision name beside the first", second, err)
	}
	// A name with separators lands as its last segment, inside Downloads.
	for _, name := range []string{"../../escape.png", `..\..\escape.png`, "docs/nested/a.png"} {
		path, err := app.SaveGitBlobImage(context.Background(), ref, r.wide, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if filepath.Dir(path) != downloads {
			t.Fatalf("%s saved to %q, outside %q", name, path, downloads)
		}
	}
	// A name without an extension takes the one the bytes call for.
	if path, err := app.SaveGitBlobImage(context.Background(), ref, r.wide, "picture"); err != nil || filepath.Base(path) != "picture.png" {
		t.Fatalf("extensionless save = %q, %v", path, err)
	}

	entries, _ := os.ReadDir(downloads)
	before := len(entries)
	for _, tc := range []struct {
		name, oid string
		want      error
	}{
		{"text", r.text, gitops.ErrNotDisplayImage},
		{"lfs", r.lfs, gitops.ErrLFSPointer},
		{"over cap", r.overCap, gitops.ErrBlobTooLarge},
		{"missing", strings.Repeat("e", 40), gitops.ErrNoLocalBlob},
	} {
		if _, err := app.SaveGitBlobImage(context.Background(), ref, tc.oid, "x.png"); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, err := app.SaveGitBlobImage(context.Background(), ref, "abc", "x.png"); err == nil {
		t.Error("an abbreviated id was saved")
	}
	if entries, _ := os.ReadDir(downloads); len(entries) != before {
		t.Fatalf("refusals wrote %d files", len(entries)-before)
	}
}
