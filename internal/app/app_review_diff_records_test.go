package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	var full []*reviewDiffRecord
	for range maxReviewDiffRecordBindings / maxPinnedWorktreeBlobs {
		record := reserve()
		stopReviewDiffRecords(records.settle(record, worktree, many(maxPinnedWorktreeBlobs), false, "")...)
		if record.failure != "" {
			t.Fatalf("a pin within the total bound failed: %s", record.failure)
		}
		full = append(full, record)
	}
	over := reserve()
	stopReviewDiffRecords(records.settle(over, worktree, many(1), false, "")...)
	if over.failure != "" || records.bindings > maxReviewDiffRecordBindings || records.holds(full[0]) || !records.holds(over) {
		t.Fatalf("a pin past the total bound = %q with %d bindings; the oldest held: %v", over.failure, records.bindings, records.holds(full[0]))
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

// manyPaths answers n working-tree bindings whose names are width bytes.
func manyPaths(prefix string, n, width int) []gitdiff.WorktreeBlob {
	blobs := make([]gitdiff.WorktreeBlob, n)
	for i := range blobs {
		name := fmt.Sprintf("%s%d-", prefix, i)
		blobs[i] = gitdiff.WorktreeBlob{Path: name + strings.Repeat("x", max(0, width-len(name))), Mode: "100644"}
	}
	return blobs
}

func reserveSettled(t *testing.T, records *reviewDiffRecords, blobs []gitdiff.WorktreeBlob) *reviewDiffRecord {
	t.Helper()
	record, evicted := records.reserve("/w", nil, transport.ScopeFilesRead, func() {})
	stopReviewDiffRecords(evicted...)
	victims := records.settle(record, gitdiff.Pin{Old: strings.Repeat("a", 40)}, blobs, false, "")
	close(record.done)
	stopReviewDiffRecords(victims...)
	return record
}

// The reported sequence: one connection opens a 1,000-path diff 21 times.
// The 21st pins and reads; the oldest record made room and answers that
// its files are no longer held.
func TestTheBindingBudgetEvictsTheOldestRecords(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	repo := testutil.InitGitRepo(t)
	for i := range 1000 {
		writeRepoBytes(t, repo, fmt.Sprintf("f%04d.txt", i), []byte{byte(i)})
	}
	ref := testWorkspaceRef(t, app, repo)
	ctx, _ := reviewDiffConn(t)
	var handles []string
	for range 21 {
		opened, err := app.OpenWorkspaceDiff(ctx, ref, false)
		handle := filesHandle(t, opened, err)
		if _, err := app.GetReviewDiffFileSizes(ctx, ref, handle, []ReviewDiffFile{{Path: "f0000.txt"}}); err != nil {
			t.Fatalf("open %d: %v", len(handles)+1, err)
		}
		handles = append(handles, handle)
	}
	sizes, err := app.GetReviewDiffFileSizes(ctx, ref, handles[20], []ReviewDiffFile{{Path: "f0999.txt"}})
	if err != nil || sizes[0].New != (ReviewDiffFileSide{true, 1}) {
		t.Fatalf("the 21st diff's files = %+v, %v", sizes, err)
	}
	_, err = app.GetReviewDiffFileSizes(ctx, ref, handles[0], []ReviewDiffFile{{Path: "f0000.txt"}})
	wantCode(t, err, reviewDiffFilesGoneCode, "This diff's files are no longer held; reload the diff.")
	if _, err := app.GetReviewDiffFileSizes(ctx, ref, handles[1], []ReviewDiffFile{{Path: "f0000.txt"}}); err != nil {
		t.Fatalf("the second diff's files went too: %v", err)
	}
	if app.reviewDiffFiles.bindings > maxReviewDiffRecordBindings {
		t.Fatalf("%d bindings held", app.reviewDiffFiles.bindings)
	}
}

// The name budget makes room the same way; only a record whose names pass
// it on their own fails.
func TestTheNameBudgetEvictsTheOldestRecords(t *testing.T) {
	t.Parallel()
	var records reviewDiffRecords
	width := 2000
	perRecord := (maxReviewDiffRecordPathBytes / width) / 4 // four records fill it
	var held []*reviewDiffRecord
	for i := range 5 {
		record := reserveSettled(t, &records, manyPaths(fmt.Sprintf("r%d-", i), perRecord, width))
		if record.failure != "" {
			t.Fatalf("record %d failed: %s", i, record.failure)
		}
		held = append(held, record)
	}
	if records.holds(held[0]) || !records.holds(held[1]) || !records.holds(held[4]) || records.pathBytes > maxReviewDiffRecordPathBytes {
		t.Fatalf("held %v %v %v with %d name bytes; want the oldest out", records.holds(held[0]), records.holds(held[1]), records.holds(held[4]), records.pathBytes)
	}
	alone := reserveSettled(t, &records, manyPaths("big", maxPinnedWorktreeBlobs, width))
	if alone.failure != fmt.Sprintf("its file names exceed %d MiB", maxReviewDiffRecordPathBytes>>20) {
		t.Fatalf("a record past the name budget on its own = %q", alone.failure)
	}
	if !records.holds(held[4]) {
		t.Fatal("a record that failed on its own evicted others")
	}
	stopReviewDiffRecords(records.removeAll()...)
	if records.bindings != 0 || records.pathBytes != 0 {
		t.Fatalf("totals after removing everything: %d bindings, %d name bytes", records.bindings, records.pathBytes)
	}
}

// A pending record that a settling pin evicts to make room has its pin
// cancelled and joined: no goroutine, git process or snapshot directory is
// left.
func TestEvictingAPendingRecordStopsItsPin(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	repo := testutil.InitGitRepo(t)
	writeRepoBytes(t, repo, "fresh.png", sizedPNG(t, 2, 2))
	ref := testWorkspaceRef(t, app, repo)
	ctx, _ := reviewDiffConn(t)
	app.reviewDiffPinTimeout = time.Hour
	held := make(chan struct{})
	var calls atomic.Int32
	app.reviewDiffPinHook = func(ctx context.Context) {
		if calls.Add(1) == 1 {
			close(held)
			<-ctx.Done()
		}
	}
	opened, err := app.OpenWorkspaceDiff(ctx, ref, false)
	pending := filesHandle(t, opened, err)
	<-held
	if n := snapshotDirs(t, app); n != 1 {
		t.Fatalf("%d snapshots while the pin is held", n)
	}
	for range maxReviewDiffRecordBindings / maxPinnedWorktreeBlobs {
		reserveSettled(t, &app.reviewDiffFiles, manyPaths("p", maxPinnedWorktreeBlobs, 8))
	}
	opened, err = app.OpenWorkspaceDiff(ctx, ref, false)
	settled := filesHandle(t, opened, err)
	done := make(chan struct{})
	go func() { app.reviewDiffFiles.pins.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the evicted pin outlived its eviction")
	}
	if n := snapshotDirs(t, app); n != 0 {
		t.Fatalf("%d snapshots after the pending record was evicted", n)
	}
	_, err = app.GetReviewDiffFileSizes(ctx, ref, pending, []ReviewDiffFile{{Path: "fresh.png"}})
	wantCode(t, err, reviewDiffFilesGoneCode, "This diff's files are no longer held; reload the diff.")
	if _, err := app.GetReviewDiffFileSizes(ctx, ref, settled, []ReviewDiffFile{{Path: "fresh.png"}}); err != nil {
		t.Fatalf("the pin that made room: %v", err)
	}
}

// A settling record makes room from the others; it never evicts itself,
// even when it is the least recently used.
func TestASettlingRecordNeverEvictsItself(t *testing.T) {
	t.Parallel()
	var records reviewDiffRecords
	oldest, evicted := records.reserve("/w", nil, transport.ScopeFilesRead, func() {})
	stopReviewDiffRecords(evicted...)
	close(oldest.done)
	var newer []*reviewDiffRecord
	for range maxReviewDiffRecordBindings / maxPinnedWorktreeBlobs {
		newer = append(newer, reserveSettled(t, &records, manyPaths("n", maxPinnedWorktreeBlobs, 8)))
	}
	stopReviewDiffRecords(records.settle(oldest, gitdiff.Pin{Old: strings.Repeat("a", 40)}, manyPaths("o", 1, 8), false, "")...)
	if !records.holds(oldest) || records.holds(newer[0]) || !records.holds(newer[1]) || records.bindings != maxReviewDiffRecordBindings-maxPinnedWorktreeBlobs+1 {
		t.Fatalf("held oldest %v, next %v, then %v with %d bindings", records.holds(oldest), records.holds(newer[0]), records.holds(newer[1]), records.bindings)
	}
	stopReviewDiffRecords(records.removeAll()...)
	if records.bindings != 0 || records.pathBytes != 0 {
		t.Fatalf("totals after removing everything: %d bindings, %d name bytes", records.bindings, records.pathBytes)
	}
}

// A read in progress when its record is evicted refuses cleanly; it never
// reads another record's bindings.
func TestAReadWhoseRecordIsEvictedRefuses(t *testing.T) {
	t.Parallel()
	app, _ := reviewDiffFilesApp(t)
	repo := testutil.InitGitRepo(t)
	writeRepoBytes(t, repo, "fresh.png", sizedPNG(t, 2, 2))
	ref := testWorkspaceRef(t, app, repo)
	ctx, _ := reviewDiffConn(t)
	opened, err := app.OpenWorkspaceDiff(ctx, ref, false)
	handle := filesHandle(t, opened, err)
	if _, err := app.GetReviewDiffFileSizes(ctx, ref, handle, []ReviewDiffFile{{Path: "fresh.png"}}); err != nil {
		t.Fatal(err)
	}
	reached, proceed := make(chan struct{}), make(chan struct{})
	app.reviewDiffReadHook = func() {
		close(reached)
		<-proceed
	}
	read := make(chan error, 1)
	go func() {
		_, err := app.GetReviewDiffImage(ctx, ref, handle, ReviewDiffFile{Path: "fresh.png"})
		read <- err
	}()
	<-reached
	app.reviewDiffReadHook = nil
	// Others take every path, with a record binding the same path.
	for range maxReviewDiffRecordBindings / maxPinnedWorktreeBlobs {
		reserveSettled(t, &app.reviewDiffFiles, append(manyPaths("q", maxPinnedWorktreeBlobs-1, 8), gitdiff.WorktreeBlob{Path: "fresh.png", Mode: "100644", Blob: strings.Repeat("e", 40), Size: 9}))
	}
	close(proceed)
	wantCode(t, <-read, reviewDiffFilesGoneCode, "This diff's files are no longer held; reload the diff.")
}

// Pins settling at once never take the store past its bounds and never
// deadlock; the totals return to zero once everything is removed.
func TestConcurrentSettlesStayWithinTheBounds(t *testing.T) {
	t.Parallel()
	var records reviewDiffRecords
	var over atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for g := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range 20 {
					reserveSettled(t, &records, manyPaths(fmt.Sprintf("g%d-%d-", g, i), 3000, 16))
					records.mu.Lock()
					if records.bindings > maxReviewDiffRecordBindings || records.pathBytes > maxReviewDiffRecordPathBytes {
						over.Add(1)
					}
					records.mu.Unlock()
				}
			}()
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent settles deadlocked")
	}
	if over.Load() != 0 {
		t.Fatalf("the bounds were passed %d times", over.Load())
	}
	stopReviewDiffRecords(records.removeAll()...)
	if records.len() != 0 || records.bindings != 0 || records.pathBytes != 0 {
		t.Fatalf("totals after removing everything: %d records, %d bindings, %d name bytes", records.len(), records.bindings, records.pathBytes)
	}
}
