package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"agent-overflow/internal/userquestion"
)

var ErrAsyncQuestionHandled = errors.New("question already answered or dismissed; refresh the questions before submitting again")

type AsyncQuestion struct {
	ItemID     string   `json:"itemId"`
	Index      int      `json:"index"`
	Title      string   `json:"title"`
	Options    []string `json:"options"`
	State      string   `json:"state"`
	Answer     string   `json:"answer"`
	SendID     string   `json:"sendId"`
	UserItemID string   `json:"userItemId"`
	CreatedAt  int64    `json:"createdAt"`
}

type AsyncQuestionAnswer struct {
	ItemID string `json:"itemId"`
	Index  int    `json:"index"`
	Answer string `json:"answer"`
}

// RecordAsyncQuestions admits the immutable prompt and its answerable questions together.
// Repeated started/completed frames cannot reset answers or reorder the prompt.
func (s *Store) RecordAsyncQuestions(item Item) (Item, error) {
	meta, found, err := userquestion.Decode([]byte(item.Meta))
	if err != nil {
		return Item{}, err
	}
	if !found || item.ID == "" || item.ThreadID == "" || item.Kind != "assistant_text" {
		return Item{}, fmt.Errorf("invalid async question item")
	}
	applyItemDefaults(&item)
	var persisted Item
	err = s.groupWriteItems(item.ThreadID, item.SubagentCard, "record async questions", func(tx *sql.Tx, w *cardWrite) error {
		existing, exists, err := s.getThreadItem(tx, item.ThreadID, item.ID)
		if err != nil {
			return err
		}
		if exists {
			previous, structured, err := userquestion.Decode([]byte(existing.Meta))
			if err != nil || !structured || !reflect.DeepEqual(previous.Questions, meta.Questions) {
				return fmt.Errorf("question identity %s has different content", item.ID)
			}
			item = existing
		} else if err := writeItemWithIndexFn(tx, w, &item, nextItemIndexTx); err != nil {
			return err
		}
		if err := insertAsyncQuestions(tx, item, meta.Questions, "unanswered"); err != nil {
			return err
		}
		if err := w.finish(); err != nil {
			return err
		}
		persisted, err = readBackUpsertedItem(tx, item.ThreadID, item.ID)
		return err
	})
	if err != nil {
		return Item{}, err
	}
	return persisted, nil
}

