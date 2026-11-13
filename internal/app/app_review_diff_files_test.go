package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
)

// reviewDiffFilesApp is a store-backed app serving the byte routes.
func reviewDiffFilesApp(t *testing.T) (*App, string) {
	t.Helper()
	app := newReviewDiffTestApp(t)
	srv, err := transport.New(transport.Config{
		Dispatcher:         transport.NewDispatcher(),
		EventBus:           transport.NewEventBus(8),
		Token:              "review-diff-files-test-token",
		AttachmentTransfer: AttachmentTransfer(app),
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("transport.Server.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	app.SetTransportServer(srv)
	return app, "http://" + srv.Addr()
}

func writeRepoBytes(t *testing.T, repo, name string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, name), data, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// diagramSVG spans lines, so an edit to one line leaves others outside any
// hunk.
const diagramSVG = "<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"4\" height=\"4\">\n<script>alert(1)</script>\n<rect width=\"4\" height=\"4\"/>\n</svg>\n"

// imageHistory is a repository, with core.abbrev=4 so nothing may lean on
// an abbreviation, whose feature branch, one commit off main, adds,
// deletes, changes and renames images.
type imageHistory struct {
	repo       string
	base, head string
	images     map[string][]byte // name -> bytes, "old:" prefix for a base-side file
}

func newImageHistory(t *testing.T) imageHistory {
	t.Helper()
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "config", "core.abbrev", "4")
	h := imageHistory{repo: repo, images: map[string][]byte{
		"old:deleted.png": sizedPNG(t, 3, 2),
		"old:changed.png": sizedPNG(t, 5, 4),
		"old:moved.png":   sizedPNG(t, 7, 6),
		"added.png":       sizedPNG(t, 9, 8),
		"changed.png":     sizedPNG(t, 11, 10),
		"moved/to.png":    sizedPNG(t, 7, 6),
		"diagram.svg":     []byte(diagramSVG),
		"fake.png":        append([]byte{0, 1, 2, 3}, bytes.Repeat([]byte{0xfe}, 64)...),
	}}
	for _, name := range []string{"deleted.png", "changed.png", "moved.png"} {
		writeRepoBytes(t, repo, name, h.images["old:"+name])
	}
	testutil.RunGit(t, repo, "add", "-A")
	testutil.RunGit(t, repo, "commit", "-qm", "base images")
	h.base = revParseIn(t, repo, "HEAD")
	testutil.RunGit(t, repo, "checkout", "-qb", "feature")
	testutil.RunGit(t, repo, "rm", "-q", "deleted.png")
	if err := os.MkdirAll(filepath.Join(repo, "moved"), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, repo, "mv", "moved.png", "moved/to.png")
	for _, name := range []string{"added.png", "changed.png", "diagram.svg", "fake.png"} {
		writeRepoBytes(t, repo, name, h.images[name])
	}
	testutil.RunGit(t, repo, "add", "-A")
	testutil.RunGit(t, repo, "commit", "-qm", "change images")
	h.head = revParseIn(t, repo, "HEAD")
	return h
}

// filesHandle answers the handle an open kept its pin under.
func filesHandle(t *testing.T, opened ReviewDiffOpened, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if opened.FilesHandle == "" {
		t.Fatal("a git-backed open answered no files handle")
	}
	return opened.FilesHandle
}

func fetchImage(t *testing.T, base string, side ReviewDiffImageSide) (*http.Response, []byte) {
	t.Helper()
	if side.URL == "" {
		t.Fatalf("side %+v has no URL", side)
	}
	return getBytes(t, base, side.URL)
}

// expectSide checks one side's answer against the bytes it must serve, nil
// for a side that does not exist.
func expectSide(t *testing.T, base, label string, got ReviewDiffImageSide, want []byte, mime string) {
	t.Helper()
	if want == nil {
		if got != (ReviewDiffImageSide{}) {
			t.Fatalf("%s = %+v, want a side that does not exist", label, got)
		}
		return
	}
	if !got.Exists || got.Size != int64(len(want)) || got.MimeType != mime || got.OverCap || got.NotImage || got.Unavailable != "" {
		t.Fatalf("%s = %+v, want %d bytes of %s", label, got, len(want), mime)
	}
	resp, body := fetchImage(t, base, got)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != mime || !bytes.Equal(body, want) {
		t.Fatalf("%s served %d %q with %d bytes, want %d bytes", label, resp.StatusCode, resp.Header.Get("Content-Type"), len(body), len(want))
	}
}

// Every side of an added, deleted, changed and renamed image is served at
// the sides the diff was opened at, in the commit, PR and PR range scopes,
// in a repository whose abbreviations are four digits.
func TestGetReviewDiffImageServesBothSidesOfEachImmutableScope(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	h := newImageHistory(t)
	ref := testWorkspaceRef(t, app, h.repo)
	ctx, _ := reviewDiffConn(t)
	rev := gitops.PRRevision{BaseRef: "main", BaseSHA: h.base, HeadSHA: h.head}
	handles := map[string]string{}
	opened, err := app.OpenCommitDiff(ctx, ref, h.head, false)
	handles["commit"] = filesHandle(t, opened, err)
	opened, err = app.OpenPRDiff(ctx, ref, prRef(), rev, false, false)
	handles["pr"] = filesHandle(t, opened, err)
	opened, err = app.OpenPRCommitDiff(ctx, ref, prRef(), rev, h.head, h.head, false, false)
	handles["range"] = filesHandle(t, opened, err)
	png := "image/png"
	for scope, handle := range handles {
		for _, tc := range []struct {
			path, oldPath string
			old, new      []byte
			mime          string
		}{
			{"added.png", "", nil, h.images["added.png"], png},
			{"deleted.png", "", h.images["old:deleted.png"], nil, png},
			{"changed.png", "", h.images["old:changed.png"], h.images["changed.png"], png},
			{"moved/to.png", "moved.png", h.images["old:moved.png"], h.images["moved/to.png"], png},
			{"diagram.svg", "", nil, []byte(diagramSVG), "image/svg+xml"},
		} {
			got, err := app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: tc.path, OldPath: tc.oldPath})
			if err != nil {
				t.Fatalf("%s %s: %v", scope, tc.path, err)
			}
			if got.MaxBytes != reviewDiffImageMaxBytes {
				t.Fatalf("MaxBytes = %d", got.MaxBytes)
			}
			expectSide(t, base, scope+" "+tc.path+" old", got.Old, tc.old, tc.mime)
			expectSide(t, base, scope+" "+tc.path+" new", got.New, tc.new, tc.mime)
		}
	}
	got, err := app.GetReviewDiffImage(ctx, ref, handles["commit"], ReviewDiffFile{Path: "fake.png"})
	if err != nil || !got.New.NotImage || got.New.URL != "" || got.New.Size != int64(len(h.images["fake.png"])) {
		t.Fatalf("a .png that is not an image = %+v (err %v), want its size and no URL", got.New, err)
	}
}

