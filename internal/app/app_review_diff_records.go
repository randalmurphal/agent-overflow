package app

import (
	"container/list"
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/gitdiff"
	"agent-overflow/internal/transport"
)

// The record store keeps what each git-backed review diff open pinned, so
// its file reads name only a handle and a path: the workspace, the two
// sides, and for a working-tree side what the open's snapshot recorded for
// each changed path. A record is as strict as a diff id: it belongs to the
// connection that opened it, a read rechecks the scope the open required,
// and the owner's teardown removes it. The open reserves a record and
// returns its handle at once; pinning settles it in the background, and a
// file read waits for that.

// Bounds of the record store: at most maxReviewDiffRecords records, the
// least recently used first out, counted from reservation; a settling pin
// binds at most maxPinnedWorktreeBlobs paths, and all records together at
// most maxReviewDiffRecordBindings paths and maxReviewDiffRecordPathBytes
// of their names; a pin past either fails with a sentence.
const (
	maxReviewDiffRecords         = 64
	maxPinnedWorktreeBlobs       = 5000
	maxReviewDiffRecordBindings  = 20000
	maxReviewDiffRecordPathBytes = 8 << 20
)

// The file reads' refusals, each with its own code: a handle this caller
// does not hold (evicted, cleared, released with its connection, another
// connection's, or never made), and a record whose pin failed, which
// reopening the same diff would not change.
const (
	reviewDiffFilesGoneCode       = "review_diff_files_gone"
	reviewDiffFilesUnreadableCode = "review_diff_files_unreadable"
)

var errReviewDiffFilesGone = errorsx.Public(reviewDiffFilesGoneCode, "This diff's files are no longer held; reload the diff.", nil)

func reviewDiffFilesUnreadable(why string) error {
	return errorsx.Public(reviewDiffFilesUnreadableCode, "This diff's files cannot be read: "+why+".", nil)
}

type reviewDiffRecord struct {
	id        string
	workspace string
	owner     *transport.ConnState
	// scope is the one the Open method required; every read requires it
	// again.
	scope transport.Scope
	// cancel stops the pin; done closes when it has exited, after it
	// let go of the snapshot.
	cancel context.CancelFunc
	done   chan struct{}

	// Set by settle, under the store's lock, before done closes.
	pin gitdiff.Pin
	// blobs binds each path a working-tree side changes; nil for committed
	// sides.
	blobs     map[string]gitdiff.WorktreeBlob
	failure   string
	pathBytes int
	elem      *list.Element
}

type reviewDiffRecords struct {
	mu        sync.Mutex
	lru       *list.List // front is most recently used; values are *reviewDiffRecord
	byID      map[string]*reviewDiffRecord
	bindings  int
	pathBytes int
	// pins counts pin goroutines; shutdown joins them.
	pins sync.WaitGroup
}

// reserve makes a pending record; the caller starts its pin, which must
// close done. evicted are the records it pushed out, still to be stopped
// outside the lock.
func (r *reviewDiffRecords) reserve(workspace string, owner *transport.ConnState, scope transport.Scope, cancel context.CancelFunc) (record *reviewDiffRecord, evicted []*reviewDiffRecord) {
	record = &reviewDiffRecord{id: uuid.NewString(), workspace: workspace, owner: owner, scope: scope, cancel: cancel, done: make(chan struct{})}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byID == nil {
		r.byID = map[string]*reviewDiffRecord{}
		r.lru = list.New()
	}
	for r.lru.Len() >= maxReviewDiffRecords {
		evicted = append(evicted, r.removeLocked(r.lru.Back().Value.(*reviewDiffRecord)))
	}
	record.elem = r.lru.PushFront(record)
	r.byID[record.id] = record
	return record, evicted
}

// settle publishes a pin's outcome on its record, unless the record was
// removed meanwhile: then it publishes nothing.
func (r *reviewDiffRecords) settle(record *reviewDiffRecord, pin gitdiff.Pin, blobs []gitdiff.WorktreeBlob, truncated bool, failure string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byID[record.id] != record {
		return
	}
	if failure == "" && truncated {
		failure = fmt.Sprintf("it changes more than %d files", maxPinnedWorktreeBlobs)
	}
	pathBytes := 0
	for _, blob := range blobs {
		pathBytes += len(blob.Path)
	}
	if failure == "" && (r.bindings+len(blobs) > maxReviewDiffRecordBindings || r.pathBytes+pathBytes > maxReviewDiffRecordPathBytes) {
		failure = "the open review diffs hold too many files; close one and reload this diff"
	}
	if failure != "" {
		record.failure = failure
		return
	}
	record.pin = pin
	if pin.New == "" {
		record.blobs = make(map[string]gitdiff.WorktreeBlob, len(blobs))
		for _, blob := range blobs {
			record.blobs[blob.Path] = blob
		}
		record.pathBytes = pathBytes
		r.bindings += len(blobs)
		r.pathBytes += pathBytes
	}
}

// get answers the record under id when owner holds it and it was opened
// in workspace; any other answer is the same refusal, so a caller learns
// nothing of another connection's handles.
func (r *reviewDiffRecords) get(id, workspace string, owner *transport.ConnState) (*reviewDiffRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.byID[id]
	if !ok || record.owner != owner || record.workspace != workspace {
		return nil, errReviewDiffFilesGone
	}
	r.lru.MoveToFront(record.elem)
	return record, nil
}

// holds reports whether record is still held.
func (r *reviewDiffRecords) holds(record *reviewDiffRecord) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[record.id] == record
}

// remove takes the record under id out, when held; the caller stops it.
func (r *reviewDiffRecords) remove(id string) *reviewDiffRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.byID[id]
	if !ok {
		return nil
	}
	return r.removeLocked(record)
}

// removeAll takes every record out; the caller stops them.
func (r *reviewDiffRecords) removeAll() []*reviewDiffRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var removed []*reviewDiffRecord
	for r.lru != nil && r.lru.Len() > 0 {
		removed = append(removed, r.removeLocked(r.lru.Back().Value.(*reviewDiffRecord)))
	}
	return removed
}

func (r *reviewDiffRecords) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}

func (r *reviewDiffRecords) removeLocked(record *reviewDiffRecord) *reviewDiffRecord {
	r.lru.Remove(record.elem)
	delete(r.byID, record.id)
	r.bindings -= len(record.blobs)
	r.pathBytes -= record.pathBytes
	return record
}

// stopReviewDiffRecords cancels and joins the pins of removed records and
// unbinds their owners' teardown, so nothing of theirs outlives the call.
func stopReviewDiffRecords(records ...*reviewDiffRecord) {
	for _, record := range records {
		if record == nil {
			continue
		}
		record.cancel()
		<-record.done
		record.owner.UnbindCleanup(reviewDiffFilesCleanupKey(record.id))
	}
}

func reviewDiffFilesCleanupKey(id string) string { return "review-diff-files:" + id }

// dropReviewDiffRecord removes and stops one record.
func (a *App) dropReviewDiffRecord(id string) {
	stopReviewDiffRecords(a.reviewDiffFiles.remove(id))
}

// dropReviewDiffRecords removes and stops every record: the harness reset
// and shutdown.
func (a *App) dropReviewDiffRecords() {
	stopReviewDiffRecords(a.reviewDiffFiles.removeAll()...)
	a.reviewDiffFiles.pins.Wait()
}
