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

	"agent-overflow/internal/errorsx"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
)

// wantCode asserts err carries the public code and message.
func wantCode(t *testing.T, err error, code, message string) {
	t.Helper()
	gotCode, gotMessage, ok := errorsx.PublicDetails(err)
	if !ok || gotCode != code || gotMessage != message {
		t.Fatalf("err = %v (code %q, message %q), want %q: %q", err, gotCode, gotMessage, code, message)
	}
}

// heldPins makes every pin of app wait at its start until released, or
// until it is cancelled; started reports each pin as it reaches the hold.
func heldPins(app *App) (started <-chan struct{}, release func()) {
	reached := make(chan struct{}, 128)
	released := make(chan struct{})
	app.reviewDiffPinHook = func(ctx context.Context) {
		reached <- struct{}{}
		select {
		case <-released:
		case <-ctx.Done():
		}
	}
	var once atomic.Bool
	return reached, func() {
		if once.CompareAndSwap(false, true) {
			close(released)
		}
	}
}

func snapshotDirs(t *testing.T, app *App) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(app.configDir, reviewDiffSnapshotDir))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// The open never waits for the pin: it answers its chunk and handle while
// the pin is held; a file read waits for the pin and then answers; the
// snapshot outlives the diff until the pin exits, then is removed once.
func TestTheOpenDoesNotWaitForThePin(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	repo := testutil.InitGitRepo(t)
	fresh := sizedPNG(t, 4, 4)
	writeRepoBytes(t, repo, "fresh.png", fresh)
	ref := testWorkspaceRef(t, app, repo)
	ctx, _ := reviewDiffConn(t)
	started, release := heldPins(app)
	defer release()

	opened, err := app.OpenWorkspaceDiff(ctx, ref, false)
	handle := filesHandle(t, opened, err)
	<-started
	if !strings.Contains(opened.Chunk.Data, "b/fresh.png") || opened.ID != "" {
		t.Fatalf("the open answered %q (id %q) while the pin was held", opened.Chunk.Data, opened.ID)
	}
	// The diff fit its first chunk and is closed; the pin still holds the
	// snapshot.
	if n := snapshotDirs(t, app); n != 1 {
		t.Fatalf("%d snapshots while the pin is held, want 1", n)
	}
	answered := make(chan []ReviewDiffFileSizes, 1)
	go func() {
		sizes, err := app.GetReviewDiffFileSizes(ctx, ref, handle, []ReviewDiffFile{{Path: "fresh.png"}})
		if err != nil {
			t.Error(err)
		}
		answered <- sizes
	}()
	select {
	case <-answered:
		t.Fatal("a sizes read answered before the pin settled")
	default:
	}
	release()
	sizes := <-answered
	if len(sizes) != 1 || sizes[0].New != (ReviewDiffFileSide{true, int64(len(fresh))}) {
		t.Fatalf("sizes after the pin = %+v", sizes)
	}
	if n := snapshotDirs(t, app); n != 0 {
		t.Fatalf("%d snapshots after the pin exited, want 0", n)
	}
}

// A pin that fails answers its own coded refusal on every file read, and
// the patch reads on.
func TestAFailedPinIsAnUnreadableRefusal(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	repo := testutil.InitGitRepo(t)
	writeRepoBytes(t, repo, "fresh.png", sizedPNG(t, 2, 2))
	writeRepoBytes(t, repo, "big.txt", []byte(strings.Repeat("line\n", 400_000)))
	ref := testWorkspaceRef(t, app, repo)
	ctx, _ := reviewDiffConn(t)
	app.reviewDiffPinTimeout = time.Millisecond
	app.reviewDiffPinHook = func(ctx context.Context) { <-ctx.Done() }
	opened, err := app.OpenWorkspaceDiff(ctx, ref, false)
	handle := filesHandle(t, opened, err)
	_, err = app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: "fresh.png"})
	wantCode(t, err, reviewDiffFilesUnreadableCode, "This diff's files cannot be read: listing them took longer than 1ms.")
	_, err = app.GetReviewDiffFileSizes(ctx, ref, handle, []ReviewDiffFile{{Path: "fresh.png"}})
	wantCode(t, err, reviewDiffFilesUnreadableCode, "This diff's files cannot be read: listing them took longer than 1ms.")
	if opened.ID == "" {
		t.Fatal("the big diff holds no id")
	}
	patch, err := readOpenedReviewDiff(t, app, ctx, opened)
	if err != nil || !strings.Contains(patch, "b/fresh.png") {
		t.Fatalf("the patch after a failed pin (err %v)", err)
	}
}

