package app

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitdiff"
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

// opened answers an open's pinned sides, failing the test when it has none.
func openedSides(t *testing.T, opened ReviewDiffOpened, err error) ReviewDiffSides {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if opened.Sides == nil || opened.SidesError != "" {
		t.Fatalf("the open pinned no sides: %q", opened.SidesError)
	}
	for _, oid := range []string{opened.Sides.Old, opened.Sides.New} {
		if oid != "" && !gitdiff.IsFullOID(oid) {
			t.Fatalf("pin %+v is not full object IDs", *opened.Sides)
		}
	}
	return *opened.Sides
}

// pinnedFile names path as the pane does: with the blob the open pinned for
// it when the new side is the working tree.
func pinnedFile(opened ReviewDiffOpened, path, oldPath string) ReviewDiffFile {
	file := ReviewDiffFile{Path: path, OldPath: oldPath}
	for _, blob := range opened.WorktreeBlobs {
		if blob.Path == path {
			file.NewBlob = blob.Blob
		}
	}
	return file
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
	scopes := map[string]ReviewDiffSides{}
	opened, err := app.OpenCommitDiff(ctx, ref, h.head, false)
	scopes["commit"] = openedSides(t, opened, err)
	opened, err = app.OpenPRDiff(ctx, ref, prRef(), rev, false, false)
	scopes["pr"] = openedSides(t, opened, err)
	opened, err = app.OpenPRCommitDiff(ctx, ref, prRef(), rev, h.head, h.head, false, false)
	scopes["range"] = openedSides(t, opened, err)
	png := "image/png"
	for scope, sides := range scopes {
		if sides.New == "" {
			t.Fatalf("%s pinned a working tree: %+v", scope, sides)
		}
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
			got, err := app.GetReviewDiffImage(ctx, ref, sides, ReviewDiffFile{Path: tc.path, OldPath: tc.oldPath})
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
	got, err := app.GetReviewDiffImage(ctx, ref, scopes["commit"], ReviewDiffFile{Path: "fake.png"})
	if err != nil || !got.New.NotImage || got.New.URL != "" || got.New.Size != int64(len(h.images["fake.png"])) {
		t.Fatalf("a .png that is not an image = %+v (err %v), want its size and no URL", got.New, err)
	}
}

// A read answers the sides the diff was opened at, never what HEAD holds
// now: a mode-only change (no index line) serves its one blob on both
// sides, and after HEAD moves to another image the opened sides still
// serve the opened bytes.
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
	sides := openedSides(t, opened, err)
	patch, err := readOpenedReviewDiff(t, app, ctx, opened)
	if err != nil || strings.Contains(patch, "\nindex ") {
		t.Fatalf("a mode-only change printed an index line (err %v):\n%s", err, patch)
	}

	writeRepoBytes(t, repo, "mode.png", sizedPNG(t, 9, 9))
	testutil.RunGit(t, repo, "commit", "-qam", "another image")

	got, err := app.GetReviewDiffImage(ctx, ref, sides, ReviewDiffFile{Path: "mode.png"})
	if err != nil {
		t.Fatal(err)
	}
	expectSide(t, base, "mode-only old", got.Old, first, "image/png")
	expectSide(t, base, "mode-only new", got.New, first, "image/png")

	writeRepoBytes(t, repo, "wt.png", sizedPNG(t, 5, 7))
	opened, err = app.OpenWorkspaceDiff(ctx, ref, false)
	sides = openedSides(t, opened, err)
	file := pinnedFile(opened, "wt.png", "")
	testutil.RunGit(t, repo, "add", "wt.png")
	testutil.RunGit(t, repo, "commit", "-qm", "wt")
	writeRepoBytes(t, repo, "wt.png", sizedPNG(t, 3, 3))
	if _, err := app.GetReviewDiffImage(ctx, ref, sides, file); err == nil || !strings.Contains(err.Error(), "changed since the diff was read") {
		t.Fatalf("a working-tree file rewritten after HEAD moved = %v, want the changed refusal", err)
	}
}

