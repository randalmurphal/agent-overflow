package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"

	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/transport"
)

// maxReviewDiffHandles bounds the review diffs held open across the app.
// Each can hold a git process and a worktree snapshot on disk, so an
// unbounded map is a resource-exhaustion surface for a caller that opens in
// a loop and never releases. Review panes and gate diffs are counted in
// single digits, and a diff read to the end in its first chunk holds no
// handle at all.
const maxReviewDiffHandles = 256

// reviewDiffFirstChunkBytes is the part of a diff an Open* call returns
// with the handle: most diffs fit, so they cost one call and hold nothing.
const reviewDiffFirstChunkBytes = 1 << 20

// reviewDiffSnapshotDir is the directory under the data root that holds
// worktree diff snapshots. The backend lock makes this process its only
// user, so boot removes whatever an earlier process left.
const reviewDiffSnapshotDir = "review-diffs"

// ErrTooManyReviewDiffs is returned once maxReviewDiffHandles diffs are open.
var ErrTooManyReviewDiffs = errors.New("review diff: too many open diffs")

// ErrReviewDiffNotFound answers a read or release of a handle this
// connection does not hold: never opened, already released, or released
// with the connection that opened it.
var ErrReviewDiffNotFound = errors.New("review diff: not open")

// ReviewDiffChunk is patch bytes [Offset, NextOffset) of an open review
// diff; EOF marks the end of the patch.
type ReviewDiffChunk = gitdiff.Chunk

// ReviewDiffOpened answers an Open*Diff call: the diff's first chunk, and
// the handle that reads the rest with ReadReviewDiff. ID is empty when the
// first chunk already reaches the end, in which case nothing is held.
type ReviewDiffOpened struct {
	ID    string          `json:"id"`
	Chunk ReviewDiffChunk `json:"chunk"`
	// HeadSHA is the commit a pull request's diff was computed at, which
	// the fetch can move past the head the caller last saw. Empty for
	// every other diff.
	HeadSHA string `json:"headSha,omitempty"`
	// PayloadIDs are the edit payloads an edits diff joins, in patch
	// order; GetPayloadPatchSpans returns each one's highlight spans.
	// Empty for every other diff.
	PayloadIDs []string `json:"payloadIds,omitempty"`
}

// reviewDiffReader is an open review diff: a git diff or a thread's edit
// payloads. Read keeps gitdiff.Diff.Read's contract: offset is 0 or a
// NextOffset an earlier read returned, chunks end where gitdiff.ChunkCut
// puts them, and re-reads that no longer match what was served fail with
// an error that says the diff changed since it was opened.
type reviewDiffReader interface {
	Read(ctx context.Context, offset int64, maxBytes int) (gitdiff.Chunk, error)
	Close() error
}

// appReviewDiffState holds the review diffs clients have open. Each belongs
// to the connection that opened it and is closed when that connection ends.
type appReviewDiffState struct {
	mu    sync.Mutex
	diffs map[string]*openReviewDiff
}

type openReviewDiff struct {
	diff  reviewDiffReader
	owner *transport.ConnState
	// scope is the one the Open method required. Reading the handle
	// requires it again, so a session narrowed after the Open stops
	// reading.
	scope transport.Scope
}

// openReviewDiff opens a git diff, returns its first chunk, and keeps it
// open under a connection-owned handle when there is more to read. scope
// is the calling Open method's //ao:scope.
func (a *App) openReviewDiff(ctx context.Context, action string, scope transport.Scope, open func(tempRoot string) (*gitdiff.Diff, error)) (ReviewDiffOpened, error) {
	tempRoot, err := a.reviewDiffSnapshotRoot()
	if err != nil {
		return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, err)
	}
	diff, err := open(tempRoot)
	if err != nil {
		return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, err)
	}
	return a.holdOpenedReviewDiff(ctx, action, scope, diff)
}

// holdOpenedReviewDiff reads an open diff's first chunk and, when there is
// more, keeps the diff under a handle. The diff is closed otherwise.
func (a *App) holdOpenedReviewDiff(ctx context.Context, action string, scope transport.Scope, diff reviewDiffReader) (ReviewDiffOpened, error) {
	chunk, err := diff.Read(ctx, 0, reviewDiffFirstChunkBytes)
	if err != nil || chunk.EOF {
		closeErr := diff.Close()
		if err != nil {
			return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, errors.Join(err, closeErr))
		}
		logReviewDiffCloseError(closeErr)
		return ReviewDiffOpened{Chunk: chunk}, nil
	}
	id, err := a.holdReviewDiff(ctx, diff, scope)
	if err != nil {
		return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, err)
	}
	return ReviewDiffOpened{ID: id, Chunk: chunk}, nil
}

