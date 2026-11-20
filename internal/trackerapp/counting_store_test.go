package trackerapp

import (
	"maps"
	"sync"

	"agent-overflow/internal/store"
)

// countingStore counts the mirror accessor calls a test makes through
// it. Each accessor is one store transaction (a write) or one read
// snapshot, so the counts are the transactions a unit costs.
type countingStore struct {
	*store.Store
	mu    sync.Mutex
	calls map[string]int
}

func newCountingStore(st *store.Store) *countingStore {
	return &countingStore{Store: st, calls: map[string]int{}}
}

func (c *countingStore) count(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[name]++
}

// take returns the counts since the last take and starts again.
func (c *countingStore) take() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := maps.Clone(c.calls)
	clear(c.calls)
	return out
}

func (c *countingStore) GetOrCreateMirrorRoot(id string) (int64, bool, error) {
	c.count("GetOrCreateMirrorRoot")
	return c.Store.GetOrCreateMirrorRoot(id)
}

func (c *countingStore) GetMirrorRoot(id string) (int64, bool, error) {
	c.count("GetMirrorRoot")
	return c.Store.GetMirrorRoot(id)
}

func (c *countingStore) GetTrackerConnection(id string) (store.TrackerConnection, error) {
	c.count("GetTrackerConnection")
	return c.Store.GetTrackerConnection(id)
}

func (c *countingStore) ListTrackerConnections() ([]store.TrackerConnection, error) {
	c.count("ListTrackerConnections")
	return c.Store.ListTrackerConnections()
}

func (c *countingStore) DetachMirror(id string) (bool, error) {
	c.count("DetachMirror")
	return c.Store.DetachMirror(id)
}

func (c *countingStore) FinishMirrorCleanup(pause store.ChunkPause) error {
	c.count("FinishMirrorCleanup")
	return c.Store.FinishMirrorCleanup(pause)
}

func (c *countingStore) GetMirrorSync(root int64, board string, now int64) (store.MirrorSync, bool, error) {
	c.count("GetMirrorSync")
	return c.Store.GetMirrorSync(root, board, now)
}

func (c *countingStore) RecordMirrorSyncAttempt(root int64, board string, a store.MirrorSyncAttempt) (bool, error) {
	c.count("RecordMirrorSyncAttempt")
	return c.Store.RecordMirrorSyncAttempt(root, board, a)
}

func (c *countingStore) WriteMirrorRowPage(root int64, page store.MirrorRowPage, cursor []byte) (store.MirrorChanges, error) {
	c.count("WriteMirrorRowPage")
	return c.Store.WriteMirrorRowPage(root, page, cursor)
}

func (c *countingStore) WriteMirrorRows(root int64, page store.MirrorRowPage) (store.MirrorChanges, error) {
	c.count("WriteMirrorRows")
	return c.Store.WriteMirrorRows(root, page)
}

func (c *countingStore) UpsertMirrorCards(root, sweep int64, cards []store.MirrorCard, catalog store.MirrorCatalog, readAt int64) (store.MirrorChanges, error) {
	c.count("UpsertMirrorCards")
	return c.Store.UpsertMirrorCards(root, sweep, cards, catalog, readAt)
}

func (c *countingStore) ReconcileMirrorBoard(root int64, board string, lifetime int64, listed []string, complete bool, readAt int64) (store.MirrorReconcile, error) {
	c.count("ReconcileMirrorBoard")
	return c.Store.ReconcileMirrorBoard(root, board, lifetime, listed, complete, readAt)
}

func (c *countingStore) ReplaceMirrorStructure(root int64, st store.MirrorStructure) (store.MirrorStructureWrite, error) {
	c.count("ReplaceMirrorStructure")
	return c.Store.ReplaceMirrorStructure(root, st)
}

func (c *countingStore) ReplaceMirrorHistories(root, sweep int64, hs []store.MirrorHistory) ([]store.MirrorHistoryResult, error) {
	c.count("ReplaceMirrorHistories")
	return c.Store.ReplaceMirrorHistories(root, sweep, hs)
}

func (c *countingStore) ListMirrorHistoryDue(root int64, now int64) ([]store.MirrorHistoryDue, bool, error) {
	c.count("ListMirrorHistoryDue")
	return c.Store.ListMirrorHistoryDue(root, now)
}

