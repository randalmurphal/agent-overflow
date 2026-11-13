package app

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
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

const diagramSVG = `<svg xmlns="http://www.w3.org/2000/svg" width="4" height="4"><script>alert(1)</script></svg>` + "\n"

// imageHistory is a repository whose feature branch, one commit off main,
// adds, deletes, changes and renames images.
type imageHistory struct {
	repo       string
	base, head string
	images     map[string][]byte // name -> bytes, "old:" prefix for a base-side file
}

func newImageHistory(t *testing.T) imageHistory {
	t.Helper()
	repo := testutil.InitGitRepo(t)
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

var indexLine = regexp.MustCompile(`(?m)^index ([0-9a-f]+)\.\.([0-9a-f]+)`)

// fileRef names a file as the pane does: by its paths and the index line of
// its section of patch, as the patch printed it.
func fileRef(t *testing.T, patch, path, oldPath string) ReviewDiffFileRef {
	t.Helper()
	for _, section := range strings.Split(patch, "\ndiff --git ") {
		header, _, _ := strings.Cut(section, "\n")
		if !strings.HasSuffix(header, " b/"+path) {
			continue
		}
		ref := ReviewDiffFileRef{Path: path, OldPath: oldPath}
		if match := indexLine.FindStringSubmatch(section); match != nil {
			ref.OldOID, ref.NewOID = match[1], match[2]
		}
		return ref
	}
	t.Fatalf("the patch has no section for %s:\n%s", path, patch)
	return ReviewDiffFileRef{}
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
	if !got.Exists || got.Size != int64(len(want)) || got.MimeType != mime || got.OverCap || got.NotImage {
		t.Fatalf("%s = %+v, want %d bytes of %s", label, got, len(want), mime)
	}
	resp, body := fetchImage(t, base, got)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != mime || !bytes.Equal(body, want) {
		t.Fatalf("%s served %d %q with %d bytes, want %d bytes", label, resp.StatusCode, resp.Header.Get("Content-Type"), len(body), len(want))
	}
}

// Every side of an added, deleted, changed and renamed image is served at
// the revision the diff read it at, in the commit, pr and range scopes.
func TestGetReviewDiffImageServesBothRevisions(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	h := newImageHistory(t)
	ref := testWorkspaceRef(t, app, h.repo)
	ctx, _ := reviewDiffConn(t)
	rev := &gitops.PRRevision{BaseRef: "main", BaseSHA: h.base, HeadSHA: h.head}
	opened, err := app.OpenCommitDiff(ctx, ref, h.head, false)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := readOpenedReviewDiff(t, app, ctx, opened)
	if err != nil {
		t.Fatal(err)
	}
	png := "image/png"
	for _, source := range map[string]ReviewDiffSource{
		"commit": {Scope: "commit", CommitSHA: h.head},
		"pr":     {Scope: "pr", Revision: rev},
		"range":  {Scope: "pr", Revision: rev, FirstSHA: h.head, LastSHA: h.head},
	} {
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
			got, err := app.GetReviewDiffImage(ctx, ref, source, fileRef(t, patch, tc.path, tc.oldPath))
			if err != nil {
				t.Fatalf("%s %s: %v", source.Scope, tc.path, err)
			}
			if got.MaxBytes != reviewDiffImageMaxBytes {
				t.Fatalf("MaxBytes = %d", got.MaxBytes)
			}
			expectSide(t, base, source.Scope+" "+tc.path+" old", got.Old, tc.old, tc.mime)
			expectSide(t, base, source.Scope+" "+tc.path+" new", got.New, tc.new, tc.mime)
		}
	}
	got, err := app.GetReviewDiffImage(ctx, ref, ReviewDiffSource{Scope: "commit", CommitSHA: h.head}, fileRef(t, patch, "fake.png", ""))
	if err != nil || !got.New.NotImage || got.New.URL != "" || got.New.Size != int64(len(h.images["fake.png"])) {
		t.Fatalf("a .png that is not an image = %+v (err %v), want its size and no URL", got.New, err)
	}
}

