package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

// CheckThreadTransferActivation admits only the secret the source releases
// AFTER its retirement is durable. Call before installing staged files; commit
// checks it again in the same transaction that makes the history runnable.
func (s *Store) CheckThreadTransferActivation(id, digest string, secret []byte) (ThreadTransfer, error) {
	row, err := s.GetThreadTransfer(id)
	if err != nil {
		return ThreadTransfer{}, err
	}
	return row, checkTransferActivation(row, digest, secret)
}

func matchesTransferSecret(row ThreadTransfer, secret []byte) bool {
	hash := sha256.Sum256(secret)
	expected, err := hex.DecodeString(row.ActivationHash)
	return err == nil && len(secret) == 32 && subtle.ConstantTimeCompare(expected, hash[:]) == 1
}

func checkTransferActivation(row ThreadTransfer, digest string, secret []byte) error {
	if !matchesTransferSecret(row, secret) ||
		row.Direction != "incoming" || row.ManifestHash != digest || !validTransferDigest(digest) ||
		(row.Phase != "prepared" && row.Phase != "complete") {
		return errors.New("transfer: activation is not authorized")
	}
	return nil
}

// CommitIncomingThreadTransfer publishes installed files and their history as
// one ownership change. The caller has already verified and durably installed
// every file, holds the thread action lock, and selects target from the immutable
// destination offer. A lost reply can be retried without reading/importing again.
// This is the ONLY incoming completion path; phase-only advancement cannot
// unlock a destination without both the source secret and complete history.
func (s *Store) CommitIncomingThreadTransfer(ctx context.Context, id, digest string, secret []byte, target Thread, history io.Reader) (ThreadTransfer, error) {
	tx, release, err := s.beginDurableTx(ctx)
	if err != nil {
		return ThreadTransfer{}, err
	}
	defer release()
	defer tx.Rollback()
	row, err := scanThreadTransfer(tx.QueryRow(`SELECT `+transferColumns+` FROM thread_transfers WHERE id = ?`, id))
	if err != nil {
		return ThreadTransfer{}, err
	}
	if err := checkTransferActivation(row, digest, secret); err != nil {
		return ThreadTransfer{}, err
	}
	if target.ID != row.ThreadID {
		return ThreadTransfer{}, errors.New("transfer: activation names another conversation")
	}
	if row.Phase == "complete" {
		return row, nil
	}
	// A holder counts: the delete of a copy its forks read kept the id
	// as one (retireToHolderTx), and the returning conversation takes it
	// back through the replacement below.
	var stamp HistoryStamp
	err = tx.QueryRow(`SELECT history_rev, history_epoch FROM threads WHERE id = ?`, target.ID).Scan(&stamp.Rev, &stamp.Epoch)
	existed := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ThreadTransfer{}, fmt.Errorf("store: read history stamp for %s: %w", target.ID, err)
	}
	if existed {
		// Only a previously retired copy can be replaced. No incoming request
		// may overwrite an unrelated conversation, even with a valid secret.
		var direction, kind, phase string
		err := tx.QueryRow(`SELECT direction, kind, phase FROM thread_transfers
WHERE thread_id = ? AND id <> ? AND phase <> 'canceled' ORDER BY rowid DESC LIMIT 1`, target.ID, id).Scan(&direction, &kind, &phase)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return ThreadTransfer{}, err
		}
		if row.Kind != "move" || direction != "outgoing" || kind != "move" || (phase != "committed" && phase != "complete") {
			return ThreadTransfer{}, errors.New("transfer: destination conversation already exists")
		}
		if stamp.Rev == math.MaxInt64 || stamp.Epoch == math.MaxInt64 {
			return ThreadTransfer{}, errors.New("transfer: destination history counter is exhausted")
		}
	}
	if history == nil {
		return ThreadTransfer{}, errors.New("transfer: missing prepared history")
	}
	released := false
	if existed {
		if released, err = s.replaceTransferredHistoryTx(ctx, tx, target, history); err != nil {
			return ThreadTransfer{}, err
		}
		if _, err := tx.Exec(`UPDATE threads SET history_rev = MAX(history_rev, ?), history_epoch = MAX(history_epoch, ?) WHERE id = ?`, stamp.Rev+1, stamp.Epoch+1, target.ID); err != nil {
			return ThreadTransfer{}, err
		}
	} else if err := s.importThreadHistoryTx(ctx, tx, target, history); err != nil {
		return ThreadTransfer{}, err
	}
	row.Phase, row.Error, row.UpdatedAt = "complete", "", time.Now().UnixMilli()
	if _, err := tx.Exec(`UPDATE thread_transfers SET phase = 'complete', error = '', updated_at = ? WHERE id = ?`, row.UpdatedAt, id); err != nil {
		return ThreadTransfer{}, err
	}
	if err := tx.Commit(); err != nil {
		return ThreadTransfer{}, err
	}
	if released {
		s.holdersMayBeReleased()
	}
	return row, nil
}