// A files handle is as strict as a diff id: another connection cannot
// read it and is told only that it is not held; a read rechecks the scope
// the open required.
func TestAFilesHandleBelongsToItsConnection(t *testing.T) {
	t.Parallel()
	app := identityApp(t)
	app.configDir = t.TempDir()
	h := newImageHistory(t)
	ref := testWorkspaceRef(t, app, h.repo)
	session := pairSessionWithScopes(t, app, "thumb-files", []identity.Scope{identity.ScopeFilesRead})
	ctx, state := transport.WithConnState(context.Background(), transport.ConnPrincipal{SessionID: session.ID})
	t.Cleanup(state.RunCleanups)

	opened, err := app.OpenCommitDiff(ctx, ref, h.head, false)
	handle := filesHandle(t, opened, err)
	if _, err := app.GetReviewDiffFileSizes(ctx, ref, handle, []ReviewDiffFile{{Path: "added.png"}}); err != nil {
		t.Fatalf("the owner reading its handle: %v", err)
	}
	other, otherState := transport.WithConnState(context.Background(), transport.ConnPrincipal{SessionID: session.ID})
	t.Cleanup(otherState.RunCleanups)
	_, err = app.GetReviewDiffFileSizes(other, ref, handle, []ReviewDiffFile{{Path: "added.png"}})
	wantCode(t, err, reviewDiffFilesGoneCode, "This diff's files are no longer held; reload the diff.")

	// The PR open requires git:operate, which this session does not hold:
	// its handle is refused as a diff id read is.
	rev := gitops.PRRevision{BaseRef: "main", BaseSHA: h.base, HeadSHA: h.head}
	opened, err = app.OpenPRDiff(ctx, ref, prRef(), rev, false, false)
	prHandle := filesHandle(t, opened, err)
	_, err = app.GetReviewDiffFileSizes(ctx, ref, prHandle, []ReviewDiffFile{{Path: "added.png"}})
	wantScopeRefusal(t, err, transport.ScopeGitOperate)
	_, err = app.GetReviewDiffImage(ctx, ref, prHandle, ReviewDiffFile{Path: "added.png"})
	wantScopeRefusal(t, err, transport.ScopeGitOperate)
}

