package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// CheckTransferHistoryConflicts compares a fully parsed candidate with the live
// cache before the source retires. Row validation in an empty scratch database
// cannot detect globally keyed turns, comments or attachments already here.
// The journal has reserved the target ID; only its own retired cache may remain.
func (s *Store) CheckTransferHistoryConflicts(ctx context.Context, candidate *Store, threadID string) error {
	var exists int
	err := s.reader().QueryRowContext(ctx, `SELECT 1 FROM owned_threads WHERE id = ?`, threadID).Scan(&exists)
	if err == nil {
		return errors.New("The destination already has an active conversation with this identity.")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, key := range []struct{ table, column, label string }{{"turns", "turn_id", "conversation turns"}, {"proposed_plan_comments", "id", "plan comments"}, {"diff_review_comments", "id", "review comments"}, {"attachments", "id", "attachments"}} {
		after := ""
		for {
			rows, err := candidate.reader().QueryContext(ctx, `SELECT `+key.column+` FROM `+key.table+` WHERE thread_id = ? AND `+key.column+` > ? ORDER BY `+key.column+` LIMIT 128`, threadID, after)
			if err != nil {
				return err
			}
			var ids []any
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				if id == "" || len(id) > 4096 {
					rows.Close()
					return errors.New("transfer: invalid history row identity")
				}
				ids = append(ids, id)
				after = id
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				break
			}
			args := append([]any{threadID}, ids...)
			query := `SELECT 1 FROM ` + key.table + ` WHERE thread_id <> ? AND ` + key.column + ` IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `) LIMIT 1`
			err = s.reader().QueryRowContext(ctx, query, args...).Scan(&exists)
			if err == nil {
				return fmt.Errorf("The destination has conflicting %s. Copy the conversation instead of moving it.", key.label)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
	}
	// Returning moves may reuse immutable uploads left with their retired
	// cache. Matching the owner alone must not replace a different upload record.
	attachments, err := candidate.ListAttachments(threadID)
	if err != nil {
		return err
	}
	for _, expected := range attachments {
		actual, err := scanAttachment(s.reader().QueryRowContext(ctx, `SELECT `+attachmentColumns+` FROM attachments WHERE id = ?`, expected.ID))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if actual != expected {
			return errors.New("The destination has a conflicting attachment record.")
		}
	}
	return nil
}