// A read answers the sides the diff was opened at, never what HEAD holds
// now: a mode-only change (no index line) serves its one blob on both
// sides after HEAD moves to another image, and a working-tree file
// rewritten after the open is refused.
func TestGetReviewDiffImageReadsTheOpenedSidesNotHEAD(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "config", "core.abbrev", "4")
	first := sizedPNG(t, 4, 4)
	writeRepoBytes(t, repo, "mode.png", first)
	testutil.RunGit(t, repo, "add", "mode.png")
	testutil.RunGit(t, repo, "commit", "-qm", "add")
	testutil.RunGit(t, repo, "update-index", "--chmod=+x", "mode.png")
	testutil.RunGit(t, repo, "commit", "-qm", "mode only")
	modeOnly := revParseIn(t, repo, "HEAD")
	ref := testWorkspaceRef(t, app, repo)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenCommitDiff(ctx, ref, modeOnly, false)
	handle := filesHandle(t, opened, err)
	patch, err := readOpenedReviewDiff(t, app, ctx, opened)
	if err != nil || strings.Contains(patch, "\nindex ") {
		t.Fatalf("a mode-only change printed an index line (err %v):\n%s", err, patch)
	}

	writeRepoBytes(t, repo, "mode.png", sizedPNG(t, 9, 9))
	testutil.RunGit(t, repo, "commit", "-qam", "another image")

	got, err := app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: "mode.png"})
	if err != nil {
		t.Fatal(err)
	}
	expectSide(t, base, "mode-only old", got.Old, first, "image/png")
	expectSide(t, base, "mode-only new", got.New, first, "image/png")

	writeRepoBytes(t, repo, "wt.png", sizedPNG(t, 5, 7))
	opened, err = app.OpenWorkspaceDiff(ctx, ref, false)
	handle = filesHandle(t, opened, err)
	testutil.RunGit(t, repo, "add", "wt.png")
	testutil.RunGit(t, repo, "commit", "-qm", "wt")
	writeRepoBytes(t, repo, "wt.png", sizedPNG(t, 3, 3))
	if _, err := app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: "wt.png"}); err == nil || !strings.Contains(err.Error(), "changed since the diff was read") {
		t.Fatalf("a working-tree file rewritten after HEAD moved = %v, want the changed refusal", err)
	}
}

