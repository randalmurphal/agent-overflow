package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
)

// newReviewDiffTestApp is newTestAppWithStore with the data directory the
// review diffs keep their worktree snapshots in.
func newReviewDiffTestApp(t *testing.T) *App {
	t.Helper()
	app := newTestAppWithStore(t)
	app.configDir = t.TempDir()
	return app
}

// reviewDiffConn returns a call context on a fresh connection, and ends that
// connection when the test does.
func reviewDiffConn(t *testing.T) (context.Context, *transport.ConnState) {
	t.Helper()
	ctx, state := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	t.Cleanup(state.RunCleanups)
	return ctx, state
}

// readOpenedReviewDiff reads the rest of an opened diff on ctx's
// connection and releases it.
func readOpenedReviewDiff(t *testing.T, app *App, ctx context.Context, opened ReviewDiffOpened) (string, error) {
	t.Helper()
	patch := opened.Chunk.Data
	if opened.ID == "" {
		if !opened.Chunk.EOF {
			t.Fatal("an opened diff with more to read must hold a handle")
		}
		return patch, nil
	}
	defer func() {
		if err := app.ReleaseReviewDiff(ctx, opened.ID); err != nil {
			t.Errorf("ReleaseReviewDiff: %v", err)
		}
	}()
	for chunk := opened.Chunk; !chunk.EOF; {
		var err error
		chunk, err = app.ReadReviewDiff(ctx, opened.ID, chunk.NextOffset, 4<<20)
		if err != nil {
			return "", err
		}
		patch += chunk.Data
	}
	return patch, nil
}

func openWorkspacePatch(t *testing.T, app *App, ref WorkspaceRef) (string, error) {
	t.Helper()
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenWorkspaceDiff(ctx, ref, false)
	if err != nil {
		return "", err
	}
	return readOpenedReviewDiff(t, app, ctx, opened)
}

func openBranchBasePatch(t *testing.T, app *App, ref WorkspaceRef, base string, ignoreWhitespace bool) (string, error) {
	t.Helper()
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenBranchBaseDiff(ctx, ref, base, ignoreWhitespace)
	if err != nil {
		return "", err
	}
	return readOpenedReviewDiff(t, app, ctx, opened)
}

func openPRCommitPatch(t *testing.T, app *App, ref WorkspaceRef, sha string) (string, error) {
	t.Helper()
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenPRCommitDiff(ctx, ref, prRef(), sha, false)
	if err != nil {
		return "", err
	}
	return readOpenedReviewDiff(t, app, ctx, opened)
}

func snapshotEntries(t *testing.T, app *App) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(app.configDir, reviewDiffSnapshotDir))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read snapshot dir: %v", err)
	}
	return len(entries)
}

func openReviewDiffCount(app *App) int {
	app.reviewDiffs.mu.Lock()
	defer app.reviewDiffs.mu.Unlock()
	return len(app.reviewDiffs.diffs)
}

// largeWorkspace returns a workspace whose uncommitted patch is several
// first chunks long.
func largeWorkspace(t *testing.T, app *App) (WorkspaceRef, string) {
	t.Helper()
	repo := testutil.InitGitRepo(t)
	var b strings.Builder
	for b.Len() < 3*reviewDiffFirstChunkBytes {
		b.WriteString("a line of generated content that goes on for a while\n")
	}
	if err := os.WriteFile(filepath.Join(repo, "generated.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write generated file: %v", err)
	}
	return testWorkspaceRef(t, app, repo), b.String()
}

func TestOpenReviewDiffHoldsNothingForAPatchThatFitsTheFirstChunk(t *testing.T) {
	app := newReviewDiffTestApp(t)
	repo := testutil.InitGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "README.txt"), []byte("hello\nedited\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenWorkspaceDiff(ctx, testWorkspaceRef(t, app, repo), false)
	if err != nil {
		t.Fatalf("OpenWorkspaceDiff: %v", err)
	}
	if opened.ID != "" || !opened.Chunk.EOF || !strings.Contains(opened.Chunk.Data, "+edited") {
		t.Fatalf("opened = %+v; want the whole patch and no handle", opened)
	}
	if n := openReviewDiffCount(app); n != 0 {
		t.Fatalf("%d diffs held after a one-chunk patch", n)
	}
	if n := snapshotEntries(t, app); n != 0 {
		t.Fatalf("%d snapshots left after a one-chunk patch", n)
	}
}