// The patch names a binary file by its index line; the sides of a pure
// rename, which has none, must hold the same blob.
func TestBinaryFilesCarryTheirIndexLine(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	h := newImageHistory(t)
	ref := testWorkspaceRef(t, app, h.repo)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenCommitDiff(ctx, ref, h.head, false)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := readOpenedReviewDiff(t, app, ctx, opened)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"added.png", "deleted.png", "changed.png", "fake.png"} {
		if ref := fileRef(t, patch, name, ""); ref.OldOID == "" || ref.NewOID == "" {
			t.Errorf("%s has no index line:\n%s", name, patch)
		}
	}
	if ref := fileRef(t, patch, "moved/to.png", "moved.png"); ref.OldOID != "" || ref.NewOID != "" {
		t.Fatalf("a pure rename printed an index line %+v", ref)
	}
}

// The worktree scopes read the snapshot: an untracked image is served, and
// a file that moved on since the patch was read is refused, never served as
// the diff's.
func TestGetReviewDiffImageReadsTheWorktreeScopes(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	h := newImageHistory(t)
	ref := testWorkspaceRef(t, app, h.repo)
	ctx, _ := reviewDiffConn(t)
	fresh := sizedPNG(t, 13, 12)
	writeRepoBytes(t, h.repo, "fresh.png", fresh)
	for _, source := range []ReviewDiffSource{{Scope: "workspace"}, {Scope: "branch", BaseBranch: "main"}} {
		var opened ReviewDiffOpened
		var err error
		if source.Scope == "workspace" {
			opened, err = app.OpenWorkspaceDiff(ctx, ref, false)
		} else {
			opened, err = app.OpenBranchBaseDiff(ctx, ref, "main", false)
		}
		if err != nil {
			t.Fatal(err)
		}
		patch, err := readOpenedReviewDiff(t, app, ctx, opened)
		if err != nil {
			t.Fatal(err)
		}
		file := fileRef(t, patch, "fresh.png", "")
		got, err := app.GetReviewDiffImage(ctx, ref, source, file)
		if err != nil {
			t.Fatalf("%s: %v", source.Scope, err)
		}
		expectSide(t, base, source.Scope+" old", got.Old, nil, "")
		expectSide(t, base, source.Scope+" new", got.New, fresh, "image/png")
		if source.Scope == "branch" {
			changed := fileRef(t, patch, "changed.png", "")
			got, err := app.GetReviewDiffImage(ctx, ref, source, changed)
			if err != nil {
				t.Fatalf("branch changed.png: %v", err)
			}
			expectSide(t, base, "branch changed old", got.Old, h.images["old:changed.png"], "image/png")
			expectSide(t, base, "branch changed new", got.New, h.images["changed.png"], "image/png")
		}
	}
	writeRepoBytes(t, h.repo, "fresh.png", sizedPNG(t, 2, 2))
	opened, err := app.OpenWorkspaceDiff(ctx, ref, false)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := readOpenedReviewDiff(t, app, ctx, opened)
	if err != nil {
		t.Fatal(err)
	}
	stale := fileRef(t, patch, "fresh.png", "")
	writeRepoBytes(t, h.repo, "fresh.png", sizedPNG(t, 3, 3))
	if _, err := app.GetReviewDiffImage(ctx, ref, ReviewDiffSource{Scope: "workspace"}, stale); err == nil || !strings.Contains(err.Error(), "changed since the diff was read") {
		t.Fatalf("a file rewritten since the patch = %v, want the changed refusal", err)
	}
}