// The working-tree scopes pin the snapshot: an untracked image and a
// committed one are served, a file rewritten since the open is refused, and
// the branch scope's old side is the merge base.
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
		sides := openedSides(t, opened, err)
		if sides.New != "" {
			t.Fatalf("%s did not pin the working tree: %+v", scope, sides)
		}
		got, err := app.GetReviewDiffImage(ctx, ref, sides, pinnedFile(opened, "fresh.png", ""))
		if err != nil {
			t.Fatalf("%s: %v", scope, err)
		}
		expectSide(t, base, scope+" old", got.Old, nil, "")
		expectSide(t, base, scope+" new", got.New, fresh, "image/png")
		if scope == "branch" {
			got, err := app.GetReviewDiffImage(ctx, ref, sides, pinnedFile(opened, "changed.png", ""))
			if err != nil {
				t.Fatalf("branch changed.png: %v", err)
			}
			expectSide(t, base, "branch changed old", got.Old, h.images["old:changed.png"], "image/png")
			expectSide(t, base, "branch changed new", got.New, h.images["changed.png"], "image/png")
		}
	}
	// A file rewritten after an earlier read of its pinned blob is answered
	// with the blob's bytes: the read is content-addressed.
	opened, err := app.OpenWorkspaceDiff(ctx, ref, false)
	sides := openedSides(t, opened, err)
	writeRepoBytes(t, h.repo, "fresh.png", sizedPNG(t, 3, 3))
	got, err := app.GetReviewDiffImage(ctx, ref, sides, pinnedFile(opened, "fresh.png", ""))
	if err != nil {
		t.Fatalf("a read blob rewritten since: %v", err)
	}
	expectSide(t, base, "a read blob rewritten since", got.New, fresh, "image/png")

	unread := sizedPNG(t, 6, 5)
	writeRepoBytes(t, h.repo, "fresh.png", unread)
	opened, err = app.OpenWorkspaceDiff(ctx, ref, false)
	sides = openedSides(t, opened, err)
	stale := pinnedFile(opened, "fresh.png", "")
	writeRepoBytes(t, h.repo, "fresh.png", sizedPNG(t, 3, 3))
	if _, err := app.GetReviewDiffImage(ctx, ref, sides, stale); err == nil || !strings.Contains(err.Error(), "changed since the diff was read") {
		t.Fatalf("a file rewritten since the open = %v, want the changed refusal", err)
	}
	if err := os.Remove(filepath.Join(h.repo, "fresh.png")); err != nil {
		t.Fatal(err)
	}
	if _, err := app.GetReviewDiffImage(ctx, ref, sides, stale); err == nil || !strings.Contains(err.Error(), "changed since the diff was read") {
		t.Fatalf("a file deleted since the open = %v, want the changed refusal", err)
	}
}