func TestReviewDiffHandleReadsTheRestOnlyOnItsConnection(t *testing.T) {
	app := newReviewDiffTestApp(t)
	ref, content := largeWorkspace(t, app)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenWorkspaceDiff(ctx, ref, false)
	if err != nil {
		t.Fatalf("OpenWorkspaceDiff: %v", err)
	}
	if opened.ID == "" || opened.Chunk.EOF {
		t.Fatalf("a %d-byte patch must hold a handle past its first chunk", len(content))
	}

	otherCtx, _ := reviewDiffConn(t)
	if _, err := app.ReadReviewDiff(otherCtx, opened.ID, opened.Chunk.NextOffset, 1<<20); !errors.Is(err, ErrReviewDiffNotFound) {
		t.Fatalf("read on another connection: err = %v, want ErrReviewDiffNotFound", err)
	}
	if err := app.ReleaseReviewDiff(otherCtx, opened.ID); err != nil {
		t.Fatalf("release on another connection: %v", err)
	}
	if n := openReviewDiffCount(app); n != 1 {
		t.Fatalf("another connection's release closed the diff: %d held", n)
	}

	patch, err := readOpenedReviewDiff(t, app, ctx, opened)
	if err != nil {
		t.Fatalf("read the rest: %v", err)
	}
	if want := "+" + strings.ReplaceAll(strings.TrimSuffix(content, "\n"), "\n", "\n+") + "\n"; !strings.Contains(patch, want) {
		t.Fatal("the reassembled patch does not hold the whole file")
	}
	if n := openReviewDiffCount(app); n != 0 {
		t.Fatalf("%d diffs held after release", n)
	}
	if n := snapshotEntries(t, app); n != 0 {
		t.Fatalf("%d snapshots left after release", n)
	}
	if _, err := app.ReadReviewDiff(ctx, opened.ID, 0, 1<<20); !errors.Is(err, ErrReviewDiffNotFound) {
		t.Fatalf("read after release: err = %v, want ErrReviewDiffNotFound", err)
	}
}

func TestReviewDiffIsReleasedWithItsConnection(t *testing.T) {
	app := newReviewDiffTestApp(t)
	ref, _ := largeWorkspace(t, app)
	ctx, state := reviewDiffConn(t)
	opened, err := app.OpenWorkspaceDiff(ctx, ref, false)
	if err != nil || opened.ID == "" {
		t.Fatalf("OpenWorkspaceDiff: %+v, %v", opened.ID, err)
	}
	state.RunCleanups()
	deadline := time.Now().Add(10 * time.Second)
	for openReviewDiffCount(app) != 0 || snapshotEntries(t, app) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the diff outlived its connection")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOpenReviewDiffRefusesPastTheHandleLimit(t *testing.T) {
	app := newReviewDiffTestApp(t)
	ref, _ := largeWorkspace(t, app)
	app.reviewDiffs.diffs = make(map[string]*openReviewDiff, maxReviewDiffHandles)
	for i := range maxReviewDiffHandles {
		app.reviewDiffs.diffs[string(rune('a'+i%26))+strings.Repeat("x", i)] = &openReviewDiff{}
	}
	t.Cleanup(func() { app.reviewDiffs.diffs = nil })
	ctx, _ := reviewDiffConn(t)
	if _, err := app.OpenWorkspaceDiff(ctx, ref, false); !errors.Is(err, ErrTooManyReviewDiffs) {
		t.Fatalf("open past the limit: err = %v, want ErrTooManyReviewDiffs", err)
	}
	if n := snapshotEntries(t, app); n != 0 {
		t.Fatalf("a refused open left %d snapshots", n)
	}
}