func insertAsyncQuestions(tx *sql.Tx, item Item, questions []userquestion.Question, state string) error {
	for index, q := range questions {
		options, err := json.Marshal(q.Options)
		if err != nil {
			return err
		}
		result, err := tx.Exec(`INSERT INTO async_questions(thread_id,item_id,question_index,title,options,state,created_at)
 VALUES(?,?,?,?,?,?,?) ON CONFLICT(thread_id,item_id,question_index) DO NOTHING`, item.ThreadID, item.ID, index, q.Title, string(options), state, item.CreatedAt)
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if inserted == 0 {
			previous, err := scanAsyncQuestion(tx.QueryRow(`SELECT `+asyncQuestionColumns+` FROM async_questions WHERE thread_id=? AND item_id=? AND question_index=?`, item.ThreadID, item.ID, index))
			if err != nil {
				return err
			}
			if previous.Title != q.Title || !reflect.DeepEqual(previous.Options, q.Options) {
				return fmt.Errorf("question identity %s/%d has different content", item.ID, index)
			}
		}
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM async_questions WHERE thread_id=? AND item_id=?`, item.ThreadID, item.ID).Scan(&count); err != nil {
		return err
	}
	if count != len(questions) {
		return fmt.Errorf("question identity %s has a different question count", item.ID)
	}
	return nil
}

const asyncQuestionColumns = `item_id,question_index,title,options,state,answer,send_id,user_item_id,created_at`

func scanAsyncQuestion(row rowScanner) (AsyncQuestion, error) {
	var q AsyncQuestion
	var options string
	if err := row.Scan(&q.ItemID, &q.Index, &q.Title, &options, &q.State, &q.Answer, &q.SendID, &q.UserItemID, &q.CreatedAt); err != nil {
		return q, err
	}
	if err := json.Unmarshal([]byte(options), &q.Options); err != nil {
		return q, fmt.Errorf("decode question options: %w", err)
	}
	return q, nil
}

// ListAsyncQuestions returns pending questions, or one historical prompt on demand.
func (s *Store) ListAsyncQuestions(threadID, itemID string) ([]AsyncQuestion, error) {
	query := `SELECT ` + asyncQuestionColumns + ` FROM async_questions WHERE thread_id=?`
	args := []any{threadID}
	if itemID == "" {
		query += ` AND state IN ('unanswered','submitted','restored') AND state <> 'restored'`
	} else {
		query += ` AND item_id=?`
		args = append(args, itemID)
	}
	query += ` ORDER BY created_at,rowid`
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AsyncQuestion{}
	for rows.Next() {
		q, err := scanAsyncQuestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// AsyncQuestionGroup is one item's questions still waiting for an answer,
// with the item's revision: the highest of its rows', which every change of
// state of any of them raises (async_question_revision), so two readings
// with one revision ask the same questions.
type AsyncQuestionGroup struct {
	ItemID    string
	Revision  int64
	Questions []AsyncQuestion
}

// UnansweredAsyncQuestions reads threadID's questions still waiting for an
// answer, by item, oldest first, on idx_async_questions_pending.
func (s *Store) UnansweredAsyncQuestions(threadID string) ([]AsyncQuestionGroup, error) {
	rows, err := s.reader().Query(unansweredAsyncQuestionsSQL, threadID)
	if err != nil {
		return nil, fmt.Errorf("store: read the unanswered questions of %s: %w", threadID, err)
	}
	var out []AsyncQuestionGroup
	for rows.Next() {
		var options string
		var q AsyncQuestion
		var revision int64
		if err := rows.Scan(&q.ItemID, &q.Index, &q.Title, &options, &q.CreatedAt, &revision); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if err := json.Unmarshal([]byte(options), &q.Options); err != nil {
			return nil, errors.Join(fmt.Errorf("decode question options: %w", err), rows.Close())
		}
		q.State = "unanswered"
		if n := len(out); n > 0 && out[n-1].ItemID == q.ItemID {
			out[n-1].Questions = append(out[n-1].Questions, q)
			continue
		}
		out = append(out, AsyncQuestionGroup{ItemID: q.ItemID, Revision: revision, Questions: []AsyncQuestion{q}})
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: read the unanswered questions of %s: %w", threadID, err)
	}
	return out, nil
}

// The state IN term is idx_async_questions_pending's predicate, which lets
// the plan use the partial index; its order keeps an item's rows together.
// The revision is read from the item's rows by the primary key.
const unansweredAsyncQuestionsSQL = `SELECT q.item_id,q.question_index,q.title,q.options,q.created_at,
 (SELECT MAX(r.revision) FROM async_questions r WHERE r.thread_id=q.thread_id AND r.item_id=q.item_id)
 FROM async_questions q WHERE q.thread_id=? AND q.state IN ('unanswered','submitted','restored') AND q.state='unanswered'
 ORDER BY q.created_at,q.item_id,q.question_index`

// asyncQuestionRevision is itemID's revision, 0 when it has no questions.
func asyncQuestionRevision(db sqlQueryer, threadID, itemID string) (int64, error) {
	var revision sql.NullInt64
	err := db.QueryRow(`SELECT MAX(revision) FROM async_questions WHERE thread_id=? AND item_id=?`, threadID, itemID).Scan(&revision)
	return revision.Int64, err
}

// ThreadsWithUnansweredAsyncQuestions lists the threads with a question
// still waiting for an answer, on idx_async_questions_pending.
func (s *Store) ThreadsWithUnansweredAsyncQuestions() ([]string, error) {
	rows, err := s.reader().Query(threadsWithUnansweredAsyncQuestionsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: list the threads with unanswered questions: %w", err)
	}
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: list the threads with unanswered questions: %w", err)
	}
	return out, nil
}

// The state IN term is idx_async_questions_pending's predicate, which lets
// the plan use the partial index.
const threadsWithUnansweredAsyncQuestionsSQL = `SELECT DISTINCT thread_id FROM async_questions
 WHERE state IN ('unanswered','submitted','restored') AND state='unanswered'`

// DismissUnansweredAsyncQuestions dismisses every question of threadID
// still waiting for an answer and reports how many it dismissed.
func (s *Store) DismissUnansweredAsyncQuestions(threadID string) (int64, error) {
	result, err := s.db.Exec(`UPDATE async_questions SET state='dismissed' WHERE thread_id=? AND state='unanswered'`, threadID)
	if err != nil {
		return 0, fmt.Errorf("store: dismiss the questions of %s: %w", threadID, err)
	}
	return result.RowsAffected()
}

// AsyncAnswerMessage validates the submitted snapshot and includes each question's text.
func (s *Store) AsyncAnswerMessage(threadID, sendID string, answers []AsyncQuestionAnswer) (string, bool, error) {
	return asyncAnswerMessage(s.reader(), threadID, sendID, answers)
}

func asyncAnswerMessage(db sqlQueryer, threadID, sendID string, answers []AsyncQuestionAnswer) (string, bool, error) {
	if strings.TrimSpace(sendID) == "" || len(answers) == 0 {
		return "", false, fmt.Errorf("send identity and answers are required")
	}
	parts := make([]string, 0, len(answers))
	seen := map[string]bool{}
	already := 0
	for _, answer := range answers {
		key := fmt.Sprintf("%s:%d", answer.ItemID, answer.Index)
		if seen[key] || strings.TrimSpace(answer.Answer) == "" {
			return "", false, fmt.Errorf("duplicate or empty question answer")
		}
		seen[key] = true
		q, err := scanAsyncQuestion(db.QueryRow(`SELECT `+asyncQuestionColumns+` FROM async_questions WHERE thread_id=? AND item_id=? AND question_index=?`, threadID, answer.ItemID, answer.Index))
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, ErrAsyncQuestionHandled
		}
		if err != nil {
			return "", false, err
		}
		if q.SendID == sendID {
			if q.Answer != answer.Answer {
				return "", false, fmt.Errorf("submission identity already used with different answers")
			}
			already++
		} else if q.State != "unanswered" {
			return "", false, ErrAsyncQuestionHandled
		}
		parts = append(parts, fmt.Sprintf("Question: %s\nAnswer: %s", q.Title, answer.Answer))
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM async_questions WHERE thread_id=? AND send_id<>'' AND send_id=?`, threadID, sendID).Scan(&count); err != nil {
		return "", false, err
	}
	if count != already || (already > 0 && already != len(answers)) {
		return "", false, fmt.Errorf("submission identity has a different question set")
	}
	return strings.Join(parts, "\n\n"), already == len(answers), nil
}