// holdReviewDiff registers diff under a new handle owned by the calling
// connection. On failure the diff is closed.
func (a *App) holdReviewDiff(ctx context.Context, diff reviewDiffReader, scope transport.Scope) (string, error) {
	state := &a.reviewDiffs
	owner := transport.ConnStateFromContext(ctx)
	id := uuid.NewString()
	state.mu.Lock()
	if len(state.diffs) >= maxReviewDiffHandles {
		state.mu.Unlock()
		return "", errors.Join(ErrTooManyReviewDiffs, diff.Close())
	}
	if state.diffs == nil {
		state.diffs = make(map[string]*openReviewDiff)
	}
	state.diffs[id] = &openReviewDiff{diff: diff, owner: owner, scope: scope}
	state.mu.Unlock()
	// Closing removes the snapshot directory, which connection teardown
	// should not wait on.
	release := func() { go func() { logReviewDiffCloseError(a.dropReviewDiff(id)) }() }
	if owner != nil && !owner.BindCleanup(reviewDiffCleanupKey(id), release) {
		return "", errors.Join(errors.New("connection closing"), a.dropReviewDiff(id))
	}
	return id, nil
}

// ReadReviewDiff returns up to maxBytes of an open review diff from offset,
// which is 0 or a NextOffset an earlier chunk of this diff returned. A
// chunk ends at a line boundary unless one line is longer than the chunk.
// The handle stays open at the end of the patch, so earlier chunks can be
// read again until ReleaseReviewDiff.
//
// Only the connection that opened the diff can read it; the handle means
// nothing on another connection or another computer, so callers pin the
// computer that opened it. The method's floor is any session because the
// handle decides the authority: a read requires the scope its Open
// required.
//
//ao:scope session
//ao:route home
func (a *App) ReadReviewDiff(ctx context.Context, id string, offset int64, maxBytes int) (ReviewDiffChunk, error) {
	if a.shuttingDown.Load() {
		return ReviewDiffChunk{}, ErrShuttingDown
	}
	open, err := a.ownedReviewDiff(ctx, id)
	if err != nil {
		return ReviewDiffChunk{}, err
	}
	if err := a.requireScope(ctx, open.scope, "reading a review diff"); err != nil {
		return ReviewDiffChunk{}, err
	}
	chunk, err := open.diff.Read(ctx, offset, maxBytes)
	if errors.Is(err, gitdiff.ErrDiffClosed) {
		// Released while this read ran.
		return ReviewDiffChunk{}, ErrReviewDiffNotFound
	}
	if err != nil {
		return ReviewDiffChunk{}, fmt.Errorf("read review diff: %w", err)
	}
	return chunk, nil
}

// ReleaseReviewDiff closes an open review diff: its git process ends and
// its worktree snapshot is removed. Releasing a handle the connection no
// longer holds is a no-op, because the connection's own cleanup may have
// released it first. Any session may release what its connection holds.
//
//ao:scope session
//ao:route home
func (a *App) ReleaseReviewDiff(ctx context.Context, id string) error {
	if _, err := a.ownedReviewDiff(ctx, id); err != nil {
		return nil
	}
	transport.ConnStateFromContext(ctx).UnbindCleanup(reviewDiffCleanupKey(id))
	if err := a.dropReviewDiff(id); err != nil {
		return fmt.Errorf("release review diff: %w", err)
	}
	return nil
}

func (a *App) ownedReviewDiff(ctx context.Context, id string) (*openReviewDiff, error) {
	state := &a.reviewDiffs
	state.mu.Lock()
	defer state.mu.Unlock()
	open, ok := state.diffs[id]
	if !ok || open.owner != transport.ConnStateFromContext(ctx) {
		return nil, ErrReviewDiffNotFound
	}
	return open, nil
}

// dropReviewDiff forgets and closes one diff.
func (a *App) dropReviewDiff(id string) error {
	state := &a.reviewDiffs
	state.mu.Lock()
	open, ok := state.diffs[id]
	delete(state.diffs, id)
	state.mu.Unlock()
	if !ok {
		return nil
	}
	return open.diff.Close()
}

func reviewDiffCleanupKey(id string) string { return "review-diff:" + id }

// logReviewDiffCloseError records a failed snapshot removal that no caller
// is waiting on. Boot removes the directory the next time either way.
func logReviewDiffCloseError(err error) {
	if err != nil {
		log.Printf("review diff: close: %v", err)
	}
}

// reviewDiffSnapshotRoot returns the directory worktree diff snapshots are
// created in, creating it when needed.
func (a *App) reviewDiffSnapshotRoot() (string, error) {
	if a.configDir == "" {
		return "", errors.New("the app data directory is not set")
	}
	root := filepath.Join(a.configDir, reviewDiffSnapshotDir)
	if err := ensureAppPrivateDir(root); err != nil {
		return "", fmt.Errorf("create review diff snapshot directory: %w", err)
	}
	return root, nil
}

// sweepReviewDiffSnapshots removes the snapshots an earlier process left
// when it exited without closing its diffs. Runs at boot, before any
// client can open one.
func (a *App) sweepReviewDiffSnapshots() error {
	if err := os.RemoveAll(filepath.Join(a.configDir, reviewDiffSnapshotDir)); err != nil {
		return fmt.Errorf("remove review diff snapshots from the last run: %w", err)
	}
	return nil
}
