package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/transport"
)

// closingReviewDiff is a diff that counts its closes. With entered set,
// its first Close reports that it began and waits for release.
type closingReviewDiff struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	closes  atomic.Int32
	err     error
}

func (d *closingReviewDiff) Read(context.Context, int64, int) (gitdiff.Chunk, error) {
	return gitdiff.Chunk{}, errors.New("not read")
}

func (d *closingReviewDiff) Close() error {
	d.closes.Add(1)
	if d.entered != nil {
		d.once.Do(func() {
			close(d.entered)
			<-d.release
		})
	}
	return d.err
}

// Shutdown ends the git process and removes the snapshot of every diff
// still open, whether a connection holds it or an in-process caller does,
// and leaves nothing that would take another.
func TestShutdownClosesHeldReviewDiffs(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	ref, _ := largeWorkspace(t, app)
	ctx, _ := reviewDiffConn(t)
	for name, callCtx := range map[string]context.Context{"a connection": ctx, "an in-process caller": context.Background()} {
		opened, err := app.OpenWorkspaceDiff(callCtx, ref, false)
		if err != nil || opened.ID == "" {
			t.Fatalf("OpenWorkspaceDiff for %s: %+v, %v", name, opened.ID, err)
		}
	}
	if n := openReviewDiffCount(app); n != 2 {
		t.Fatalf("%d diffs held, want 2", n)
	}

	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if n := openReviewDiffCount(app); n != 0 {
		t.Fatalf("%d diffs held after shutdown", n)
	}
	if n := app.reviewDiffFiles.len(); n != 0 {
		t.Fatalf("%d file records held after shutdown", n)
	}
	if n := snapshotEntries(t, app); n != 0 {
		t.Fatalf("%d snapshots left after shutdown", n)
	}
	if record, _ := app.reviewDiffFiles.reserve("/w", nil, transport.ScopeFilesRead, func() {}); record != nil {
		t.Fatal("a file record was reserved after shutdown")
	}
	late := &closingReviewDiff{}
	if _, err := app.holdReviewDiff(context.Background(), late, transport.ScopeFilesRead); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("hold after shutdown: err = %v, want ErrShuttingDown", err)
	}
}

// An open that reaches the file records after shutdown removed them is
// refused and leaves no pin, diff or snapshot behind.
func TestOpenReviewDiffIsRefusedOnceItsRecordsAreClosed(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	ref, _ := largeWorkspace(t, app)
	ctx, _ := reviewDiffConn(t)
	app.closeReviewDiffRecords()
	if _, err := app.OpenWorkspaceDiff(ctx, ref, false); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("open after the records closed: err = %v, want ErrShuttingDown", err)
	}
	app.reviewDiffFiles.pins.Wait()
	if n := app.reviewDiffFiles.len(); n != 0 {
		t.Fatalf("%d file records held after a refused open", n)
	}
	if openReviewDiffCount(app) != 0 || snapshotEntries(t, app) != 0 {
		t.Fatal("the refused open left a diff or a snapshot")
	}
}

func TestCloseReviewDiffsReportsACloseFailureAndRefusesNewHandles(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	failure := errors.New("snapshot not removed")
	held := &closingReviewDiff{err: failure}
	if _, err := app.holdReviewDiff(context.Background(), held, transport.ScopeFilesRead); err != nil {
		t.Fatalf("holdReviewDiff: %v", err)
	}
	if err := app.closeReviewDiffs(); !errors.Is(err, failure) {
		t.Fatalf("closeReviewDiffs: err = %v, want the close's failure", err)
	}
	if n := held.closes.Load(); n != 1 {
		t.Fatalf("the held diff was closed %d times", n)
	}

	late := &closingReviewDiff{}
	if _, err := app.holdReviewDiff(context.Background(), late, transport.ScopeFilesRead); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("hold after shutdown closed the diffs: err = %v, want ErrShuttingDown", err)
	}
	if n := late.closes.Load(); n != 1 {
		t.Fatalf("the refused diff was closed %d times, want once", n)
	}
	if n := openReviewDiffCount(app); n != 0 {
		t.Fatalf("%d diffs held after a refused hold", n)
	}
	if err := app.closeReviewDiffs(); err != nil {
		t.Fatalf("closeReviewDiffs twice: %v", err)
	}
}

// A connection's teardown closes its diffs on a goroutine nothing else
// waits for; shutdown returns only once such a close has finished.
func TestCloseReviewDiffsWaitsForACloseAConnectionStarted(t *testing.T) {
	t.Parallel()
	app := newReviewDiffTestApp(t)
	ctx, conn := reviewDiffConn(t)
	diff := &closingReviewDiff{entered: make(chan struct{}), release: make(chan struct{})}
	if _, err := app.holdReviewDiff(ctx, diff, transport.ScopeFilesRead); err != nil {
		t.Fatalf("holdReviewDiff: %v", err)
	}
	conn.RunCleanups()
	select {
	case <-diff.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the connection's teardown never closed its diff")
	}

	done := make(chan error, 1)
	go func() { done <- app.closeReviewDiffs() }()
	select {
	case err := <-done:
		t.Fatalf("closeReviewDiffs returned %v while a close was running", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(diff.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("closeReviewDiffs: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("closeReviewDiffs did not return after the close finished")
	}
	if n := diff.closes.Load(); n != 1 {
		t.Fatalf("the diff was closed %d times", n)
	}
}
