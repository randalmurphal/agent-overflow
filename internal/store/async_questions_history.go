package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"agent-overflow/internal/userquestion"
)

// Only an explicit conversation cut removes question state. Cache pruning does not.
// The cut is predicate over the thread's rows, all of which are in fromTurn
// or later.
func cutAsyncQuestionsTx(tx *sql.Tx, threadID string, fromTurn int, predicate string, args []any) error {
	selection, cutArgs, err := timelineArms(tx, threadID, timelineSelection{
		Columns:   func(string, string) string { return "items.id" },
		Turn:      boundInt,
		TurnArgs:  []any{fromTurn},
		FromTurn:  true,
		Where:     "(" + predicate + ")",
		WhereArgs: args,
	})
	if err != nil {
		return err
	}
	binds := append([]any{threadID}, cutArgs...)
	if _, err := tx.Exec(`DELETE FROM async_questions WHERE thread_id=? AND item_id IN (`+selection+`)`, binds...); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE async_questions SET state='unanswered',answer='',send_id='',user_item_id='' WHERE thread_id=? AND user_item_id IN (`+selection+`)`, binds...)
	return err
}

// insertAsyncQuestionStateTx writes a copy of a question into threadID with
// the mark q carries (AsyncQuestion.run): a fork's carries its source's, and
// a transfer's carries none, since the mark is on no wire.
func insertAsyncQuestionStateTx(tx *sql.Tx, threadID string, q AsyncQuestion) error {
	if q.ItemID == "" || q.Index < 0 {
		return fmt.Errorf("invalid question identity")
	}
	if err := userquestion.Validate([]userquestion.Question{{Title: q.Title, Options: q.Options}}); err != nil {
		return err
	}
	switch q.State {
	case "unanswered", "dismissed":
		if q.Answer != "" || q.SendID != "" || q.UserItemID != "" {
			return fmt.Errorf("unanswered question carries a submission")
		}
	case "submitted", "restored", "delivered":
		if q.Answer == "" || q.SendID == "" {
			return fmt.Errorf("answered question is missing its submission")
		}
	default:
		return fmt.Errorf("invalid question state %q", q.State)
	}
	options, err := json.Marshal(q.Options)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO async_questions(thread_id,`+asyncQuestionColumns+`,run_id) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, threadID, q.ItemID, q.Index, q.Title, string(options), q.State, q.Answer, q.SendID, q.UserItemID, q.CreatedAt, q.run)
	return err
}

// scanAsyncQuestionWithRun reads a row selected as asyncQuestionColumns
// followed by run_id.
func scanAsyncQuestionWithRun(row rowScanner) (AsyncQuestion, error) {
	var q AsyncQuestion
	var options string
	if err := row.Scan(&q.ItemID, &q.Index, &q.Title, &options, &q.State, &q.Answer, &q.SendID, &q.UserItemID, &q.CreatedAt, &q.run); err != nil {
		return q, err
	}
	if err := json.Unmarshal([]byte(options), &q.Options); err != nil {
		return q, fmt.Errorf("decode question options: %w", err)
	}
	return q, nil
}