// The working-tree scopes read what the open recorded: an untracked image
// is served, a file rewritten since is refused unless the blob the record
// binds to that path is already held, and the branch scope's old side is
// the merge base.
func TestGetReviewDiffImageReadsTheWorktreeScopes(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	h := newImageHistory(t)
	ref := testWorkspaceRef(t, app, h.repo)
	ctx, _ := reviewDiffConn(t)
	fresh := sizedPNG(t, 13, 12)
	writeRepoBytes(t, h.repo, "fresh.png", fresh)
	opens := map[string]func() (ReviewDiffOpened, error){
		"workspace": func() (ReviewDiffOpened, error) { return app.OpenWorkspaceDiff(ctx, ref, false) },
		"branch":    func() (ReviewDiffOpened, error) { return app.OpenBranchBaseDiff(ctx, ref, "main", false) },
		"commit dialog": func() (ReviewDiffOpened, error) {
			return app.OpenCommitFileDiff(ctx, ref, []string{"fresh.png"})
		},
	}
	for scope, open := range opens {
		opened, err := open()
		handle := filesHandle(t, opened, err)
		got, err := app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: "fresh.png"})
		if err != nil {
			t.Fatalf("%s: %v", scope, err)
		}
		expectSide(t, base, scope+" old", got.Old, nil, "")
		expectSide(t, base, scope+" new", got.New, fresh, "image/png")
		if scope == "branch" {
			got, err := app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: "changed.png"})
			if err != nil {
				t.Fatalf("branch changed.png: %v", err)
			}
			expectSide(t, base, "branch changed old", got.Old, h.images["old:changed.png"], "image/png")
			expectSide(t, base, "branch changed new", got.New, h.images["changed.png"], "image/png")
		}
	}
	opened, err := app.OpenWorkspaceDiff(ctx, ref, false)
	handle := filesHandle(t, opened, err)
	writeRepoBytes(t, h.repo, "fresh.png", sizedPNG(t, 3, 3))
	got, err := app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: "fresh.png"})
	if err != nil {
		t.Fatalf("a held blob rewritten since: %v", err)
	}
	expectSide(t, base, "a held blob rewritten since", got.New, fresh, "image/png")

	writeRepoBytes(t, h.repo, "fresh.png", sizedPNG(t, 6, 5))
	opened, err = app.OpenWorkspaceDiff(ctx, ref, false)
	handle = filesHandle(t, opened, err)
	writeRepoBytes(t, h.repo, "fresh.png", sizedPNG(t, 3, 3))
	if _, err := app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: "fresh.png"}); err == nil || !strings.Contains(err.Error(), "changed since the diff was read") {
		t.Fatalf("a file rewritten since the open = %v, want the changed refusal", err)
	}
	if err := os.Remove(filepath.Join(h.repo, "fresh.png")); err != nil {
		t.Fatal(err)
	}
	if _, err := app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: "fresh.png"}); err == nil || !strings.Contains(err.Error(), "changed since the diff was read") {
		t.Fatalf("a file deleted since the open = %v, want the changed refusal", err)
	}
}