func TestGetReviewDiffImageRefusesWhatTheDiffDoesNotReport(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	h := newImageHistory(t)
	ref := testWorkspaceRef(t, app, h.repo)
	ctx, _ := reviewDiffConn(t)
	commit := ReviewDiffSource{Scope: "commit", CommitSHA: h.head}
	opened, err := app.OpenCommitDiff(ctx, ref, h.head, false)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := readOpenedReviewDiff(t, app, ctx, opened)
	if err != nil {
		t.Fatal(err)
	}
	changed := fileRef(t, patch, "changed.png", "")
	rev := &gitops.PRRevision{BaseRef: "main", BaseSHA: h.base, HeadSHA: h.head}
	for _, tc := range []struct {
		name   string
		source ReviewDiffSource
		file   ReviewDiffFileRef
		want   string
	}{
		{"a file the diff leaves alone", commit, ReviewDiffFileRef{Path: "README.txt", OldOID: "0000000", NewOID: "1234567"}, "not a file this diff changes"},
		{"a path outside the repository", commit, ReviewDiffFileRef{Path: "../../etc/passwd", OldOID: "0000000", NewOID: "1234567"}, "outside repository"},
		{"another blob than the patch named", commit, ReviewDiffFileRef{Path: "changed.png", OldOID: changed.OldOID, NewOID: "abcdef0"}, "changed since the diff was read"},
		{"a side the patch says is absent", commit, ReviewDiffFileRef{Path: "changed.png", OldOID: "0000000", NewOID: changed.NewOID}, "changed since the diff was read"},
		{"no index line for a changed file", commit, ReviewDiffFileRef{Path: "changed.png"}, "changed since the diff was read"},
		{"an index line that is not one", commit, ReviewDiffFileRef{Path: "changed.png", OldOID: "zz", NewOID: changed.NewOID}, "not an abbreviated object ID"},
		{"a rename source of another file", commit, ReviewDiffFileRef{Path: "added.png", OldPath: "README.txt", OldOID: "1234567", NewOID: "1234567"}, "not a file this diff changes"},
		{"an unknown scope", ReviewDiffSource{Scope: "edits"}, changed, "not a git review scope"},
		{"a pr scope without its revision", ReviewDiffSource{Scope: "pr"}, changed, "revision is required"},
		{"half a range", ReviewDiffSource{Scope: "pr", Revision: rev, FirstSHA: h.head}, changed, "needs both ends"},
		{"a range outside the PR", ReviewDiffSource{Scope: "pr", Revision: rev, FirstSHA: h.base, LastSHA: h.head}, changed, "not one of the pull request's commits"},
		{"a branch scope without its base", ReviewDiffSource{Scope: "branch"}, changed, "base branch is required"},
	} {
		if _, err := app.GetReviewDiffImage(ctx, ref, tc.source, tc.file); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// Over the cap a side answers its size and nothing is read or served.
func TestGetReviewDiffImageAnswersTheSizeOverTheCap(t *testing.T) {
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
	if err != nil {
		t.Fatal(err)
	}
	patch, err := readOpenedReviewDiff(t, app, ctx, opened)
	if err != nil {
		t.Fatal(err)
	}
	got, err := app.GetReviewDiffImage(ctx, ref, ReviewDiffSource{Scope: "commit", CommitSHA: head}, fileRef(t, patch, "big.png", ""))
	if err != nil {
		t.Fatal(err)
	}
	if want := (ReviewDiffImageSide{Exists: true, Size: int64(len(big)), OverCap: true}); got.New != want {
		t.Fatalf("over the cap = %+v, want %+v", got.New, want)
	}
	if held := app.reviewDiffImages().cache.Bytes(); held != 0 {
		t.Fatalf("the cache holds %d bytes after an over-cap answer", held)
	}
}

// An SVG is served as an image that can run nothing at the app's origin.
func TestReviewDiffSVGIsServedSandboxed(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	h := newImageHistory(t)
	ref := testWorkspaceRef(t, app, h.repo)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenCommitDiff(ctx, ref, h.head, false)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := readOpenedReviewDiff(t, app, ctx, opened)
	if err != nil {
		t.Fatal(err)
	}
	got, err := app.GetReviewDiffImage(ctx, ref, ReviewDiffSource{Scope: "commit", CommitSHA: h.head}, fileRef(t, patch, "diagram.svg", ""))
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

func TestGetReviewDiffFileSizesAnswersEachFile(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	h := newImageHistory(t)
	ref := testWorkspaceRef(t, app, h.repo)
	ctx, _ := reviewDiffConn(t)
	rev := &gitops.PRRevision{BaseRef: "main", BaseSHA: h.base, HeadSHA: h.head}
	opened, err := app.OpenPRDiff(ctx, ref, prRef(), *rev, false, false)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := readOpenedReviewDiff(t, app, ctx, opened)
	if err != nil {
		t.Fatal(err)
	}
	got, err := app.GetReviewDiffFileSizes(ctx, ref, ReviewDiffSource{Scope: "pr", Revision: rev}, []ReviewDiffFileRef{
		fileRef(t, patch, "changed.png", ""),
		fileRef(t, patch, "fake.png", ""),
		{Path: "README.txt", OldOID: "0000000", NewOID: "1234567"},
		fileRef(t, patch, "deleted.png", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	size := func(name string) int64 { return int64(len(h.images[name])) }
	want := []ReviewDiffFileSizes{
		{Path: "changed.png", Old: ReviewDiffFileSide{true, size("old:changed.png")}, New: ReviewDiffFileSide{true, size("changed.png")}},
		{Path: "fake.png", New: ReviewDiffFileSide{true, size("fake.png")}},
		{Path: "README.txt", Error: "README.txt is not a file this diff changes"},
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
	if _, err := app.GetReviewDiffFileSizes(ctx, ref, ReviewDiffSource{Scope: "pr", Revision: rev}, make([]ReviewDiffFileRef, maxReviewDiffFileRefs+1)); err == nil {
		t.Fatal("an over-long batch was accepted")
	}
}

// The edits scope serves the new side its context expansion would, verified
// against the file's patch; an edit records no old side.
func TestGetEditDiffImageServesTheVerifiedNewSide(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	workspace := t.TempDir()
	thread := testThread("thread-edit-diff-image")
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	writeRepoBytes(t, workspace, "diagram.svg", []byte(diagramSVG))
	verify := "diff --git a/diagram.svg b/diagram.svg\n--- /dev/null\n+++ b/diagram.svg\n@@ -0,0 +1 @@\n+" + strings.TrimSuffix(diagramSVG, "\n") + "\n"
	req := DiffContextRequest{Scope: "edits", Path: "diagram.svg", VerifyPatch: verify}
	got, err := app.GetEditDiffImage(thread.ID, req)
	if err != nil {
		t.Fatalf("GetEditDiffImage: %v", err)
	}
	expectSide(t, base, "edit old", got.Old, nil, "")
	expectSide(t, base, "edit new", got.New, []byte(diagramSVG), "image/svg+xml")

	writeRepoBytes(t, workspace, "diagram.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`+"\n"))
	if _, err := app.GetEditDiffImage(thread.ID, req); err == nil {
		t.Fatal("a file that drifted from the edit's patch was served")
	}
	if _, err := app.GetEditDiffImage(thread.ID, DiffContextRequest{Scope: "pr", Path: "diagram.svg"}); err == nil {
		t.Fatal("a non-edits scope was accepted")
	}
}

// The harness reset forgets every image a spec read.
func TestDropReviewDiffImagesForgetsServedImages(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	h := newImageHistory(t)
	ref := testWorkspaceRef(t, app, h.repo)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenCommitDiff(ctx, ref, h.head, false)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := readOpenedReviewDiff(t, app, ctx, opened)
	if err != nil {
		t.Fatal(err)
	}
	got, err := app.GetReviewDiffImage(ctx, ref, ReviewDiffSource{Scope: "commit", CommitSHA: h.head}, fileRef(t, patch, "added.png", ""))
	if err != nil {
		t.Fatal(err)
	}
	(&harnessHost{app: app}).DropReviewDiffImages()
	if resp, _ := fetchImage(t, base, got.New); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an image read before the reset answered %d, want 404", resp.StatusCode)
	}
}
