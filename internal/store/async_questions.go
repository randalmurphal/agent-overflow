package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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
	// run is the run identity of the Store that recorded the question
	// (async_questions.run_id), read only where a copy carries it
	// (copyForkQuestionsTx). Unexported, it is on no wire: a transfer's
	// question lands with the empty mark, which is no run's.
	run string
}

// newRunID makes a Store's run identity (Store.run).
func newRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: make a run identity: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

type AsyncQuestionAnswer struct {
	ItemID string `json:"itemId"`
	Index  int    `json:"index"`
	Answer string `json:"answer"`
}

// RecordAsyncQuestions admits the immutable prompt and its answerable questions together.
// Repeated started/completed frames cannot reset answers or reorder the prompt.
// A prompt it writes is marked with this Store's run (Store.run); one already
// stored keeps the mark it has, and its missing rows, if any, get none, so a
// provider replaying an old prompt after a restart does not make it this
// run's.
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
		run := s.run
		if exists {
			previous, structured, err := userquestion.Decode([]byte(existing.Meta))
			if err != nil || !structured || !reflect.DeepEqual(previous.Questions, meta.Questions) {
				return fmt.Errorf("question identity %s has different content", item.ID)
			}
			item, run = existing, ""
		} else if err := writeItemWithIndexFn(tx, w, &item, nextItemIndexTx); err != nil {
			return err
		}
		if err := insertAsyncQuestions(tx, item, meta.Questions, "unanswered", run); err != nil {
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

// insertAsyncQuestions inserts item's questions it does not hold yet, in
// state and marked with run (empty for none), and checks those it holds
// carry the same content. A row it already holds keeps its state and mark.
func insertAsyncQuestions(tx *sql.Tx, item Item, questions []userquestion.Question, state, run string) error {
	for index, q := range questions {
		options, err := json.Marshal(q.Options)
		if err != nil {
			return err
		}
		result, err := tx.Exec(`INSERT INTO async_questions(thread_id,item_id,question_index,title,options,state,created_at,run_id)
 VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(thread_id,item_id,question_index) DO NOTHING`, item.ThreadID, item.ID, index, q.Title, string(options), state, item.CreatedAt, run)
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
// with one revision ask the same questions. AskedThisRun says this Store's
// run recorded every one of them (Store.run); one an earlier run recorded,
// or no run (a row from before the mark, an imported or transferred
// question), was asked before this run of the app.
type AsyncQuestionGroup struct {
	ItemID       string
	Revision     int64
	AskedThisRun bool
	Questions    []AsyncQuestion
}

// UnansweredAsyncQuestions reads threadID's questions still waiting for an
// answer, by item, oldest first, on idx_async_questions_pending, in one
// statement: none for a thread this computer does not hold (gone, being
// deleted or moved away) or that is archived, whose questions ask nobody.
func (s *Store) UnansweredAsyncQuestions(threadID string) ([]AsyncQuestionGroup, error) {
	rows, err := s.reader().Query(unansweredAsyncQuestionsSQL, threadID, s.run)
	if err != nil {
		return nil, fmt.Errorf("store: read the unanswered questions of %s: %w", threadID, err)
	}
	var out []AsyncQuestionGroup
	for rows.Next() {
		var options string
		var q AsyncQuestion
		var revision int64
		var thisRun bool
		if err := rows.Scan(&q.ItemID, &q.Index, &q.Title, &options, &q.CreatedAt, &revision, &thisRun); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		if err := json.Unmarshal([]byte(options), &q.Options); err != nil {
			return nil, errors.Join(fmt.Errorf("decode question options: %w", err), rows.Close())
		}
		q.State = "unanswered"
		if n := len(out); n > 0 && out[n-1].ItemID == q.ItemID {
			out[n-1].Questions = append(out[n-1].Questions, q)
			out[n-1].AskedThisRun = out[n-1].AskedThisRun && thisRun
			continue
		}
		out = append(out, AsyncQuestionGroup{ItemID: q.ItemID, Revision: revision, AskedThisRun: thisRun, Questions: []AsyncQuestion{q}})
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("store: read the unanswered questions of %s: %w", threadID, err)
	}
	return out, nil
}

// The state IN term is idx_async_questions_pending's predicate, which lets
// the plan use the partial index; its order keeps an item's rows together.
// The revision is read from the item's rows by the primary key, and the
// thread's row once, by its own. ?2 is the Store's run, never empty.
const unansweredAsyncQuestionsSQL = `SELECT q.item_id,q.question_index,q.title,q.options,q.created_at,
 (SELECT MAX(r.revision) FROM async_questions r WHERE r.thread_id=q.thread_id AND r.item_id=q.item_id),
 q.run_id=?2
 FROM async_questions q WHERE q.thread_id=?1 AND q.state IN ('unanswered','submitted','restored') AND q.state='unanswered'
 AND EXISTS (SELECT 1 FROM owned_threads t WHERE t.id=?1 AND t.archived=0)
 ORDER BY q.created_at,q.item_id,q.question_index`

// asyncQuestionRevision is itemID's revision, 0 when it has no questions.
func asyncQuestionRevision(db sqlQueryer, threadID, itemID string) (int64, error) {
	var revision sql.NullInt64
	err := db.QueryRow(`SELECT MAX(revision) FROM async_questions WHERE thread_id=? AND item_id=?`, threadID, itemID).Scan(&revision)
	return revision.Int64, err
}

// ThreadsWithUnansweredAsyncQuestions lists the threads with a question
// still waiting for an answer that holds its thread (asyncQuestionHoldsSQL),
// on idx_async_questions_pending.
func (s *Store) ThreadsWithUnansweredAsyncQuestions() ([]string, error) {
	rows, err := s.reader().Query(threadsWithUnansweredAsyncQuestionsSQL, s.run)
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
// the plan use the partial index. ?1 is the Store's run.
var threadsWithUnansweredAsyncQuestionsSQL = `SELECT DISTINCT q.thread_id FROM async_questions q
 WHERE q.state IN ('unanswered','submitted','restored') AND q.state='unanswered' AND ` + asyncQuestionHoldsSQL("q", "?1")

// asyncQuestionHoldsSQL is whether question alias q, still unanswered,
// holds its thread from rest: it was asked in this run of the app (run, the
// Store's), or its thread is no owned worker. An owned worker's question an
// earlier run recorded is asked of nobody (docs/specs/agent-thread-tools.md,
// "thread_reply"): it holds neither the worker nor its spawner, and no
// boot pass takes it up.
func asyncQuestionHoldsSQL(q, run string) string {
	return `(` + q + `.run_id = ` + run + ` OR NOT EXISTS (SELECT 1 FROM threads hq WHERE hq.id = ` + q + `.thread_id AND hq.placement = 'owned'))`
}

// DismissUnansweredAsyncQuestions dismisses every question of threadID
// still waiting for an answer and reports how many it dismissed.
func (s *Store) DismissUnansweredAsyncQuestions(threadID string) (int64, error) {
	result, err := s.db.Exec(`UPDATE async_questions SET state='dismissed' WHERE thread_id=? AND state='unanswered'`, threadID)
	if err != nil {
		return 0, fmt.Errorf("store: dismiss the questions of %s: %w", threadID, err)
	}
	return result.RowsAffected()
}

// ReturnAsyncAnswer takes back an answer the app wrote to threadID's async
// questions that the provider did not take in: in one transaction it
// deletes the queue row that carries it, if any is left, and puts the
// questions it claimed back to unanswered. It returns the revision each
// item it reopened is at now (AsyncQuestionGroup.Revision): none once the
// provider took the answer in (its questions read delivered) or it was
// returned before. reopened, when set, is given those revisions inside the
// transaction, before any reader can see them.
func (s *Store) ReturnAsyncAnswer(threadID, sendID string, reopened func(revisions map[string]int64)) (_ map[string]int64, err error) {
	if threadID == "" || sendID == "" {
		return nil, fmt.Errorf("store: return an async answer: thread and send id are required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer rollbackTx(tx, "store: return "+sendID, &err)
	if _, err := tx.Exec(`DELETE FROM flush_queue_items WHERE thread_id=? AND send_id<>'' AND send_id=?`, threadID, sendID); err != nil {
		return nil, fmt.Errorf("store: return %s: %w", sendID, err)
	}
	items, err := queryIDs(tx, `SELECT DISTINCT item_id FROM async_questions
 WHERE thread_id=? AND send_id<>'' AND send_id=? AND state IN ('submitted','restored')`, threadID, sendID)
	if err != nil {
		return nil, fmt.Errorf("store: return %s: %w", sendID, err)
	}
	if len(items) == 0 {
		return nil, tx.Commit()
	}
	if _, err := tx.Exec(`UPDATE async_questions SET state='unanswered',answer='',send_id='',user_item_id=''
 WHERE thread_id=? AND send_id<>'' AND send_id=? AND state IN ('submitted','restored')`, threadID, sendID); err != nil {
		return nil, fmt.Errorf("store: return %s: %w", sendID, err)
	}
	revisions := make(map[string]int64, len(items))
	for _, item := range items {
		if revisions[item], err = asyncQuestionRevision(tx, threadID, item); err != nil {
			return nil, fmt.Errorf("store: return %s: %w", sendID, err)
		}
	}
	if reopened != nil {
		reopened(revisions)
	}
	return revisions, tx.Commit()
}

// ConfirmAsyncAnswer records the provider's echo of an answer the app
// wrote to threadID's async questions, as the echo's user row does through
// its triggers once it is written (async_question_answer_insert): in one
// transaction the questions sendID claimed read delivered, by the user row
// itemID, and the recovery row the dispatch kept for the answer
// (DeleteDispatchedFlushQueueItems) goes. It reports whether any question
// changed: none does once a row write or an earlier confirmation took them,
// or the answer went back.
func (s *Store) ConfirmAsyncAnswer(threadID, sendID, itemID string) (_ bool, err error) {
	if threadID == "" || sendID == "" {
		return false, fmt.Errorf("store: confirm an async answer: thread and send id are required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer rollbackTx(tx, "store: confirm "+sendID, &err)
	result, err := tx.Exec(`UPDATE async_questions SET state='delivered',user_item_id=?
 WHERE thread_id=? AND send_id<>'' AND send_id=? AND state='submitted'`, itemID, threadID, sendID)
	if err != nil {
		return false, fmt.Errorf("store: confirm %s: %w", sendID, err)
	}
	confirmed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM flush_queue_items WHERE thread_id=? AND send_id<>'' AND send_id=? AND EXISTS (
 SELECT 1 FROM async_questions q WHERE q.thread_id=flush_queue_items.thread_id AND q.send_id<>''
 AND q.send_id=flush_queue_items.send_id AND q.state='delivered')`, threadID, sendID); err != nil {
		return false, fmt.Errorf("store: confirm %s: %w", sendID, err)
	}
	return confirmed > 0, tx.Commit()
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
// never infers that historic questions are still awaiting an answer. The rows
// it writes for such a question carry no run: it was not asked in this run.
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
		if err := insertAsyncQuestions(tx, item, meta.Questions, "dismissed", ""); err != nil {
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
