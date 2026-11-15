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

	"agent-overflow/internal/fnv1a"
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
type ReviewDiffChunk struct {
	Data       string `json:"data"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"nextOffset"`
	EOF        bool   `json:"eof"`
	// Key is the key of the patch text [0, NextOffset). No key is zero
	// (the empty text's hash is fnv1a.OffsetBasis), so every chunk carries
	// one; omitzero makes the generated binding leave a chunk from an
	// older backend without a key instead of filling in a zero one.
	Key TextKey `json:"key,omitzero"`
}

// TextKey is the patch text from offset 0 to a chunk's end, as the
// frontend keys it: FNV-1a (32-bit) over the text's UTF-16 code units,
// and how many units that is. The text is what a JSON client receives for
// the chunks' bytes (fnv1a.FoldUTF16), the same whichever form a chunk is
// answered in.
type TextKey struct {
	Hash  uint32 `json:"hash"`
	Units int64  `json:"units"`
}

// emptyTextKey is the key of no text.
var emptyTextKey = TextKey{Hash: fnv1a.OffsetBasis}

// extend returns the key of k's text followed by text.
func (k TextKey) extend(text string) TextKey {
	hash, units := fnv1a.FoldUTF16(k.Hash, text)
	return TextKey{Hash: hash, Units: k.Units + int64(units)}
}

// emptyReviewDiffOpened answers an open of a diff that has no patch.
func emptyReviewDiffOpened() ReviewDiffOpened {
	return ReviewDiffOpened{Chunk: ReviewDiffChunk{EOF: true, Key: emptyTextKey}}
}

// errReviewDiffKey fails a read whose key cannot be answered from what
// the handle served. The diff's reader accepted the read, so it is a
// defect in a reader, never a client's mistake.
var errReviewDiffKey = errors.New("no key for the chunk's text")

