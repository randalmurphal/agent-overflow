package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const transferDiffCommentColumns = `id,thread_id,scope,source_key,commit_sha,file_path,status,old_line,new_line,side,
selected_text,body,sent_at,sent_turn_id,created_at,updated_at`

func (s *Store) exportTransferAnnotations(ctx context.Context, threadID string, output io.Writer) error {
	if err := exportTransferRows(ctx, s.reader(), output, "plan", `SELECT `+proposedPlanStateColumns+` FROM proposed_plans WHERE thread_id = ? ORDER BY version`, threadID, scanProposedPlanState); err != nil {
		return err
	}
	if err := exportTransferRows(ctx, s.reader(), output, "plan_comment", `SELECT `+proposedPlanCommentColumns+` FROM proposed_plan_comments WHERE thread_id = ? ORDER BY id`, threadID, scanProposedPlanComment); err != nil {
		return err
	}
	return exportTransferRows(ctx, s.reader(), output, "diff_comment", `SELECT `+transferDiffCommentColumns+` FROM diff_review_comments WHERE thread_id = ? ORDER BY id`, threadID, scanDiffReviewComment)
}

func importTransferAnnotation(tx *sql.Tx, targetID, sourceID string, record transferHistoryRecord) error {
	switch record.Kind {
	case "plan":
		var p ProposedPlanState
		if err := json.Unmarshal(record.Data, &p); err != nil {
			return err
		}
		if p.ThreadID != sourceID || p.ItemID == "" {
			return errors.New("transfer: plan belongs to another conversation")
		}
		if p.ImplementedByThreadID == sourceID {
			p.ImplementedByThreadID = targetID
		}
		_, err := tx.Exec(`INSERT INTO proposed_plans (`+proposedPlanStateColumns+`) VALUES (?,?,?,?,?,?,?,?,?)`, p.ItemID, targetID, p.RevisionParentItemID, p.Version, p.ImplementedAt, p.ImplementedByThreadID, p.ImplementedByItemID, p.CreatedAt, p.UpdatedAt)
		return err
	case "plan_comment":
		var p ProposedPlanComment
		if err := json.Unmarshal(record.Data, &p); err != nil {
			return err
		}
		if p.ThreadID != sourceID || p.ID == "" {
			return errors.New("transfer: plan comment belongs to another conversation")
		}
		if sourceID != targetID {
			p.ID = transferContentID(targetID, "plan_comment", p.ID)
		}
		p.SentTurnID = transferredTurnID(sourceID, targetID, p.SentTurnID)
		_, err := tx.Exec(`INSERT INTO proposed_plan_comments (`+proposedPlanCommentColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, p.ID, targetID, p.PlanItemID, p.Status, p.StartLine, p.EndLine, p.SelectedText, p.Body, p.SentAt, p.SentTurnID, p.CreatedAt, p.UpdatedAt)
		return err
	case "diff_comment":
		var p DiffReviewComment
		if err := json.Unmarshal(record.Data, &p); err != nil {
			return err
		}
		if p.ThreadID != sourceID || p.ID == "" {
			return errors.New("transfer: diff comment belongs to another conversation")
		}
		if sourceID != targetID {
			p.ID = transferContentID(targetID, "diff_comment", p.ID)
		}
		p.SentTurnID = transferredTurnID(sourceID, targetID, p.SentTurnID)
		_, err := tx.Exec(`INSERT INTO diff_review_comments (`+transferDiffCommentColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.ID, targetID, p.Scope, p.SourceKey, p.CommitSHA, p.FilePath, p.Status, p.OldLine, p.NewLine, p.Side, p.SelectedText, p.Body, p.SentAt, p.SentTurnID, p.CreatedAt, p.UpdatedAt)
		return err
	default:
		return fmt.Errorf("transfer: unsupported history record %q; update this computer first", record.Kind)
	}
}
