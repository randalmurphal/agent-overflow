package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func openPRCommitPatch(t *testing.T, app *App, ref WorkspaceRef, rev gitops.PRRevision, sha string) (string, error) {
	t.Helper()
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenPRCommitDiff(ctx, ref, prRef(), rev, sha, sha, false, true)
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
	repo := initMainGitRepo(t)
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
	t.Parallel()
	app := newReviewDiffTestApp(t)
	repo := initMainGitRepo(t)
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
	// The files pin holds the snapshot until it settles, then lets go.
	app.reviewDiffFiles.pins.Wait()
	if n := snapshotEntries(t, app); n != 0 {
		t.Fatalf("%d snapshots left after a one-chunk patch", n)
	}
}

func TestReviewDiffHandleReadsTheRestOnlyOnItsConnection(t *testing.T) {
	t.Parallel()
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
	app.reviewDiffFiles.pins.Wait()
	if n := snapshotEntries(t, app); n != 0 {
		t.Fatalf("%d snapshots left after release", n)
	}
	if _, err := app.ReadReviewDiff(ctx, opened.ID, 0, 1<<20); !errors.Is(err, ErrReviewDiffNotFound) {
		t.Fatalf("read after release: err = %v, want ErrReviewDiffNotFound", err)
	}
}

func TestReviewDiffIsReleasedWithItsConnection(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// The forge's base tip has moved on past the clone's origin/main: the read
// fetches it, and the three-dot diff leaves the base's own commit out.
func TestOpenPRDiffIsTheThreeDotDiffAtTheRevision(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	ref, clone, prSHAs := prCloneFixture(t, app)
	origin := moveOriginMainOn(t, clone)
	rev := gitops.PRRevision{BaseRef: "main", BaseSHA: revParseIn(t, origin, "main"), HeadSHA: prSHAs[0]}
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenPRDiff(ctx, ref, prRef(), rev, false, true)
	if err != nil {
		t.Fatalf("OpenPRDiff: %v", err)
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

// A commit the clone lacks and cannot fetch fails the read with git's own
// diagnosis rather than an exit code.
func TestOpenPRDiffReportsWhyTheCommitsCouldNotBeFetched(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	ref, clone, prSHAs := prCloneFixture(t, app)
	origin := moveOriginMainOn(t, clone)
	rev := gitops.PRRevision{BaseRef: "main", BaseSHA: revParseIn(t, origin, "main"), HeadSHA: prSHAs[0]}
	testutil.RunGit(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
	ctx, _ := reviewDiffConn(t)
	_, err := app.OpenPRDiff(ctx, ref, prRef(), rev, false, true)
	if err == nil {
		t.Fatal("OpenPRDiff succeeded without the base commit")
	}
	for _, want := range []string{"open PR diff: fetch pull request commits", "exited with code 128", "fatal:"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	}
}

// moveOriginMainOn commits to the fixture origin's main after the clone
// was made and returns the origin's path.
func moveOriginMainOn(t *testing.T, clone string) string {
	t.Helper()
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
	return origin
}

func revParseIn(t *testing.T, repo, rev string) string {
	t.Helper()
	out, _, err := gitops.NewCore().Execute(repo, "rev-parse", rev)
	if err != nil {
		t.Fatalf("rev-parse %s: %v", rev, err)
	}
	return strings.TrimSpace(out)
}

// A handle is read under the scope its Open required, checked on every
// read, whatever the reading method's own floor admits.
func TestReadReviewDiffRequiresTheScopeItsOpenRequired(t *testing.T) {
	t.Parallel()
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
	id, err := app.holdReviewDiff(testConnection(t, ctx), diff, transport.ScopeFilesRead)
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
	t.Parallel()
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

// reviewDiffTieFixture is a repository the git-backed Open*Diff rows of
// connectionTieChecks all open a diff of: a feature branch one commit off
// main, with an uncommitted file.
type reviewDiffTieFixture struct {
	ref WorkspaceRef
	rev gitops.PRRevision
}

const reviewDiffTieUncommitted = "uncommitted.txt"

// gitReviewDiffConnectionTie is a git-backed Open*Diff method's row of
// connectionTieChecks: the files record every such open keeps for its
// connection, which nothing but the connection or eviction removes. The
// diffs it opens fit their first chunk, so no handle is held.
func gitReviewDiffConnectionTie(t *testing.T, open func(*App, context.Context, reviewDiffTieFixture) (ReviewDiffOpened, error)) connectionTie {
	app := newReviewDiffTestApp(t)
	t.Cleanup(func() {
		app.closeReviewDiffRecords()
		if err := app.closeReviewDiffs(); err != nil {
			t.Error(err)
		}
	})
	h := newImageHistory(t)
	writeRepoBytes(t, h.repo, reviewDiffTieUncommitted, []byte("not committed\n"))
	fixture := reviewDiffTieFixture{
		ref: testWorkspaceRef(t, app, h.repo),
		rev: gitops.PRRevision{BaseRef: "main", BaseSHA: h.base, HeadSHA: h.head},
	}
	return connectionTie{
		open: func(ctx context.Context) (string, error) {
			opened, err := open(app, ctx, fixture)
			if err == nil && opened.ID != "" {
				t.Fatal("the fixture's diff should fit its first chunk")
			}
			return opened.FilesHandle, err
		},
		held: func() int { return app.reviewDiffFiles.len() + openReviewDiffCount(app) },
	}
}

// editsReviewDiffConnectionTie is an edits Open*Diff method's row of
// connectionTieChecks: a diff too long for its first chunk, held under a
// handle only its own connection releases.
func editsReviewDiffConnectionTie(t *testing.T, open func(*App, context.Context, string) (ReviewDiffOpened, error)) connectionTie {
	app := newReviewDiffTestApp(t)
	t.Cleanup(func() {
		if err := app.closeReviewDiffs(); err != nil {
			t.Error(err)
		}
	})
	threadID := seedEditPayloads(t, app, 1, generatedPatch("big.go", 60000))
	return connectionTie{
		open: func(ctx context.Context) (string, error) {
			opened, err := open(app, ctx, threadID)
			if err == nil && opened.ID == "" {
				t.Fatal("the fixture's diff should be held past its first chunk")
			}
			return opened.ID, err
		},
		held:    func() int { return openReviewDiffCount(app) },
		release: app.ReleaseReviewDiff,
		key:     reviewDiffCleanupKey,
	}
}

// closeCountingDiff is a held diff that counts its closes and fails them
// with errClosingDiff.
type closeCountingDiff struct {
	reviewDiffReader
	closes atomic.Int32
}

var errClosingDiff = errors.New("closing the diff failed")

func (d *closeCountingDiff) Close() error {
	d.closes.Add(1)
	return errClosingDiff
}

// A diff whose hold is refused, on a connection that is closing or on none
// at all, is closed before the refusal returns, its close's failure joins
// the refusal, and nothing is held: the refusal does not hand the close to
// a release that runs later.
func TestARefusedHoldClosesTheDiffBeforeItReturns(t *testing.T) {
	t.Parallel()
	closing, state := newTestConnection(t, transport.ConnPrincipal{})
	state.RunCleanups()
	for _, tc := range []struct {
		name string
		conn *transport.ConnState
		want error
	}{
		{"a closing connection", testConnection(t, closing), transport.ErrConnectionClosing},
		{"no connection", nil, transport.ErrNoConnection},
	} {
		app := newReviewDiffTestApp(t)
		diff := &closeCountingDiff{}
		_, err := app.holdReviewDiff(tc.conn, diff, transport.ScopeFilesRead)
		if !errors.Is(err, tc.want) || !errors.Is(err, errClosingDiff) {
			t.Fatalf("%s: hold = %v, want %v joined with the close's failure", tc.name, err, tc.want)
		}
		if got := diff.closes.Load(); got != 1 {
			t.Fatalf("%s: the refused diff was closed %d times by the time the refusal returned, want 1", tc.name, got)
		}
		if n := openReviewDiffCount(app); n != 0 {
			t.Fatalf("%s: a refused hold left %d diffs held", tc.name, n)
		}
	}
}

// A record is tied to its connection from the moment it is reserved, so an
// eviction that lands before its open finishes unties what it removes,
// and a record evicted from under its open leaves nothing bound.
func TestAReviewDiffRecordIsTiedWhenItIsReserved(t *testing.T) {
	t.Parallel()
	ctx, state := newTestConnection(t, transport.ConnPrincipal{})
	conn := testConnection(t, ctx)
	records := &reviewDiffRecords{}
	first, _, err := records.reserve("/w", conn, transport.ScopeFilesRead, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if !state.Bound(reviewDiffFilesCleanupKey(first.id)) {
		t.Fatal("a reserved record is not tied to its connection yet")
	}
	close(first.done)
	var evicted []*reviewDiffRecord
	for range maxReviewDiffRecords {
		record, pushed, err := records.reserve("/w", conn, transport.ScopeFilesRead, func() {})
		if err != nil {
			t.Fatal(err)
		}
		close(record.done)
		evicted = append(evicted, pushed...)
	}
	if len(evicted) != 1 || evicted[0] != first {
		t.Fatalf("evicted %d records, want the first", len(evicted))
	}
	stopReviewDiffRecords(evicted...)
	if state.Bound(reviewDiffFilesCleanupKey(first.id)) {
		t.Fatal("an evicted record left its release bound on its connection")
	}
	none, _, err := records.reserve("/w", nil, transport.ScopeFilesRead, func() {})
	if !errors.Is(err, transport.ErrNoConnection) || none != nil {
		t.Fatalf("reserve with no connection = %v, %v; want ErrNoConnection and no record", none, err)
	}
	if n := records.len(); n != maxReviewDiffRecords {
		t.Fatalf("%d records after a refused reserve, want %d", n, maxReviewDiffRecords)
	}
}

// An open refused after its record was reserved (the handle cap is
// reached) removes the record and leaves nothing bound on its connection.
func TestAnOpenRefusedAfterItsRecordLeavesNothingTied(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	t.Cleanup(app.closeReviewDiffRecords)
	ref, _ := largeWorkspace(t, app)
	app.reviewDiffs.diffs = make(map[string]*openReviewDiff, maxReviewDiffHandles)
	for i := range maxReviewDiffHandles {
		app.reviewDiffs.diffs[fmt.Sprint("held-", i)] = &openReviewDiff{}
	}
	t.Cleanup(func() { app.reviewDiffs.diffs = nil })
	ctx, state := newTestConnection(t, transport.ConnPrincipal{})
	// The handle registry's lock holds the open at its hold, after its
	// record was reserved, so the record's tie is seen before the refusal
	// rolls it back: the rollback is proven only if there was a release.
	app.reviewDiffs.mu.Lock()
	refused := make(chan error, 1)
	go func() {
		_, err := app.OpenWorkspaceDiff(ctx, ref, false)
		refused <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(state.BoundKeys()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	tied := state.BoundKeys()
	app.reviewDiffs.mu.Unlock()
	if err := <-refused; !errors.Is(err, ErrTooManyReviewDiffs) {
		t.Fatalf("open past the handle cap = %v, want ErrTooManyReviewDiffs", err)
	}
	if len(tied) != 1 {
		t.Fatalf("the open's reserved record had %d releases bound before its refusal, want 1: %v", len(tied), tied)
	}
	if n := app.reviewDiffFiles.len(); n != 0 {
		t.Fatalf("the refused open left %d records", n)
	}
	if keys := state.BoundKeys(); len(keys) != 0 {
		t.Fatalf("the refused open left releases bound on its connection: %v", keys)
	}
}
