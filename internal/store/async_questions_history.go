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
	selection, cutArgs := timelineArms(threadID, timelineSelection{
		Columns:   func(string, string) string { return "items.id" },
		Turn:      "?",
		TurnArgs:  []any{fromTurn},
		FromTurn:  true,
		Where:     "(" + predicate + ")",
		WhereArgs: args,
	})
	binds := append([]any{threadID}, cutArgs...)
	if _, err := tx.Exec(`DELETE FROM async_questions WHERE thread_id=? AND item_id IN (`+selection+`)`, binds...); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE async_questions SET state='unanswered',answer='',send_id='',user_item_id='' WHERE thread_id=? AND user_item_id IN (`+selection+`)`, binds...)
	return err
}

func cloneAsyncQuestionsTx(tx *sql.Tx, sourceID, targetID string, idMap map[string]string) error {
	rows, err := tx.Query(`SELECT `+asyncQuestionColumns+` FROM async_questions WHERE thread_id=? ORDER BY created_at,rowid`, sourceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		q, err := scanAsyncQuestion(rows)
		if err != nil {
			return err
		}
		mapped, keep := idMap[q.ItemID]
		if !keep {
			continue
		}
		q.ItemID = mapped
		q.UserItemID = idMap[q.UserItemID]
		if q.State == "submitted" || q.State == "restored" || (q.State == "delivered" && q.UserItemID == "") {
			q.State, q.Answer, q.SendID = "unanswered", "", ""
		}
		if err := insertAsyncQuestionStateTx(tx, targetID, q); err != nil {
			return err
		}
	}
	return rows.Err()
}

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
	_, err = tx.Exec(`INSERT INTO async_questions(thread_id,`+asyncQuestionColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?)`, threadID, q.ItemID, q.Index, q.Title, string(options), q.State, q.Answer, q.SendID, q.UserItemID, q.CreatedAt)
	return err
}