// The owner's teardown removes its records, a held diff's and a
// single-chunk diff's alike, with their pins and snapshots, and frees
// their bounds; an open racing teardown keeps nothing.
func TestTeardownRemovesTheOwnersRecords(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	repo := testutil.InitGitRepo(t)
	writeRepoBytes(t, repo, "fresh.png", sizedPNG(t, 2, 2))
	ref := testWorkspaceRef(t, app, repo)
	ctx, state := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	started, release := heldPins(app)
	defer release()

	single, err := app.OpenWorkspaceDiff(ctx, ref, false)
	filesHandle(t, single, err)
	<-started
	writeRepoBytes(t, repo, "big.txt", []byte(strings.Repeat("line\n", 400_000)))
	held, err := app.OpenWorkspaceDiff(ctx, ref, false)
	filesHandle(t, held, err)
	<-started
	if held.ID == "" || single.ID != "" {
		t.Fatalf("ids %q and %q, want one held diff and one single chunk", single.ID, held.ID)
	}
	if app.reviewDiffFiles.len() != 2 {
		t.Fatalf("%d records before teardown", app.reviewDiffFiles.len())
	}
	state.RunCleanups()
	app.reviewDiffFiles.pins.Wait()
	if n := app.reviewDiffFiles.len(); n != 0 {
		t.Fatalf("%d records after the owner left", n)
	}
	// Closing a held diff's snapshot is asynchronous on teardown.
	for deadline := time.Now().Add(10 * time.Second); snapshotDirs(t, app) != 0; {
		if time.Now().After(deadline) {
			t.Fatalf("%d snapshots after the owner left", snapshotDirs(t, app))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if app.reviewDiffFiles.bindings != 0 || app.reviewDiffFiles.pathBytes != 0 {
		t.Fatalf("bounds not freed: %d bindings, %d path bytes", app.reviewDiffFiles.bindings, app.reviewDiffFiles.pathBytes)
	}

	late, lateState := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	lateState.RunCleanups()
	if _, err := app.OpenWorkspaceDiff(late, ref, false); err == nil {
		t.Fatal("an open on a closed connection answered")
	}
	app.reviewDiffFiles.pins.Wait()
	if n := app.reviewDiffFiles.len(); n != 0 || snapshotDirs(t, app) != 0 {
		t.Fatalf("an open racing teardown kept %d records, %d snapshots", n, snapshotDirs(t, app))
	}
}

// Eviction and the harness reset stop a held pin: a read waiting on it
// answers that the files are no longer held, and no record, snapshot or
// goroutine is left; the pin publishes nothing.
func TestEvictionAndResetStopAHeldPin(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"eviction", "reset"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			app, _ := reviewDiffFilesApp(t)
			repo := testutil.InitGitRepo(t)
			writeRepoBytes(t, repo, "fresh.png", sizedPNG(t, 2, 2))
			ref := testWorkspaceRef(t, app, repo)
			ctx, _ := reviewDiffConn(t)
			// Only a cancel ends the held pin: its own bound is far away.
			app.reviewDiffPinTimeout = time.Hour
			var pins atomic.Int32
			first := make(chan struct{})
			app.reviewDiffPinHook = func(ctx context.Context) {
				if pins.Add(1) == 1 {
					close(first)
					<-ctx.Done()
				}
			}
			opened, err := app.OpenWorkspaceDiff(ctx, ref, false)
			handle := filesHandle(t, opened, err)
			<-first
			waiting := make(chan struct{})
			app.reviewDiffReadHook = func() { close(waiting) }
			read := make(chan error, 1)
			go func() {
				_, err := app.GetReviewDiffFileSizes(ctx, ref, handle, []ReviewDiffFile{{Path: "fresh.png"}})
				read <- err
			}()
			<-waiting
			app.reviewDiffReadHook = nil
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				switch how {
				case "eviction":
					for range maxReviewDiffRecords - 1 {
						record, _ := app.reviewDiffFiles.reserve("/elsewhere", nil, transport.ScopeFilesRead, func() {})
						close(record.done)
					}
					// The next open pushes the held record out and stops its pin.
					opened, err := app.OpenCommitDiff(ctx, ref, revParseIn(t, repo, "HEAD"), false)
					filesHandle(t, opened, err)
				case "reset":
					(&harnessHost{app: app}).DropReviewDiffImages()
				}
			}()
			select {
			case <-stopped:
			case <-time.After(10 * time.Second):
				t.Fatalf("the %s did not stop the held pin", how)
			}
			select {
			case err := <-read:
				wantCode(t, err, reviewDiffFilesGoneCode, "This diff's files are no longer held; reload the diff.")
			case <-time.After(10 * time.Second):
				t.Fatalf("a read waiting on the pin kept waiting after the %s", how)
			}
			_, err = app.GetReviewDiffFileSizes(ctx, ref, handle, []ReviewDiffFile{{Path: "fresh.png"}})
			wantCode(t, err, reviewDiffFilesGoneCode, "This diff's files are no longer held; reload the diff.")
			if n := snapshotDirs(t, app); n != 0 {
				t.Fatalf("%d snapshots after the %s", n, how)
			}
			done := make(chan struct{})
			go func() { app.reviewDiffFiles.pins.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("a pin outlived the %s", how)
			}
		})
	}
}