// A held image is reachable only through a record whose open bound its
// blob to the path read: not through a record of another workspace, not
// through a path the record does not bind, and not through another
// workspace's handle.
func TestAHeldImageIsReachableOnlyThroughTheRecordThatBindsIt(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	ctx, _ := reviewDiffConn(t)
	first, second := testutil.InitGitRepo(t), testutil.InitGitRepo(t)
	firstRef, secondRef := testWorkspaceRef(t, app, first), testWorkspaceRef(t, app, second)
	held, other := sizedPNG(t, 8, 8), sizedPNG(t, 2, 9)
	writeRepoBytes(t, first, "same.png", held)
	opened, err := app.OpenWorkspaceDiff(ctx, firstRef, false)
	firstHandle := filesHandle(t, opened, err)
	got, err := app.GetReviewDiffImage(ctx, firstRef, firstHandle, ReviewDiffFile{Path: "same.png"})
	if err != nil {
		t.Fatal(err)
	}
	expectSide(t, base, "the first workspace's image", got.New, held, "image/png")

	writeRepoBytes(t, second, "same.png", other)
	writeRepoBytes(t, second, "README.txt", held)
	opened, err = app.OpenWorkspaceDiff(ctx, secondRef, false)
	secondHandle := filesHandle(t, opened, err)
	got, err = app.GetReviewDiffImage(ctx, secondRef, secondHandle, ReviewDiffFile{Path: "same.png"})
	if err != nil {
		t.Fatal(err)
	}
	expectSide(t, base, "the second workspace's image", got.New, other, "image/png")
	// The second open bound README.txt to the held image's blob, so the
	// held entry is that blob's bytes.
	if got, err := app.GetReviewDiffImage(ctx, secondRef, secondHandle, ReviewDiffFile{Path: "README.txt"}); err != nil {
		t.Fatal(err)
	} else {
		expectSide(t, base, "README.txt", got.New, held, "image/png")
	}
	for name, read := range map[string]func() error{
		"the first handle in the second workspace": func() error {
			_, err := app.GetReviewDiffImage(ctx, secondRef, firstHandle, ReviewDiffFile{Path: "same.png"})
			return err
		},
		"a path the record does not bind": func() error {
			_, err := app.GetReviewDiffImage(ctx, firstRef, firstHandle, ReviewDiffFile{Path: "README.txt"})
			return err
		},
	} {
		if err := read(); err == nil {
			t.Errorf("%s was served", name)
		}
	}
}

func TestGetReviewDiffImageRefusesWhatTheOpenedDiffDoesNotChange(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	h := newImageHistory(t)
	ref := testWorkspaceRef(t, app, h.repo)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenCommitDiff(ctx, ref, h.head, false)
	commit := filesHandle(t, opened, err)
	writeRepoBytes(t, h.repo, "fresh.png", sizedPNG(t, 2, 2))
	opened, err = app.OpenWorkspaceDiff(ctx, ref, false)
	workspace := filesHandle(t, opened, err)
	for _, tc := range []struct {
		name   string
		handle string
		file   ReviewDiffFile
		want   string
	}{
		{"a file the diff leaves alone", commit, ReviewDiffFile{Path: "README.txt"}, "not a file this diff changes"},
		{"a directory", commit, ReviewDiffFile{Path: "moved"}, "not a file"},
		{"a directory with a trailing slash", commit, ReviewDiffFile{Path: "moved/"}, "empty segment"},
		{"a glob", commit, ReviewDiffFile{Path: "*.png"}, "not a file this diff changes"},
		{"a path outside the repository", commit, ReviewDiffFile{Path: "../../etc/passwd"}, `".." segment`},
		{"a backslash path", commit, ReviewDiffFile{Path: `moved\to.png`}, "separators"},
		{"a rename source of another file", commit, ReviewDiffFile{Path: "added.png", OldPath: "README.txt"}, "not a file this diff changes"},
		{"a working-tree file the diff leaves alone", workspace, ReviewDiffFile{Path: "README.txt"}, "not a file this diff changes"},
		{"an unknown handle", "no-such-handle", ReviewDiffFile{Path: "fresh.png"}, "This diff's files are no longer held; reload the diff."},
	} {
		if _, err := app.GetReviewDiffImage(ctx, ref, tc.handle, tc.file); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
	if _, err := app.GetReviewDiffImage(ctx, ref, "no-such-handle", ReviewDiffFile{Path: "fresh.png"}); !errors.Is(err, errReviewDiffFilesGone) {
		t.Fatalf("an unknown handle = %v, want errReviewDiffFilesGone", err)
	}
}

// Names are literal: spaces at either end and pathspec magic are part of
// a file's name.
func TestGetReviewDiffImageReadsLiteralNames(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	repo := testutil.InitGitRepo(t)
	images := map[string][]byte{" logo.png": sizedPNG(t, 3, 3), "logo.png ": sizedPNG(t, 4, 3), ":(top)icon.png": sizedPNG(t, 5, 3)}
	for name, data := range images {
		writeRepoBytes(t, repo, name, data)
	}
	testutil.RunGit(t, repo, "add", "-A")
	testutil.RunGit(t, repo, "commit", "-qm", "names")
	ref := testWorkspaceRef(t, app, repo)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenCommitDiff(ctx, ref, revParseIn(t, repo, "HEAD"), false)
	handle := filesHandle(t, opened, err)
	for name, data := range images {
		got, err := app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: name})
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		expectSide(t, base, name, got.New, data, "image/png")
	}
}