// ReviewDiffOpened answers an Open*Diff call: the diff's first chunk, and
// the handle that reads the rest with ReadReviewDiff. ID is empty when the
// first chunk already reaches the end, in which case nothing is held.
type ReviewDiffOpened struct {
	ID    string          `json:"id"`
	Chunk ReviewDiffChunk `json:"chunk"`
	// PayloadIDs are the edit payloads an edits diff joins, in patch
	// order; GetPayloadPatchSpans returns each one's highlight spans.
	// Empty for every other diff.
	PayloadIDs []string `json:"payloadIds,omitempty"`
	// FilesHandle names what a git-backed open pinned for its file reads
	// (GetReviewDiffFileSizes, GetReviewDiffImage), held on the server for
	// the connection that opened it (app_review_diff_records.go). Every
	// git-backed open answers one; empty for an edits diff.
	FilesHandle string `json:"filesHandle,omitempty"`
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
// to the connection that opened it and is closed when that connection
// ends; shutdown closes the rest and joins every close (closeReviewDiffs).
type appReviewDiffState struct {
	mu    sync.Mutex
	diffs map[string]*openReviewDiff
	// closed refuses new handles once shutdown has closed the held ones.
	closed bool
	// closing counts the closes in progress. A close is added under mu
	// while its diff is still in diffs, so none starts after shutdown has
	// emptied the map.
	closing sync.WaitGroup
}

type openReviewDiff struct {
	diff  reviewDiffReader
	owner *transport.ConnState
	// scope is the one the Open method required. Reading the handle
	// requires it again, so a session narrowed after the Open stops
	// reading.
	scope transport.Scope

	mu sync.Mutex
	// keys holds the key at the end of every chunk served, by its
	// NextOffset. A read starts at 0 or at one of those (reviewDiffReader),
	// so its start key is here.
	keys map[int64]TextKey
}

// newOpenReviewDiff is diff, not yet held, owned by ctx's connection.
func newOpenReviewDiff(ctx context.Context, diff reviewDiffReader, scope transport.Scope) *openReviewDiff {
	return &openReviewDiff{diff: diff, owner: transport.ConnStateFromContext(ctx), scope: scope}
}

// read reads a chunk and answers it with its key, recording the key at
// the chunk's end for the read that starts there. Reads may run
// concurrently. A re-read answers the key it answered first, and a chunk
// whose start has no recorded key fails rather than answer a key folded
// from a wrong start.
func (o *openReviewDiff) read(ctx context.Context, offset int64, maxBytes int) (ReviewDiffChunk, error) {
	chunk, err := o.diff.Read(ctx, offset, maxBytes)
	if err != nil {
		return ReviewDiffChunk{}, err
	}
	if chunk.Offset != offset || chunk.NextOffset != chunk.Offset+int64(len(chunk.Data)) {
		return ReviewDiffChunk{}, fmt.Errorf("%w: a read at %d answered %d bytes spanning [%d, %d)", errReviewDiffKey, offset, len(chunk.Data), chunk.Offset, chunk.NextOffset)
	}
	start, ok := o.keyAt(offset)
	if !ok {
		return ReviewDiffChunk{}, fmt.Errorf("%w: nothing served ends at offset %d", errReviewDiffKey, offset)
	}
	end := start.extend(chunk.Data)
	o.mu.Lock()
	prev, seen := o.keys[chunk.NextOffset]
	if !seen {
		if o.keys == nil {
			o.keys = make(map[int64]TextKey)
		}
		o.keys[chunk.NextOffset] = end
	}
	o.mu.Unlock()
	if seen && prev != end {
		return ReviewDiffChunk{}, fmt.Errorf("%w: the text up to offset %d differs from what was served", errReviewDiffKey, chunk.NextOffset)
	}
	return ReviewDiffChunk{Data: chunk.Data, Offset: chunk.Offset, NextOffset: chunk.NextOffset, EOF: chunk.EOF, Key: end}, nil
}

// keyAt returns the key of the text before offset.
func (o *openReviewDiff) keyAt(offset int64) (TextKey, bool) {
	if offset == 0 {
		return emptyTextKey, true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	key, ok := o.keys[offset]
	return key, ok
}

// openReviewDiff opens the patch between the sides open resolves, returns
// its first chunk and a files handle at once, and keeps the diff open under
// a connection-owned id when there is more to read. The handle's record is
// pinned in the background (startReviewDiffPin); the open never waits for
// it. scope is the calling Open method's //ao:scope.
func (a *App) openReviewDiff(ctx context.Context, action string, scope transport.Scope, opts gitdiff.Options, open func(tempRoot string) (*gitdiff.Sides, error)) (ReviewDiffOpened, error) {
	tempRoot, err := a.reviewDiffSnapshotRoot()
	if err != nil {
		return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, err)
	}
	sides, err := open(tempRoot)
	if err != nil {
		return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, err)
	}
	// The pin holds the snapshot until it exits, whenever the diff closes.
	releasePin := sides.Hold()
	diff := sides.OpenDiff(opts)
	owner := transport.ConnStateFromContext(ctx)
	pinCtx, cancelPin := context.WithTimeout(context.Background(), a.reviewDiffPinWait())
	record, evicted := a.reviewDiffFiles.reserve(sides.Workspace(), owner, scope, cancelPin)
	if record == nil {
		cancelPin()
		return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, errors.Join(ErrShuttingDown, diff.Close(), releasePin()))
	}
	a.startReviewDiffPin(pinCtx, record, sides, releasePin)
	stopReviewDiffRecords(evicted...)
	if owner != nil && !owner.BindCleanup(reviewDiffFilesCleanupKey(record.id), func() { a.dropReviewDiffRecord(record.id) }) {
		a.dropReviewDiffRecord(record.id)
		return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, errors.Join(errors.New("connection closing"), diff.Close()))
	}
	opened, err := a.holdOpenedReviewDiff(ctx, action, scope, diff)
	if err != nil {
		owner.UnbindCleanup(reviewDiffFilesCleanupKey(record.id))
		a.dropReviewDiffRecord(record.id)
		return ReviewDiffOpened{}, err
	}
	opened.FilesHandle = record.id
	return opened, nil
}

// holdOpenedReviewDiff reads an open diff's first chunk and, when there is
// more, keeps the diff under a handle. The diff is closed otherwise.
func (a *App) holdOpenedReviewDiff(ctx context.Context, action string, scope transport.Scope, diff reviewDiffReader) (ReviewDiffOpened, error) {
	open := newOpenReviewDiff(ctx, diff, scope)
	chunk, err := open.read(ctx, 0, reviewDiffFirstChunkBytes)
	if err != nil || chunk.EOF {
		closeErr := diff.Close()
		if err != nil {
			return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, errors.Join(err, closeErr))
		}
		logReviewDiffCloseError(closeErr)
		return ReviewDiffOpened{Chunk: chunk}, nil
	}
	id, err := a.holdReviewDiff(open)
	if err != nil {
		return ReviewDiffOpened{}, fmt.Errorf("%s: %w", action, err)
	}
	return ReviewDiffOpened{ID: id, Chunk: chunk}, nil
}