// The store keeps at most its count, least recently used first out, and a
// settling pin past the per-record or total bound fails with a sentence.
func TestReviewDiffRecordsAreBounded(t *testing.T) {
	t.Parallel()
	var records reviewDiffRecords
	reserve := func() *reviewDiffRecord {
		record, evicted := records.reserve("/w", nil, transport.ScopeFilesRead, func() {})
		close(record.done)
		for _, gone := range evicted {
			stopReviewDiffRecords(gone)
		}
		return record
	}
	committed := gitdiff.Pin{Old: strings.Repeat("a", 40), New: strings.Repeat("b", 40)}
	worktree := gitdiff.Pin{Old: strings.Repeat("a", 40)}
	first := reserve()
	for i := 1; i < maxReviewDiffRecords; i++ {
		reserve()
	}
	if _, err := records.get(first.id, "/w", nil); err != nil {
		t.Fatalf("the first record went before the bound: %v", err)
	}
	reserve()
	if records.len() != maxReviewDiffRecords {
		t.Fatalf("%d records, want %d", records.len(), maxReviewDiffRecords)
	}
	if _, err := records.get(first.id, "/w", nil); err != nil {
		t.Fatalf("the recently used record was evicted: %v", err)
	}
	if _, err := records.get(first.id, "/other", nil); !errors.Is(err, errReviewDiffFilesGone) {
		t.Fatalf("another workspace = %v, want the same refusal as a missing handle", err)
	}
	records.settle(first, committed, nil, false, "")
	if first.failure != "" {
		t.Fatalf("a committed pin failed: %s", first.failure)
	}

	many := func(n int) []gitdiff.WorktreeBlob {
		blobs := make([]gitdiff.WorktreeBlob, n)
		for i := range blobs {
			blobs[i] = gitdiff.WorktreeBlob{Path: fmt.Sprintf("f%d", i), Mode: "100644"}
		}
		return blobs
	}
	truncated := reserve()
	records.settle(truncated, worktree, many(maxPinnedWorktreeBlobs), true, "")
	if truncated.failure != fmt.Sprintf("it changes more than %d files", maxPinnedWorktreeBlobs) {
		t.Fatalf("a pin past the per-record bound = %q", truncated.failure)
	}
	for range maxReviewDiffRecordBindings / maxPinnedWorktreeBlobs {
		record := reserve()
		records.settle(record, worktree, many(maxPinnedWorktreeBlobs), false, "")
		if record.failure != "" {
			t.Fatalf("a pin within the total bound failed: %s", record.failure)
		}
	}
	over := reserve()
	records.settle(over, worktree, many(1), false, "")
	if over.failure == "" || records.bindings > maxReviewDiffRecordBindings {
		t.Fatalf("a pin past the total bound = %q with %d bindings", over.failure, records.bindings)
	}

	stopReviewDiffRecords(records.removeAll()...)
	if records.len() != 0 || records.bindings != 0 {
		t.Fatal("removeAll kept records or bindings")
	}
	records.settle(over, worktree, many(1), false, "")
	if records.bindings != 0 {
		t.Fatal("a pin that settled after its record was removed published")
	}
}

// An edit image read always reads the snapshot row: an overwritten
// snapshot serves the new content, a cleared one is unavailable whatever
// the cache holds, and an unchanged one is decompressed once.
func TestEditImagesFollowTheirSnapshotRow(t *testing.T) {
	t.Parallel()
	app, base := reviewDiffFilesApp(t)
	ctx, _ := reviewDiffConn(t)
	threadID := editThread(t, app, "thread-edit-snapshot-row", "payload-row")
	put := func(content string) {
		t.Helper()
		if err := app.store.PutEditFileSnapshot(threadID, "payload-row", "diagram.svg", content, time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	read := func() ReviewDiffImageSide {
		t.Helper()
		got, err := app.GetEditDiffImage(ctx, threadID, EditDiffImageRequest{Path: "diagram.svg", EditPayloadID: "payload-row"})
		if err != nil {
			t.Fatal(err)
		}
		return got.New
	}
	contentID := func(side ReviewDiffImageSide) string {
		id, _, _ := strings.Cut(strings.TrimPrefix(side.URL, "/attachments/image/"), "?")
		return id
	}
	a := diagramSVG
	b := strings.Replace(diagramSVG, "alert(1)", "alert(2)", 1)
	put(a)
	first := read()
	expectSide(t, base, "A", first, []byte(a), "image/svg+xml")
	again := read()
	if contentID(again) != contentID(first) {
		t.Fatal("an unchanged snapshot was decompressed again")
	}
	put(b)
	expectSide(t, base, "B over A", read(), []byte(b), "image/svg+xml")

	thread, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	// Rewriting the payload clears its snapshots.
	if _, err := app.store.UpsertItem(store.Item{ID: "item-payload-row", ThreadID: thread.ID, Kind: "tool_call", Role: "assistant", Status: "completed", PayloadID: "payload-row", CreatedAt: now, UpdatedAt: now},
		&store.Payload{ID: "payload-row", Kind: "tool_result", Data: []byte("diff --git a/diagram.svg b/diagram.svg\n"), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != (ReviewDiffImageSide{Unavailable: "This edit's file content was not recorded."}) {
		t.Fatalf("a cleared snapshot = %+v, want unavailable", got)
	}
}