// Over the cap a side answers its size and nothing is read or held, for a
// committed blob and a working-tree file alike.
func TestGetReviewDiffImageReadsNothingOverTheCap(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	repo := testutil.InitGitRepo(t)
	big := append(sizedPNG(t, 1, 1), make([]byte, reviewDiffImageMaxBytes)...)
	writeRepoBytes(t, repo, "big.png", big)
	testutil.RunGit(t, repo, "add", "big.png")
	testutil.RunGit(t, repo, "commit", "-qm", "big")
	head := revParseIn(t, repo, "HEAD")
	ref := testWorkspaceRef(t, app, repo)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenCommitDiff(ctx, ref, head, false)
	handle := filesHandle(t, opened, err)
	want := ReviewDiffImageSide{Exists: true, Size: int64(len(big)), OverCap: true}
	got, err := app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: "big.png"})
	if err != nil || got.New != want {
		t.Fatalf("a committed side over the cap = %+v (err %v), want %+v", got.New, err, want)
	}
	writeRepoBytes(t, repo, "wt-big.png", big)
	opened, err = app.OpenWorkspaceDiff(ctx, ref, false)
	handle = filesHandle(t, opened, err)
	got, err = app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: "wt-big.png"})
	if err != nil || got.New != want {
		t.Fatalf("a working-tree side over the cap = %+v (err %v), want %+v", got.New, err, want)
	}
	if held := app.reviewDiffImages().Held(); held != 0 {
		t.Fatalf("the image store holds %d bytes after over-cap answers", held)
	}
}

// A committed Git LFS pointer and a working-tree file under a filter are
// unavailable with their reason; a CRLF file under core.autocrlf is
// served as the bytes on disk.
func TestFilteredAndLFSFilesAreUnavailable(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	repo := testutil.InitGitRepo(t)
	pointer := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize 4096\n"
	writeRepoBytes(t, repo, ".gitattributes", []byte("*.png filter=lfs diff=lfs merge=lfs -text\n"))
	writeRepoBytes(t, repo, "stored.png", []byte(pointer))
	testutil.RunGit(t, repo, "add", "-A")
	testutil.RunGit(t, repo, "commit", "-qm", "lfs")
	testutil.RunGit(t, repo, "config", "core.autocrlf", "true")
	ref := testWorkspaceRef(t, app, repo)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenCommitDiff(ctx, ref, revParseIn(t, repo, "HEAD"), false)
	got, err := app.GetReviewDiffImage(ctx, ref, filesHandle(t, opened, err), ReviewDiffFile{Path: "stored.png"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (ReviewDiffImageSide{Exists: true, Size: int64(len(pointer)), Unavailable: "Stored with Git LFS; the image is not in the repository."}); got.New != want {
		t.Fatalf("a committed LFS pointer = %+v, want %+v", got.New, want)
	}

	smudged := sizedPNG(t, 6, 6)
	writeRepoBytes(t, repo, "drawn.png", smudged)
	crlf := []byte("<svg xmlns=\"http://www.w3.org/2000/svg\">\r\n<rect/>\r\n</svg>\r\n")
	writeRepoBytes(t, repo, "crlf.svg", crlf)
	opened, err = app.OpenWorkspaceDiff(ctx, ref, false)
	handle := filesHandle(t, opened, err)
	got, err = app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: "drawn.png"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (ReviewDiffImageSide{Exists: true, Size: int64(len(smudged)), Unavailable: "This file passes through a git filter, so its image is not shown."}); got.New != want {
		t.Fatalf("a filtered working-tree file = %+v, want %+v", got.New, want)
	}
	got, err = app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: "crlf.svg"})
	if err != nil {
		t.Fatalf("a CRLF file under core.autocrlf: %v", err)
	}
	expectSide(t, base, "crlf.svg", got.New, crlf, "image/svg+xml")
}