func (c *countingStore) StoreMirrorDetails(root, sweep int64, docs []store.MirrorDetailDoc, openedAt, readAt int64) (store.MirrorDetailWrites, error) {
	c.count("StoreMirrorDetails")
	return c.Store.StoreMirrorDetails(root, sweep, docs, openedAt, readAt)
}

func (c *countingStore) ConfirmMirrorDetail(root int64, d store.MirrorDetail, openedAt int64) (store.MirrorDetailWrite, error) {
	c.count("ConfirmMirrorDetail")
	return c.Store.ConfirmMirrorDetail(root, d, openedAt)
}

func (c *countingStore) ListMirrorDetailStates(root int64, issues []string) ([]store.MirrorDetail, error) {
	c.count("ListMirrorDetailStates")
	return c.Store.ListMirrorDetailStates(root, issues)
}

func (c *countingStore) GetMirrorDetail(root int64, issue string) (store.MirrorDetail, bool, error) {
	c.count("GetMirrorDetail")
	return c.Store.GetMirrorDetail(root, issue)
}

func (c *countingStore) GetMirrorBoard(root int64, board string) (store.MirrorStructure, bool, error) {
	c.count("GetMirrorBoard")
	return c.Store.GetMirrorBoard(root, board)
}

func (c *countingStore) GetMirrorCatalog(root int64, users, statuses []string) (store.MirrorCatalog, error) {
	c.count("GetMirrorCatalog")
	return c.Store.GetMirrorCatalog(root, users, statuses)
}

func (c *countingStore) ListMirrorCards(root int64, issues []string) ([]store.MirrorCard, error) {
	c.count("ListMirrorCards")
	return c.Store.ListMirrorCards(root, issues)
}

func (c *countingStore) ListMirrorIssueRows(root int64, issue, afterBoard string) ([]store.MirrorIssueRow, bool, error) {
	c.count("ListMirrorIssueRows")
	return c.Store.ListMirrorIssueRows(root, issue, afterBoard)
}

func (c *countingStore) ListMirrorOffBoardLinkedIssues(root int64, after string) ([]string, bool, error) {
	c.count("ListMirrorOffBoardLinkedIssues")
	return c.Store.ListMirrorOffBoardLinkedIssues(root, after)
}

func (c *countingStore) ListMirrorSection(root int64, board string, section store.MirrorSection, after store.MirrorRowKey) (store.MirrorSectionPage, error) {
	c.count("ListMirrorSection")
	return c.Store.ListMirrorSection(root, board, section, after)
}

func (c *countingStore) ReadMirrorBoardView(root int64, board string, sections []store.MirrorSection) (store.MirrorBoardView, bool, error) {
	c.count("ReadMirrorBoardView")
	return c.Store.ReadMirrorBoardView(root, board, sections)
}

func (c *countingStore) ReadMirrorBoardPatch(root int64, board string, sections []store.MirrorSection, issues []string) (store.MirrorBoardPatch, bool, error) {
	c.count("ReadMirrorBoardPatch")
	return c.Store.ReadMirrorBoardPatch(root, board, sections, issues)
}

func (c *countingStore) ListMirrorIssueBoards(root int64, issues []string) ([]store.MirrorIssueBoard, error) {
	c.count("ListMirrorIssueBoards")
	return c.Store.ListMirrorIssueBoards(root, issues)
}

func (c *countingStore) ReadMirrorRefreshTargets(root int64, issues, boards []string) (store.MirrorRefreshTargets, error) {
	c.count("ReadMirrorRefreshTargets")
	return c.Store.ReadMirrorRefreshTargets(root, issues, boards)
}

func (c *countingStore) ReplaceMirrorWorklogs(root int64, author, from, to string, readAt int64, logs []store.MirrorWorklog) (store.MirrorWorklogChanges, error) {
	c.count("ReplaceMirrorWorklogs")
	return c.Store.ReplaceMirrorWorklogs(root, author, from, to, readAt, logs)
}

func (c *countingStore) ListMirrorWorklogs(root int64, author, from, to string) ([]store.MirrorWorklog, bool, error) {
	c.count("ListMirrorWorklogs")
	return c.Store.ListMirrorWorklogs(root, author, from, to)
}
