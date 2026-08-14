package sessionimport

import (
	"fmt"
	"time"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// sessionImporter commits one scanned session's branches, one at a time.
//
// One at a time is the whole point: a Claude transcript is converted branch
// by branch and each branch's events are released once its rows are
// committed, so peak memory is one branch rather than the whole file. The
// importer is what makes that safe — it owns the whole-session rollback, so
// a branch that fails after three succeeded still leaves nothing behind.
//
// Rollback is not an optimisation. The dedup set the scan subtracts keys on
// the source session id, so a half-imported session would hide its missing
// branches from every future scan.
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

// add commits one branch: its thread row, its whole history in one
// transaction, and the cursor a refresh resumes from. Any failure rolls the
// WHOLE session back and returns the error to report.
func (im *sessionImporter) add(plan branchPlan) error {
	suffixModel, _, _ := sessionModelProfile(plan.events)
	thread := newImportedThread(
		im.row, im.proj, plan.title, plan.sessionRef, plan.events, plan.lastActivityAt)
	if err := im.store.CreateThread(thread); err != nil {
		return im.fail(fmt.Errorf("sessionimport: create thread for %s: %w", im.row.ID, err))
	}
	im.created = append(im.created, thread)

	prefix := store.ImportedHistoryPrefix{LastTurnIndex: -1, LastItemIndex: -1}
	if plan.prefixSourceThreadID != "" {
		boundaryAt := int64(0)
		for _, event := range plan.events {
			if event.Kind == provider.EventTurnStart {
				boundaryAt = event.Timestamp.UnixMilli()
				break
			}
		}
		if boundaryAt <= 0 {
			return im.fail(fmt.Errorf(
				"sessionimport: shared prefix for %s has no suffix turn boundary", im.row.ID))
		}
		var err error
		prefix, err = im.store.CloneImportedHistoryPrefix(
			plan.prefixSourceThreadID, thread.ID, plan.prefixBeforeTurn, boundaryAt)
		if err != nil {
			return im.fail(fmt.Errorf("sessionimport: share prefix for %s: %w", im.row.ID, err))
		}
		if suffixModel == "" && prefix.Model != "" && prefix.Model != thread.Model {
			if err := im.store.UpdateModel(thread.ID, prefix.Model); err != nil {
				return im.fail(fmt.Errorf("sessionimport: inherit prefix model for %s: %w", im.row.ID, err))
			}
			thread.Model = prefix.Model
			im.created[len(im.created)-1].Model = prefix.Model
		}
	}

	batch, buildWarnings, err := NewWriter(im.store, thread).Build(plan.events)
	if err != nil {
		return im.fail(fmt.Errorf("sessionimport: convert %s: %w", im.row.ID, err))
	}
	im.warn(buildWarnings...)
	if err := im.store.ApplyImportBatch(thread.ID, batch); err != nil {
		return im.fail(fmt.Errorf("sessionimport: write %s: %w", im.row.ID, err))
	}

	cursor := NewCursor(batch, plan.events)
	if cursor.TurnIndex < 0 && prefix.LastTurnIndex >= 0 {
		cursor.TurnIndex = prefix.LastTurnIndex
		cursor.ItemIndex = prefix.LastItemIndex
	}
	if cursor.SourceUUID == "" {
		cursor.SourceUUID = prefix.LastSourceUUID
	}
	if plan.endOffset > cursor.SourceOffset {
		cursor.SourceOffset = plan.endOffset
	}
	state := store.ThreadImportState{
		ThreadID:        thread.ID,
		Provider:        im.row.Provider,
		SourcePath:      im.row.SourcePath,
		SourceSessionID: im.row.SessionID,
		LeafUUID:        plan.leafUUID,
		ImportedAt:      im.importedAt,
	}
	cursor.Apply(&state)
	if err := im.store.SetThreadImportState(state); err != nil {
		return im.fail(fmt.Errorf("sessionimport: record cursor for %s: %w", im.row.ID, err))
	}
	return nil
}

// settleBranches decides the two things that can only be known once every
// branch of a transcript has been converted.
//
// Both are computed over the threads that SURVIVED, never over the
// transcript's leaf count — a branch whose rows were all metadata produces
// no thread, and counting leaves would put a "— branch 2" suffix on a sole
// survivor.
//
// activeThread is the index of the thread cut from the transcript's LAST
// branch, or -1 when that branch produced none. Only that thread may carry
// the session ref: `claude --resume <id>` reopens the file's ACTIVE branch,
// so giving the ref to any other thread would silently continue a different
// conversation, with two AO threads appending to one file. When the active
// branch produced no thread, NOTHING gets the ref — every imported thread
// then materialises a transcript of its own at its recorded leaf the first
// time it runs (app_session_import_branch.go), which is correct rather than
// merely cheap.
func (im *sessionImporter) settleBranches(activeThread int) error {
	if len(im.created) == 0 {
		return nil
	}
	if len(im.created) == 1 {
		if plain := importedTitle(im.row.Title); plain != im.created[0].Title {
			if err := im.store.UpdateTitle(im.created[0].ID, plain); err != nil {
				return im.fail(fmt.Errorf("sessionimport: title thread for %s: %w", im.row.ID, err))
			}
			im.created[0].Title = plain
		}
	}
	if activeThread < 0 || activeThread >= len(im.created) {
		return nil
	}
	if _, err := im.store.UpdateSessionRef(im.created[activeThread].ID, im.row.SessionID); err != nil {
		return im.fail(fmt.Errorf("sessionimport: record session ref for %s: %w", im.row.ID, err))
	}
	im.created[activeThread].SessionRef = im.row.SessionID
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