// replaceTransferredHistoryTx replaces a returning thread's history with
// the one it brings back. It reports whether it removed lineage rows,
// which can release a holder (holdersMayBeReleased).
func (s *Store) replaceTransferredHistoryTx(ctx context.Context, tx *sql.Tx, target Thread, history io.Reader) (bool, error) {
	prepared, lastReadAt, err := prepareThreadForCreate(target)
	if err != nil {
		return false, err
	}
	if err := setHistoryBulkLoadTx(tx, target.ID, true, "returning transfer"); err != nil {
		return false, err
	}
	// The forks that read the thread keep what they show: those rows go to
	// a holder first, and the forks read none of the replacement. The
	// replacement is the thread's whole history, so the thread reads
	// nothing through its own lineage any more. Its hides stay: they are
	// the ones its forks read its ancestors through. The cards are
	// recomputed below, so the split's card write is not finished.
	w := s.bulkItemWrites(tx, target.ID, false)
	if _, err := splitShownRowsTx(tx, w, target.ID, forkSplit{keep: &nothingKept, turnsKeptThrough: -1}); err != nil {
		return false, err
	}
	result, err := tx.Exec(`DELETE FROM thread_fork_lineage WHERE thread_id = ?`, target.ID)
	if err != nil {
		return false, fmt.Errorf("store: unlink returning thread %s: %w", target.ID, err)
	}
	unlinked, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: unlink returning thread %s: %w", target.ID, err)
	}
	// Preserve the parent row: deleting/recreating it would silently null
	// other local threads' fork links and cascade unrelated host records.
	// Attachments also remain: a local fork may still reference an old upload.
	for _, table := range []string{"thread_import_chunks", "thread_import_item_overrides", "items", "payloads", "turns", "message_anchors", "async_questions", "proposed_plans", "diff_review_comments", "thread_drafts", "pending_background_task_terminals", "flush_queue_items"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE thread_id = ?`, target.ID); err != nil {
			return false, err
		}
	}
	// The replacement history reindexes itself row by row; what the deleted
	// history left in the index has to go first, title included, because the
	// thread row itself survives and cascades nothing.
	if err := deleteThreadSearchThreadTx(tx, target.ID); err != nil {
		return false, err
	}
	var fields []string
	for _, column := range strings.Split(threadInsertColumns, ",") {
		column = strings.TrimSpace(column)
		if column != "id" {
			fields = append(fields, column+"=excluded."+column)
		}
	}
	// Reuse creation's column set so a new portable field cannot silently
	// disappear only when the conversation returns to a former owner.
	// The returning conversation is live even where a delete of the copy
	// it replaces had begun, or had kept it as a holder its forks read.
	if err := writeThread(tx, prepared, lastReadAt, ` ON CONFLICT(id) DO UPDATE SET `+strings.Join(fields, ",")+`,live_todo='',worktree_setup_state='',deleting=0`); err != nil {
		return false, err
	}
	if err := readTransferHistoryTx(ctx, tx, target, history); err != nil {
		return false, err
	}
	// The rows were loaded with the triggers' aggregate work suspended and
	// carry whatever stamps the sending computer wrote; rebuild them from
	// the rows this thread now holds.
	if err := s.restampSubagentAggregatesTx(tx, target.ID); err != nil {
		return false, err
	}
	return unlinked > 0 || w.levelsDropped, setHistoryBulkLoadTx(tx, target.ID, false, "returning transfer")
}