func TestReviewDiffBootSweepRemovesSnapshotsFromTheLastRun(t *testing.T) {
	app := newReviewDiffTestApp(t)
	left := filepath.Join(app.configDir, reviewDiffSnapshotDir, "snapshot-123", "objects")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(left, "blob"), []byte("left behind"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := app.sweepReviewDiffSnapshots(); err != nil {
		t.Fatalf("sweepReviewDiffSnapshots: %v", err)
	}
	if n := snapshotEntries(t, app); n != 0 {
		t.Fatalf("%d snapshots survived the boot sweep", n)
	}
}

func TestOpenPRDiffIsTheThreeDotDiffAtTheFetchedHead(t *testing.T) {
	app := newReviewDiffTestApp(t)
	ref, clone, prSHAs := prCloneFixture(t, app)
	// The base moves on after the PR branched; a three-dot diff leaves it out.
	origin, _, err := gitops.NewCore().Execute(clone, "remote", "get-url", "origin")
	if err != nil {
		t.Fatalf("origin url: %v", err)
	}
	origin = strings.TrimSpace(origin)
	if err := os.WriteFile(filepath.Join(origin, "base-only.txt"), []byte("base moved on\n"), 0o644); err != nil {
		t.Fatalf("write base-only: %v", err)
	}
	testutil.RunGit(t, origin, "add", "base-only.txt")
	testutil.RunGit(t, origin, "commit", "-m", "base moves on")
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenPRDiff(ctx, ref, prRef(), "main", false)
	if err != nil {
		t.Fatalf("OpenPRDiff: %v", err)
	}
	if opened.HeadSHA != prSHAs[0] {
		t.Fatalf("HeadSHA = %q, want the fetched head %q", opened.HeadSHA, prSHAs[0])
	}
	patch, err := readOpenedReviewDiff(t, app, ctx, opened)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"+first.txt content", "+second.txt content"} {
		if !strings.Contains(patch, want) {
			t.Fatalf("PR diff lacks %q:\n%s", want, patch)
		}
	}
	if strings.Contains(patch, "base moved on") {
		t.Fatalf("PR diff includes the base's own later commit:\n%s", patch)
	}
}

// A handle is read under the scope its Open required, checked on every
// read, whatever the reading method's own floor admits.
func TestReadReviewDiffRequiresTheScopeItsOpenRequired(t *testing.T) {
	app := identityApp(t)
	threadID := seedEditPayloads(t, app, 1, generatedPatch("big.go", 60000))
	session := pairSessionWithScopes(t, app, "thumb-threads", []identity.Scope{identity.ScopeThreadsRead})
	ctx, state := transport.WithConnState(context.Background(), transport.ConnPrincipal{SessionID: session.ID})
	t.Cleanup(state.RunCleanups)

	opened, err := app.OpenTurnEditsDiff(ctx, threadID, 1)
	if err != nil || opened.ID == "" {
		t.Fatalf("OpenTurnEditsDiff() = %+v, %v", opened.ID, err)
	}
	if _, err := app.ReadReviewDiff(ctx, opened.ID, opened.Chunk.NextOffset, 1<<20); err != nil {
		t.Fatalf("a session holding threads:read reading an edits diff: %v", err)
	}

	diff, err := newEditsDiff(ctx, app.store, threadID, opened.PayloadIDs)
	if err != nil {
		t.Fatalf("newEditsDiff() error = %v", err)
	}
	id, err := app.holdReviewDiff(ctx, diff, transport.ScopeFilesRead)
	if err != nil {
		t.Fatalf("holdReviewDiff() error = %v", err)
	}
	_, err = app.ReadReviewDiff(ctx, id, 0, 1<<20)
	wantScopeRefusal(t, err, transport.ScopeFilesRead)
	if err := app.ReleaseReviewDiff(ctx, id); err != nil {
		t.Fatalf("a session releasing its own handle: %v", err)
	}
	if n := openReviewDiffCount(app); n != 1 {
		t.Fatalf("%d diffs held, want only the edits diff", n)
	}
}

// A read that finds its diff closed under it answers "not open", the one
// error the client retries from a fresh Open.
func TestReadReviewDiffOfAClosedDiffIsNotOpen(t *testing.T) {
	app := newReviewDiffTestApp(t)
	ref, _ := largeWorkspace(t, app)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenWorkspaceDiff(ctx, ref, false)
	if err != nil || opened.ID == "" {
		t.Fatalf("OpenWorkspaceDiff: %+v, %v", opened.ID, err)
	}
	open, err := app.ownedReviewDiff(ctx, opened.ID)
	if err != nil {
		t.Fatalf("ownedReviewDiff: %v", err)
	}
	if err := open.diff.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := app.ReadReviewDiff(ctx, opened.ID, opened.Chunk.NextOffset, 1<<20); !errors.Is(err, ErrReviewDiffNotFound) {
		t.Fatalf("read of a closed diff: err = %v, want ErrReviewDiffNotFound", err)
	}
}