// QueueAsyncAnswers atomically claims every question and admits its one user
// message. asked names, by item, the revision the answers were written for
// (AsyncQuestionGroup.Revision): an item whose revision moved since was
// answered, dismissed or reopened meanwhile, and the submission is
// ErrAsyncQuestionHandled. The person's answers name none: they answer what
// is unanswered when they land.
func (s *Store) QueueAsyncAnswers(item FlushQueueItem, answers []AsyncQuestionAnswer, asked map[string]int64) error {
	if item.SendID == "" || len(answers) == 0 {
		return fmt.Errorf("question submission requires an identity and answers")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for itemID, want := range asked {
		revision, err := asyncQuestionRevision(tx, item.ThreadID, itemID)
		if err != nil {
			return err
		}
		if revision != want {
			return ErrAsyncQuestionHandled
		}
	}
	body, already, err := asyncAnswerMessage(tx, item.ThreadID, item.SendID, answers)
	if err != nil {
		return err
	}
	if already {
		return ErrAsyncQuestionHandled
	}
	if item.Message != body {
		return fmt.Errorf("question message does not match its answers")
	}
	for _, answer := range answers {
		if strings.TrimSpace(answer.Answer) == "" {
			return fmt.Errorf("empty question answer")
		}
		result, err := tx.Exec(`UPDATE async_questions SET state='submitted',answer=?,send_id=? WHERE thread_id=? AND item_id=? AND question_index=? AND state='unanswered'`, answer.Answer, item.SendID, item.ThreadID, answer.ItemID, answer.Index)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrAsyncQuestionHandled
		}
	}
	if err := insertFlushQueueItem(tx, item); err != nil {
		return err
	}
	return tx.Commit()
}

// SetAsyncQuestionDismissed can expose imported questions on explicit request, but
// never infers that historic questions are still awaiting an answer.
func (s *Store) SetAsyncQuestionDismissed(threadID, itemID string, index int, dismissed bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	item, found, err := s.getThreadItem(tx, threadID, itemID)
	if err != nil {
		return err
	}
	if found {
		meta, structured, err := userquestion.Decode([]byte(item.Meta))
		if err != nil {
			return err
		}
		if !structured {
			return fmt.Errorf("item has no structured questions")
		}
		if err := insertAsyncQuestions(tx, item, meta.Questions, "dismissed"); err != nil {
			return err
		}
	}
	state := "unanswered"
	if dismissed {
		state = "dismissed"
	}
	result, err := tx.Exec(`UPDATE async_questions SET state=? WHERE thread_id=? AND item_id=? AND question_index=? AND state IN ('unanswered','dismissed')`, state, threadID, itemID, index)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrAsyncQuestionHandled
	}
	return tx.Commit()
}
