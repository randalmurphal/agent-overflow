package sessionimport

import (
	"fmt"
	"time"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/store"
)

// sessionImporter commits one scanned provider session as at most one thread.
// It owns rollback so a failed write cannot leave a thread that causes the
// source session to disappear from future scans.
type sessionImporter struct {
	store *store.Store
	row   Row
	proj  store.Project
	// importedAt is bookkeeping — when AO read the file — not history. It
	// is the one timestamp in this package that is allowed to be now().
	importedAt int64

	created  []store.Thread
	warnings []importir.Warning
}

func newSessionImporter(s *store.Store, row Row, proj store.Project) *sessionImporter {
	return &sessionImporter{
		store:      s,
		row:        row,
		proj:       proj,
		importedAt: time.Now().UnixMilli(),
	}
}

func (im *sessionImporter) warn(warnings ...importir.Warning) {
	im.warnings = append(im.warnings, warnings...)
}

func (im *sessionImporter) outcome() ImportOutcome {
	return ImportOutcome{Threads: im.created, Warnings: im.warnings}
}

// add commits one session: its thread row, its whole history in one
// transaction, and the cursor a refresh resumes from. Any failure rolls the
// session back and returns the error to report.
func (im *sessionImporter) add(plan branchPlan) error {
	if len(im.created) != 0 {
		return im.fail(fmt.Errorf(
			"sessionimport: %s attempted to create more than one thread", im.row.ID))
	}
	thread := newImportedThread(
		im.row, im.proj, plan.title, plan.sessionRef, plan.events, plan.profile, plan.lastActivityAt)
	if err := im.store.CreateThread(thread); err != nil {
		return im.fail(fmt.Errorf("sessionimport: create thread for %s: %w", im.row.ID, err))
	}
	im.created = append(im.created, thread)

	batch, buildWarnings, err := NewWriter(im.store, thread).Build(plan.events)
	if err != nil {
		return im.fail(fmt.Errorf("sessionimport: convert %s: %w", im.row.ID, err))
	}
	im.warn(buildWarnings...)
	if err := im.store.ApplyImportBatch(thread.ID, batch); err != nil {
		return im.fail(fmt.Errorf("sessionimport: write %s: %w", im.row.ID, err))
	}

	cursor := NewCursor(batch, plan.events)
	if plan.endOffset > cursor.SourceOffset {
		cursor.SourceOffset = plan.endOffset
	}
	state := store.ThreadImportState{
		ThreadID:              thread.ID,
		Provider:              im.row.Provider,
		SourcePath:            im.row.SourcePath,
		SourceSessionID:       im.row.SessionID,
		SourceParentSessionID: im.row.ParentSessionID,
		LeafUUID:              plan.leafUUID,
		ImportedAt:            im.importedAt,
	}
	cursor.Apply(&state)
	if err := im.store.SetThreadImportState(state); err != nil {
		return im.fail(fmt.Errorf("sessionimport: record cursor for %s: %w", im.row.ID, err))
	}
	lineageWarnings, err := im.store.ReconcileImportedForkLineage(im.row.Provider, im.row.SessionID)
	if err != nil {
		return im.fail(fmt.Errorf("sessionimport: reconcile fork lineage for %s: %w", im.row.ID, err))
	}
	for _, warning := range lineageWarnings {
		// Reconciliation is global because parent and child can arrive in
		// either order. Surface every new observation even when importing the
		// parent is what finally made an older child's invalid cycle visible.
		im.warn(importir.Warning{Code: warning.Code, Message: warning.Message})
	}
	if im.row.ParentSessionID != "" {
		resolved, err := im.store.GetThread(thread.ID)
		if err != nil {
			return im.fail(fmt.Errorf("sessionimport: read reconciled fork %s: %w", im.row.ID, err))
		}
		im.created[len(im.created)-1] = resolved
	}
	return nil
}

// fail rolls every thread this session created back and returns the error
// the caller should report. A rollback failure is reported alongside the
// original rather than replacing it: it leaves a thread the next scan will
// dedup against, which is exactly what the user needs to be told.
func (im *sessionImporter) fail(err error) error {
	for i := len(im.created) - 1; i >= 0; i-- {
		// Import rollback also removes usage. Ordinary DeleteThread keeps the
		// ledger intentionally, but this thread never successfully existed.
		if deleteErr := im.store.RollbackImportedThread(im.created[i].ID); deleteErr != nil {
			err = fmt.Errorf("%w (rollback of thread %s also failed: %v)",
				err, im.created[i].ID, deleteErr)
		}
	}
	im.created = nil
	return err
}