func TestGetReviewDiffImageRefusesWhatTheOpenedDiffDoesNotChange(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	h := newImageHistory(t)
	ref := testWorkspaceRef(t, app, h.repo)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenCommitDiff(ctx, ref, h.head, false)
	commit := openedSides(t, opened, err)
	writeRepoBytes(t, h.repo, "fresh.png", sizedPNG(t, 2, 2))
	opened, err = app.OpenWorkspaceDiff(ctx, ref, false)
	workspace := openedSides(t, opened, err)
	freshBlob := pinnedFile(opened, "fresh.png", "").NewBlob
	readmeBlob := revParseIn(t, h.repo, "HEAD:README.txt")
	tree := revParseIn(t, h.repo, h.head+"^{tree}")
	for _, tc := range []struct {
		name  string
		sides ReviewDiffSides
		file  ReviewDiffFile
		want  string
	}{
		{"a file the diff leaves alone", commit, ReviewDiffFile{Path: "README.txt"}, "not a file this diff changes"},
		{"a directory", commit, ReviewDiffFile{Path: "moved"}, "not a file"},
		{"a directory with a trailing slash", commit, ReviewDiffFile{Path: "moved/"}, "invalid path"},
		{"a glob", commit, ReviewDiffFile{Path: "*.png"}, "not a file this diff changes"},
		{"a path outside the repository", commit, ReviewDiffFile{Path: "../../etc/passwd"}, "invalid path"},
		{"a rename source of another file", commit, ReviewDiffFile{Path: "added.png", OldPath: "README.txt"}, "not a file this diff changes"},
		{"an abbreviated side", ReviewDiffSides{Old: commit.Old[:7], New: commit.New}, ReviewDiffFile{Path: "changed.png"}, "full"},
		{"a blob as a side", ReviewDiffSides{Old: readmeBlob, New: commit.New}, ReviewDiffFile{Path: "changed.png"}, "not a commit or a tree"},
		{"a missing side", ReviewDiffSides{Old: strings.Repeat("e", 40), New: commit.New}, ReviewDiffFile{Path: "changed.png"}, "not a commit or a tree"},
		{"a tree with no change", ReviewDiffSides{Old: tree, New: tree}, ReviewDiffFile{Path: "changed.png"}, "not a file this diff changes"},
		{"a working-tree file without its pin", workspace, ReviewDiffFile{Path: "fresh.png"}, "not a file the diff pinned"},
		{"another file's pin", workspace, ReviewDiffFile{Path: "fresh.png", NewBlob: readmeBlob}, "changed since the diff was read"},
		{"an abbreviated pin", workspace, ReviewDiffFile{Path: "fresh.png", NewBlob: freshBlob[:7]}, "full object ID"},
		{"a working-tree file the diff leaves alone", workspace, ReviewDiffFile{Path: "README.txt", NewBlob: readmeBlob}, "not a file this diff changes"},
	} {
		if _, err := app.GetReviewDiffImage(ctx, ref, tc.sides, tc.file); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
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
	sides := openedSides(t, opened, err)
	want := ReviewDiffImageSide{Exists: true, Size: int64(len(big)), OverCap: true}
	got, err := app.GetReviewDiffImage(ctx, ref, sides, ReviewDiffFile{Path: "big.png"})
	if err != nil || got.New != want {
		t.Fatalf("a committed side over the cap = %+v (err %v), want %+v", got.New, err, want)
	}
	writeRepoBytes(t, repo, "wt-big.png", big)
	opened, err = app.OpenWorkspaceDiff(ctx, ref, false)
	sides = openedSides(t, opened, err)
	got, err = app.GetReviewDiffImage(ctx, ref, sides, pinnedFile(opened, "wt-big.png", ""))
	if err != nil || got.New != want {
		t.Fatalf("a working-tree side over the cap = %+v (err %v), want %+v", got.New, err, want)
	}
	if held := app.reviewDiffImages().Held(); held != 0 {
		t.Fatalf("the image store holds %d bytes after over-cap answers", held)
	}
	edit, err := app.reviewDiffImageLoad(ctx, "edit\x00big", int64(len(big)), func([]byte) error {
		t.Fatal("an edit over the cap was read")
		return nil
	})
	if err != nil || edit != want {
		t.Fatalf("an edit over the cap = %+v (err %v), want %+v", edit, err, want)
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
	got, err := app.GetReviewDiffImage(ctx, ref, openedSides(t, opened, err), ReviewDiffFile{Path: "diagram.svg"})
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
	rev := gitops.PRRevision{BaseRef: "main", BaseSHA: h.base, HeadSHA: h.head}
	opened, err := app.OpenPRDiff(ctx, ref, prRef(), rev, false, false)
	sides := openedSides(t, opened, err)
	got, err := app.GetReviewDiffFileSizes(ctx, ref, sides, []ReviewDiffFile{
		{Path: "changed.png"},
		{Path: "fake.png"},
		{Path: "README.txt"},
		{Path: "moved"},
		{Path: "a/../b"},
		{Path: "moved/to.png", OldPath: "moved.png"},
		{Path: "deleted.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	size := func(name string) int64 { return int64(len(h.images[name])) }
	want := []ReviewDiffFileSizes{
		{Path: "changed.png", Old: ReviewDiffFileSide{true, size("old:changed.png")}, New: ReviewDiffFileSide{true, size("changed.png")}},
		{Path: "fake.png", New: ReviewDiffFileSide{true, size("fake.png")}},
		{Path: "README.txt", Error: "README.txt is not a file this diff changes"},
		{Path: "moved", Error: "moved is not a file"},
		{Path: "a/../b"},
		{Path: "moved/to.png", OldPath: "moved.png", Old: ReviewDiffFileSide{true, size("old:moved.png")}, New: ReviewDiffFileSide{true, size("moved/to.png")}},
		{Path: "deleted.png", Old: ReviewDiffFileSide{true, size("old:deleted.png")}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if want[i].Path == "a/../b" {
			if got[i].Error == "" {
				t.Errorf("file %d = %+v, want a refused path", i, got[i])
			}
			continue
		}
		if got[i] != want[i] {
			t.Errorf("file %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if _, err := app.GetReviewDiffFileSizes(ctx, ref, sides, make([]ReviewDiffFile, maxReviewDiffFiles+1)); err == nil {
		t.Fatal("an over-long batch was accepted")
	}
}

// An edit's image is the content the edit recorded, never the file on
// disk: without a snapshot the side is unavailable with a sentence, even
// when the disk holds a file a patch would match outside its hunks.
func TestGetEditDiffImageServesOnlyRecordedContent(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	workspace := t.TempDir()
	thread := testThread("thread-edit-diff-image")
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	ctx, _ := reviewDiffConn(t)
	patch := "diff --git a/diagram.svg b/diagram.svg\n--- a/diagram.svg\n+++ b/diagram.svg\n@@ -2 +2 @@\n-<script>alert(0)</script>\n+<script>alert(1)</script>\n"
	insertDiffSpanPayload(t, app, thread.ID, "item-recorded", "payload-recorded", "tool_result", patch)
	insertDiffSpanPayload(t, app, thread.ID, "item-unrecorded", "payload-unrecorded", "tool_result", patch)
	if err := app.store.PutEditFileSnapshot(thread.ID, "payload-recorded", "diagram.svg", diagramSVG, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	// The disk holds the edited line, but its other lines are not the
	// recorded ones.
	writeRepoBytes(t, workspace, "diagram.svg", []byte(strings.Replace(diagramSVG, `width="4" height="4"/>`, `width="1" height="1"/>`, 1)))

	got, err := app.GetEditDiffImage(ctx, thread.ID, EditDiffImageRequest{Path: "diagram.svg", EditPayloadID: "payload-recorded"})
	if err != nil {
		t.Fatalf("GetEditDiffImage: %v", err)
	}
	if got.Old != (ReviewDiffImageSide{Unavailable: "An edit does not record the file before it."}) {
		t.Fatalf("old = %+v", got.Old)
	}
	expectSide(t, base, "recorded new", got.New, []byte(diagramSVG), "image/svg+xml")

	got, err = app.GetEditDiffImage(ctx, thread.ID, EditDiffImageRequest{Path: "diagram.svg", EditPayloadID: "payload-unrecorded"})
	if err != nil {
		t.Fatalf("GetEditDiffImage: %v", err)
	}
	if got.New != (ReviewDiffImageSide{Unavailable: "This edit's file content was not recorded."}) {
		t.Fatalf("an edit with no recorded content = %+v, want unavailable", got.New)
	}
	for name, req := range map[string]EditDiffImageRequest{
		"a path outside the workspace": {Path: "../x.svg", EditPayloadID: "payload-recorded"},
		"a directory path":             {Path: "dir/", EditPayloadID: "payload-recorded"},
	} {
		if _, err := app.GetEditDiffImage(ctx, thread.ID, req); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := app.GetEditDiffImage(ctx, "no-such-thread", EditDiffImageRequest{Path: "diagram.svg"}); err == nil {
		t.Fatal("an unknown thread was accepted")
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
	got, err := app.GetReviewDiffImage(ctx, ref, openedSides(t, opened, err), ReviewDiffFile{Path: "added.png"})
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
}