// holdReviewDiff registers open under a new handle owned by its
// connection. On failure the diff is closed.
func (a *App) holdReviewDiff(open *openReviewDiff) (string, error) {
	state := &a.reviewDiffs
	owner := open.owner
	id := uuid.NewString()
	state.mu.Lock()
	if state.closed {
		state.mu.Unlock()
		return "", errors.Join(ErrShuttingDown, open.diff.Close())
	}
	if len(state.diffs) >= maxReviewDiffHandles {
		state.mu.Unlock()
		return "", errors.Join(ErrTooManyReviewDiffs, open.diff.Close())
	}
	if state.diffs == nil {
		state.diffs = make(map[string]*openReviewDiff)
	}
	state.diffs[id] = open
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
//ao:callerbound
func (a *App) ReadReviewDiff(ctx context.Context, id string, offset int64, maxBytes int) (ReviewDiffChunk, error) {
	return a.readReviewDiff(ctx, id, offset, maxBytes)
}

// ReviewDiffPackedChunk is a ReviewDiffChunk whose text is packed where
// gitdiff.PackText packs it. Offset, NextOffset and EOF are the raw
// chunk's, so len(Data) in characters differs from NextOffset-Offset when
// the chunk had invalid UTF-8.
type ReviewDiffPackedChunk struct {
	// Data is the chunk's text: as UTF-8, one byte per character
	// (U+0000..U+00FF), when Packed, and otherwise the text itself, as
	// ReadReviewDiff answers it.
	Data string `json:"data"`
	// Packed depends on the chunk's bytes alone: a chunk read again has
	// the form it had.
	Packed bool `json:"packed"`
	// Wide holds, for each line of packed Data that has a byte past
	// ASCII, the index in Data of the first such byte, ascending. A line
	// here is a run of Data between newlines, so the piece of a line that
	// began in an earlier chunk counts as one. Never null; empty when
	// !Packed.
	Wide       []uint32 `json:"wide"`
	Offset     int64    `json:"offset"`
	NextOffset int64    `json:"nextOffset"`
	EOF        bool     `json:"eof"`
	// Key is the plain chunk's: the key of the text, not of its form.
	Key TextKey `json:"key,omitzero"`
}

// ReadReviewDiffPacked is ReadReviewDiff with the chunk's text packed
// where that is its cheaper form for the client (gitdiff.PackText): the
// same chunk, handle, owner and scope rules. A client calls it only on a
// backend that advertises transport.CapabilityReviewDiffPacked.
//
//ao:scope session
//ao:route home
//ao:callerbound
func (a *App) ReadReviewDiffPacked(ctx context.Context, id string, offset int64, maxBytes int) (ReviewDiffPackedChunk, error) {
	chunk, err := a.readReviewDiff(ctx, id, offset, maxBytes)
	if err != nil {
		return ReviewDiffPackedChunk{}, err
	}
	data, wide, packed := gitdiff.PackText(chunk.Data)
	return ReviewDiffPackedChunk{Data: data, Packed: packed, Wide: wide, Offset: chunk.Offset, NextOffset: chunk.NextOffset, EOF: chunk.EOF, Key: chunk.Key}, nil
}

// readReviewDiff reads a chunk of a diff the calling connection holds,
// under the scope its Open required.
func (a *App) readReviewDiff(ctx context.Context, id string, offset int64, maxBytes int) (ReviewDiffChunk, error) {
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
	chunk, err := open.read(ctx, offset, maxBytes)
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
	if !ok {
		state.mu.Unlock()
		return nil
	}
	delete(state.diffs, id)
	state.closing.Add(1)
	state.mu.Unlock()
	defer state.closing.Done()
	return open.diff.Close()
}

// closeReviewDiffs closes every held review diff, waits for the closes
// their connections started, and refuses handles from then on: shutdown.
// It returns what the closes it ran reported; a connection's close logs
// its own.
func (a *App) closeReviewDiffs() error {
	state := &a.reviewDiffs
	state.mu.Lock()
	state.closed = true
	held := state.diffs
	state.diffs = nil
	state.mu.Unlock()
	var errs []error
	for _, open := range held {
		errs = append(errs, open.diff.Close())
	}
	state.closing.Wait()
	return errors.Join(errs...)
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
