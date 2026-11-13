package app

import (
	"container/list"
	"errors"
	"sync"

	"agent-overflow/internal/gitdiff"
	"github.com/google/uuid"
)

// The record store keeps what each git-backed review diff open pinned, so
// its file reads name only a handle and a path: the workspace, the two
// sides, and for a working-tree side the blob and size the open's snapshot
// recorded for each changed file. Nothing a client sends is an object ID.

// Bounds of the record store: at most maxReviewDiffRecords records holding
// at most maxReviewDiffRecordBindings working-tree bindings and
// maxReviewDiffRecordPathBytes of their paths in all; one record binds at
// most maxPinnedWorktreeBlobs files. The least recently used record goes
// first.
const (
	maxReviewDiffRecords         = 64
	maxPinnedWorktreeBlobs       = 5000
	maxReviewDiffRecordBindings  = 20000
	maxReviewDiffRecordPathBytes = 8 << 20
)

// errReviewDiffFilesGone answers a file read whose record is gone: evicted,
// cleared, or never made. Its text is the sentence the client shows.
var errReviewDiffFilesGone error = reviewDiffFilesGone{}

type reviewDiffFilesGone struct{}

func (reviewDiffFilesGone) Error() string {
	return "This diff's files are no longer held; reload the diff."
}

type reviewDiffRecord struct {
	id        string
	workspace string
	pin       gitdiff.Pin
	// blobs binds each changed regular file of a working-tree side to the
	// blob the open's snapshot recorded; nil for committed sides.
	blobs     map[string]gitdiff.WorktreeBlob
	truncated bool
	pathBytes int
	elem      *list.Element
}

type reviewDiffRecords struct {
	mu        sync.Mutex
	gen       uint64
	lru       *list.List // front is most recently used; values are *reviewDiffRecord
	byID      map[string]*reviewDiffRecord
	bindings  int
	pathBytes int
}

// generation answers the store's generation; a record pinned during an
// older one is not kept (put).
func (r *reviewDiffRecords) generation() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gen
}

// put keeps a record made in generation gen and answers its handle, or ""
// when the store was cleared since.
func (r *reviewDiffRecords) put(gen uint64, workspace string, pin gitdiff.Pin, blobs []gitdiff.WorktreeBlob, truncated bool) string {
	record := &reviewDiffRecord{id: uuid.NewString(), workspace: workspace, pin: pin, truncated: truncated}
	if pin.New == "" {
		record.blobs = make(map[string]gitdiff.WorktreeBlob, len(blobs))
		for _, blob := range blobs {
			record.blobs[blob.Path] = blob
			record.pathBytes += len(blob.Path)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if gen != r.gen {
		return ""
	}
	if r.byID == nil {
		r.byID = map[string]*reviewDiffRecord{}
		r.lru = list.New()
	}
	for r.lru.Len() > 0 && (r.lru.Len() >= maxReviewDiffRecords ||
		r.bindings+len(record.blobs) > maxReviewDiffRecordBindings ||
		r.pathBytes+record.pathBytes > maxReviewDiffRecordPathBytes) {
		r.removeLocked(r.lru.Back().Value.(*reviewDiffRecord))
	}
	record.elem = r.lru.PushFront(record)
	r.byID[record.id] = record
	r.bindings += len(record.blobs)
	r.pathBytes += record.pathBytes
	return record.id
}

// get answers the record under id, opened in workspace.
func (r *reviewDiffRecords) get(id, workspace string) (*reviewDiffRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.byID[id]
	if !ok {
		return nil, errReviewDiffFilesGone
	}
	if record.workspace != workspace {
		return nil, errors.New("this diff was opened in another workspace")
	}
	r.lru.MoveToFront(record.elem)
	return record, nil
}

// clear forgets every record; a pin in flight keeps nothing.
func (r *reviewDiffRecords) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen++
	r.byID = nil
	r.lru = nil
	r.bindings, r.pathBytes = 0, 0
}

func (r *reviewDiffRecords) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}

func (r *reviewDiffRecords) removeLocked(record *reviewDiffRecord) {
	r.lru.Remove(record.elem)
	delete(r.byID, record.id)
	r.bindings -= len(record.blobs)
	r.pathBytes -= record.pathBytes
}