// An SVG is served as an image that can run nothing at the app's origin.
func TestReviewDiffSVGIsServedSandboxed(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	h := newImageHistory(t)
	ref := testWorkspaceRef(t, app, h.repo)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenCommitDiff(ctx, ref, h.head, false)
	got, err := app.GetReviewDiffImage(ctx, ref, filesHandle(t, opened, err), ReviewDiffFile{Path: "diagram.svg"})
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := fetchImage(t, base, got.New)
	for header, want := range map[string]string{
		"Content-Type":            "image/svg+xml",
		"Content-Disposition":     "attachment",
		"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; sandbox",
		"X-Content-Type-Options":  "nosniff",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// A sizes batch answers each file on its own: a file that cannot be read,
// committed or working-tree, does not fail the others.
func TestGetReviewDiffFileSizesAnswersEachFile(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	h := newImageHistory(t)
	ref := testWorkspaceRef(t, app, h.repo)
	ctx, _ := reviewDiffConn(t)
	rev := gitops.PRRevision{BaseRef: "main", BaseSHA: h.base, HeadSHA: h.head}
	opened, err := app.OpenPRDiff(ctx, ref, prRef(), rev, false, false)
	handle := filesHandle(t, opened, err)
	size := func(name string) int64 { return int64(len(h.images[name])) }
	got, err := app.GetReviewDiffFileSizes(ctx, ref, handle, []ReviewDiffFile{
		{Path: "changed.png"}, {Path: "fake.png"}, {Path: "README.txt"}, {Path: "moved"},
		{Path: "a/../b"}, {Path: "moved/to.png", OldPath: "moved.png"}, {Path: "deleted.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []ReviewDiffFileSizes{
		{Path: "changed.png", Old: ReviewDiffFileSide{true, size("old:changed.png")}, New: ReviewDiffFileSide{true, size("changed.png")}},
		{Path: "fake.png", New: ReviewDiffFileSide{true, size("fake.png")}},
		{Path: "README.txt", Error: "README.txt is not a file this diff changes"},
		{Path: "moved", Error: "moved is not a file"},
		{Path: "a/../b", Error: `gitdiff: invalid path "a/../b": ".." segment`},
		{Path: "moved/to.png", OldPath: "moved.png", Old: ReviewDiffFileSide{true, size("old:moved.png")}, New: ReviewDiffFileSide{true, size("moved/to.png")}},
		{Path: "deleted.png", Old: ReviewDiffFileSide{true, size("old:deleted.png")}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("file %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if _, err := app.GetReviewDiffFileSizes(ctx, ref, handle, make([]ReviewDiffFile, maxReviewDiffFiles+1)); err == nil {
		t.Fatal("an over-long batch was accepted")
	}

	fresh := sizedPNG(t, 7, 7)
	writeRepoBytes(t, h.repo, "fresh.png", fresh)
	if err := os.Remove(filepath.Join(h.repo, "added.png")); err != nil {
		t.Fatal(err)
	}
	opened, err = app.OpenWorkspaceDiff(ctx, ref, false)
	handle = filesHandle(t, opened, err)
	// The working tree moves on after the open; the sizes are the open's.
	if err := os.Remove(filepath.Join(h.repo, "fresh.png")); err != nil {
		t.Fatal(err)
	}
	got, err = app.GetReviewDiffFileSizes(ctx, ref, handle, []ReviewDiffFile{
		{Path: "fresh.png"}, {Path: "README.txt"}, {Path: "added.png"}, {Path: "dir//x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want = []ReviewDiffFileSizes{
		{Path: "fresh.png", New: ReviewDiffFileSide{true, int64(len(fresh))}},
		{Path: "README.txt", Error: "README.txt is not a file this diff changes"},
		{Path: "added.png", Old: ReviewDiffFileSide{true, size("added.png")}},
		{Path: "dir//x", Error: `gitdiff: invalid path "dir//x": empty segment`},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("working-tree file %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The harness reset forgets every image a spec read and every record its
// opens kept.
func TestDropReviewDiffImagesForgetsImagesAndRecords(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	h := newImageHistory(t)
	ref := testWorkspaceRef(t, app, h.repo)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenCommitDiff(ctx, ref, h.head, false)
	handle := filesHandle(t, opened, err)
	got, err := app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: "added.png"})
	if err != nil {
		t.Fatal(err)
	}
	(&harnessHost{app: app}).DropReviewDiffImages()
	if resp, _ := fetchImage(t, base, got.New); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an image read before the reset answered %d, want 404", resp.StatusCode)
	}
	if held := app.reviewDiffImages().Held(); held != 0 {
		t.Fatalf("the store holds %d bytes after the reset", held)
	}
	if _, err := app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: "added.png"}); !errors.Is(err, errReviewDiffFilesGone) {
		t.Fatalf("a handle from before the reset = %v, want errReviewDiffFilesGone", err)
	}
}

// editThread is a thread whose turn 0 holds the given edits, in order,
// each a payload editing diagram.svg.
func editThread(t *testing.T, app *App, id string, payloads ...string) string {
	t.Helper()
	thread := testThread(id)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	patch := "diff --git a/diagram.svg b/diagram.svg\n--- a/diagram.svg\n+++ b/diagram.svg\n@@ -2 +2 @@\n-<script>alert(0)</script>\n+<script>alert(1)</script>\n"
	for _, payload := range payloads {
		insertDiffSpanPayload(t, app, thread.ID, "item-"+payload, payload, "tool_result", patch)
	}
	return thread.ID
}

// An edit's image is the content the edit recorded, never the file on
// disk: without a snapshot the side is unavailable with a sentence, even
// when the disk holds a file a patch would match outside its hunks.
func TestGetEditDiffImageServesOnlyRecordedContent(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	ctx, _ := reviewDiffConn(t)
	threadID := editThread(t, app, "thread-edit-diff-image", "payload-recorded", "payload-unrecorded")
	if err := app.store.PutEditFileSnapshot(threadID, "payload-recorded", "diagram.svg", diagramSVG, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	thread, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	writeRepoBytes(t, thread.WorkspacePath, "diagram.svg", []byte(strings.Replace(diagramSVG, `width="4" height="4"/>`, `width="1" height="1"/>`, 1)))

	got, err := app.GetEditDiffImage(ctx, threadID, EditDiffImageRequest{Path: "diagram.svg", EditPayloadID: "payload-recorded"})
	if err != nil {
		t.Fatalf("GetEditDiffImage: %v", err)
	}
	if got.Old != (ReviewDiffImageSide{Unavailable: "An edit does not record the file before it."}) {
		t.Fatalf("old = %+v", got.Old)
	}
	expectSide(t, base, "recorded new", got.New, []byte(diagramSVG), "image/svg+xml")

	got, err = app.GetEditDiffImage(ctx, threadID, EditDiffImageRequest{Path: "diagram.svg", EditPayloadID: "payload-unrecorded"})
	if err != nil {
		t.Fatalf("GetEditDiffImage: %v", err)
	}
	if got.New != (ReviewDiffImageSide{Unavailable: "This edit's file content was not recorded."}) {
		t.Fatalf("an edit with no recorded content = %+v, want unavailable", got.New)
	}
	got, err = app.GetEditDiffImage(ctx, threadID, EditDiffImageRequest{Path: "other.svg", EditTurnIndex: 0})
	if err != nil || got.New != (ReviewDiffImageSide{Unavailable: "This turn does not edit this file."}) {
		t.Fatalf("a path the turn does not edit = %+v (err %v)", got.New, err)
	}
	for name, req := range map[string]EditDiffImageRequest{
		"a path outside the workspace": {Path: "../x.svg", EditPayloadID: "payload-recorded"},
		"a directory path":             {Path: "dir/", EditPayloadID: "payload-recorded"},
	} {
		if _, err := app.GetEditDiffImage(ctx, threadID, req); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := app.GetEditDiffImage(ctx, "no-such-thread", EditDiffImageRequest{Path: "diagram.svg"}); err == nil {
		t.Fatal("an unknown thread was accepted")
	}
}

// A whole turn's file is its last edit's: an earlier recorded edit never
// stands in for a later unrecorded one, for the image and for context
// lines alike.
func TestATurnsFileIsItsLastEdit(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	ctx, _ := reviewDiffConn(t)
	threadID := editThread(t, app, "thread-superseded-edit", "payload-e1", "payload-e2")
	if err := app.store.PutEditFileSnapshot(threadID, "payload-e1", "diagram.svg", diagramSVG, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	got, err := app.GetEditDiffImage(ctx, threadID, EditDiffImageRequest{Path: "diagram.svg", EditTurnIndex: 0})
	if err != nil {
		t.Fatal(err)
	}
	if got.New != (ReviewDiffImageSide{Unavailable: "This edit's file content was not recorded."}) {
		t.Fatalf("the turn's image after an unrecorded later edit = %+v, want unavailable", got.New)
	}
	verify := "diff --git a/diagram.svg b/diagram.svg\n--- a/diagram.svg\n+++ b/diagram.svg\n@@ -2 +2 @@\n-<script>alert(0)</script>\n+<script>alert(1)</script>\n"
	if _, err := app.GetEditDiffContextLines(threadID, DiffContextRequest{
		Scope: "edits", Path: "diagram.svg", StartLine: 1, EndLine: 1, VerifyPatch: verify, EditTurnIndex: 0,
	}); err == nil {
		t.Fatal("the turn's context lines came from an earlier edit's snapshot")
	}

	if err := app.store.PutEditFileSnapshot(threadID, "payload-e2", "diagram.svg", diagramSVG, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	got, err = app.GetEditDiffImage(ctx, threadID, EditDiffImageRequest{Path: "diagram.svg", EditTurnIndex: 0})
	if err != nil {
		t.Fatal(err)
	}
	expectSide(t, base, "the turn's last edit", got.New, []byte(diagramSVG), "image/svg+xml")
}

// An edit image reserves the snapshot capture limit, then decompresses
// straight into the store's buffer: the bytes allocated outside it stay a
// small fraction of the content. Serial: it measures the process's
// allocations.
func TestGetEditDiffImageAllocatesItsContentOnlyInTheStore(t *testing.T) {
	app, _ := reviewDiffFilesApp(t)
	ctx, _ := reviewDiffConn(t)
	threadID := editThread(t, app, "thread-edit-image-allocations", "payload-big")
	content := "<svg xmlns=\"http://www.w3.org/2000/svg\">\n" + strings.Repeat("<g/>\n", (3<<20)/5) + "</svg>\n"
	if len(content) > store.MaxEditFileSnapshotBytes {
		t.Fatal("the content is past the capture limit")
	}
	if err := app.store.PutEditFileSnapshot(threadID, "payload-big", "diagram.svg", content, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	app.reviewDiffImages() // created outside the measure
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, err := app.GetEditDiffImage(ctx, threadID, EditDiffImageRequest{Path: "diagram.svg", EditPayloadID: "payload-big"})
	runtime.ReadMemStats(&after)
	if err != nil || got.New.Size != int64(len(content)) || got.New.URL == "" {
		t.Fatalf("the edit image = %+v, err %v", got.New, err)
	}
	outside := int64(after.TotalAlloc-before.TotalAlloc) - app.reviewDiffImages().Held()
	if outside > 1<<20 {
		t.Fatalf("%d bytes were allocated outside the store for %d bytes of content", outside, len(content))
	}
}
