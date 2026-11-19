package store

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"
)

// questionRuns is the run mark of each of itemID's questions in threadID,
// by index.
func questionRuns(t *testing.T, s *Store, threadID, itemID string) []string {
	t.Helper()
	runs, err := queryIDs(s.db, `SELECT run_id FROM async_questions WHERE thread_id=? AND item_id=? ORDER BY question_index`, threadID, itemID)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// askedThisRun maps each unanswered item of threadID to whether s's run
// asked it.
func askedThisRun(t *testing.T, s *Store, threadID string) map[string]bool {
	t.Helper()
	groups, err := s.UnansweredAsyncQuestions(threadID)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]bool, len(groups))
	for _, group := range groups {
		out[group.ItemID] = group.AskedThisRun
	}
	return out
}

// A question is marked with the run that first recorded it, and nothing
// after moves the mark: a replay of the same item in that run or a later
// one, an answer and its echo, a revert of the answer, a dismissal and its
// reopening, a dismissal of the thread's questions and a returned answer.
// The run that recorded it reads it as asked this run; a later one does not.
func TestAnAsyncQuestionKeepsTheMarkOfTheRunThatRecordedIt(t *testing.T) {
	first := openStoreAt(t)
	mustCreateThread(t, first, "q")
	item := questionFixture(t, first, "q", "question:call")
	mark := first.run
	if mark == "" {
		t.Fatal("the store has no run")
	}
	want := []string{mark, mark}
	requireMark := func(s *Store, after string) {
		t.Helper()
		if got := questionRuns(t, s, "q", item.ID); !slices.Equal(got, want) {
			t.Fatalf("after %s the question's marks = %q, want %q", after, got, want)
		}
	}
	requireMark(first, "the first insert")
	if got := askedThisRun(t, first, "q"); !got[item.ID] {
		t.Fatalf("the recording run reads its question as asked this run = %v", got)
	}
	if _, err := first.RecordAsyncQuestions(item); err != nil {
		t.Fatal(err)
	}
	requireMark(first, "a replay in the same run")

	s := reopenStore(t, first)
	if s.run == mark || s.run == "" {
		t.Fatalf("the reopened store's run = %q, want a new one (the first was %q)", s.run, mark)
	}
	if got := askedThisRun(t, s, "q"); got[item.ID] {
		t.Fatalf("a later run reads the question as asked this run = %v", got)
	}
	if _, err := s.RecordAsyncQuestions(item); err != nil {
		t.Fatal(err)
	}
	requireMark(s, "a replay in a later run")
	// The history cache can lose the prompt's row while its questions stay;
	// the replay writes the row again and leaves the questions' mark.
	if _, err := s.db.Exec(`DELETE FROM items WHERE thread_id=? AND id=?`, "q", item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordAsyncQuestions(item); err != nil {
		t.Fatal(err)
	}
	requireMark(s, "a replay in a later run of a pruned prompt")
	if got := askedThisRun(t, s, "q"); got[item.ID] {
		t.Fatalf("a replay made the question this run's: %v", got)
	}

	answers := []AsyncQuestionAnswer{{ItemID: item.ID, Index: 0, Answer: "One"}}
	body, _, err := s.AsyncAnswerMessage("q", "send", answers)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.QueueAsyncAnswers(FlushQueueItem{ID: "queue", ThreadID: "q", SendID: "send", Message: body, Payload: json.RawMessage(`{}`)}, answers, nil); err != nil {
		t.Fatal(err)
	}
	requireMark(s, "an answer")
	if _, err := s.ReturnAsyncAnswer("q", "send", nil); err != nil {
		t.Fatal(err)
	}
	requireMark(s, "a returned answer")
	if err := s.QueueAsyncAnswers(FlushQueueItem{ID: "queue-2", ThreadID: "q", SendID: "send-2", Message: body, Payload: json.RawMessage(`{}`)}, []AsyncQuestionAnswer{{ItemID: item.ID, Index: 0, Answer: "One"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertItem(Item{ID: "answer", ThreadID: "q", TurnIndex: 1, Kind: "user_text", Role: "user", Meta: `{"sendId":"send-2","provider_item_id":"native"}`}); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.ListAsyncQuestions("q", item.ID); err != nil || rows[0].State != "delivered" {
		t.Fatalf("the answered question = %+v, %v; want it delivered", rows, err)
	}
	requireMark(s, "the answer's echo")
	if _, _, err := s.DeleteConversationFromItem("q", "answer"); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.ListAsyncQuestions("q", item.ID); err != nil || rows[0].State != "unanswered" {
		t.Fatalf("the reverted question = %+v, %v; want it unanswered", rows, err)
	}
	requireMark(s, "a revert of the answer")
	if err := s.SetAsyncQuestionDismissed("q", item.ID, 1, true); err != nil {
		t.Fatal(err)
	}
	requireMark(s, "a dismissal")
	if err := s.SetAsyncQuestionDismissed("q", item.ID, 1, false); err != nil {
		t.Fatal(err)
	}
	requireMark(s, "a reopening")
	if n, err := s.DismissUnansweredAsyncQuestions("q"); err != nil || n != 2 {
		t.Fatalf("dismissed %d, %v; want both questions", n, err)
	}
	requireMark(s, "the thread's dismissal")
}

// A question a later run records is that run's, beside one an earlier run
// recorded, which stays the earlier run's.
func TestEachRunMarksTheQuestionsItRecords(t *testing.T) {
	first := openStoreAt(t)
	mustCreateThread(t, first, "q")
	questionFixture(t, first, "q", "question:old")
	earlier := first.run
	s := reopenStore(t, first)
	questionFixture(t, s, "q", "question:new")
	if got := questionRuns(t, s, "q", "question:old"); !slices.Equal(got, []string{earlier, earlier}) {
		t.Fatalf("the earlier question's marks = %q, want the earlier run's", got)
	}
	if got := questionRuns(t, s, "q", "question:new"); !slices.Equal(got, []string{s.run, s.run}) {
		t.Fatalf("the new question's marks = %q, want this run's", got)
	}
	if got := askedThisRun(t, s, "q"); len(got) != 2 || got["question:old"] || !got["question:new"] {
		t.Fatalf("asked this run = %v, want only the new question", got)
	}
}

// A prompt stored without its questions (imported history) was not asked
// in this run: neither the person opening one of its questions nor a
// provider replaying it marks the questions it writes.
func TestQuestionsOfAPromptStoredBeforeThemCarryNoMark(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "q")
	const meta = `{"delivery":"async","questions":[{"title":"Still needed?"}]}`
	for index, id := range []string{"historic-opened", "historic-replayed"} {
		if err := s.InsertItem(Item{ID: id, ThreadID: "q", ItemIndex: index, Kind: "assistant_text", Role: "assistant", Meta: meta, CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetAsyncQuestionDismissed("q", "historic-opened", 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordAsyncQuestions(Item{ID: "historic-replayed", ThreadID: "q", Kind: "assistant_text", Role: "assistant", Status: "completed", Meta: meta, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"historic-opened", "historic-replayed"} {
		if got := questionRuns(t, s, "q", id); !slices.Equal(got, []string{""}) {
			t.Fatalf("%s's marks = %q, want none", id, got)
		}
	}
	if got := askedThisRun(t, s, "q"); len(got) != 2 || got["historic-opened"] || got["historic-replayed"] {
		t.Fatalf("asked this run = %v, want neither", got)
	}
}

// A fork made in this run of a thread holding a question an earlier run
// asked and one this run asked copies both marks: only the second is this
// run's in the fork.
func TestAForkCarriesTheMarksOfItsQuestions(t *testing.T) {
	first := openStoreAt(t)
	mustCreateThread(t, first, "q")
	questionFixture(t, first, "q", "question:old")
	earlier := first.run
	s := reopenStore(t, first)
	questionFixture(t, s, "q", "question:new")
	mustPointerFork(t, s, "q", "fork", throughTurn(0))
	if got := questionRuns(t, s, "fork", "question:old"); !slices.Equal(got, []string{earlier, earlier}) {
		t.Fatalf("the fork's earlier question marks = %q, want the earlier run's %q", got, earlier)
	}
	if got := questionRuns(t, s, "fork", "question:new"); !slices.Equal(got, []string{s.run, s.run}) {
		t.Fatalf("the fork's new question marks = %q, want this run's %q", got, s.run)
	}
	if got := askedThisRun(t, s, "fork"); len(got) != 2 || got["question:old"] || !got["question:new"] {
		t.Fatalf("the fork's questions asked this run = %v, want only the new one", got)
	}
}

// A conversation transferred in carries no mark, whichever run recorded its
// questions: the wire has none, and they are the person's where they land.
func TestATransferredQuestionCarriesNoMark(t *testing.T) {
	s, destination := newTestStore(t), newTestStore(t)
	mustCreateThread(t, s, "q")
	item := questionFixture(t, s, "q", "question:call")
	var exported bytes.Buffer
	if err := s.ExportThreadHistory(context.Background(), "q", &exported); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(exported.Bytes(), []byte(s.run)) {
		t.Fatal("the transfer wire carries the run")
	}
	thread, err := s.GetThread("q")
	if err != nil {
		t.Fatal(err)
	}
	if err := destination.ImportThreadHistory(context.Background(), thread, &exported); err != nil {
		t.Fatal(err)
	}
	if got := questionRuns(t, destination, "q", item.ID); !slices.Equal(got, []string{"", ""}) {
		t.Fatalf("the transferred question's marks = %q, want none", got)
	}
	if got := askedThisRun(t, destination, "q"); len(got) != 1 || got[item.ID] {
		t.Fatalf("the transferred question asked this run = %v, want it not", got)
	}
}

// v160 adds the mark and leaves every question from before it with the
// empty one, which no run's equals.
func TestAsyncQuestionRunMigrationLeavesOldQuestionsUnmarked(t *testing.T) {
	db := migrateThrough(t, asyncQuestionRunMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p-run', '/tmp/run', 'Run', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived, mode)
		VALUES ('t-run', 'p-run', 'Run', 'codex', '/tmp/run', 'test-model', 1, 1, 0, 'chat')`)
	mustExec(t, db, `INSERT INTO async_questions (thread_id, item_id, question_index, title, options, created_at)
		VALUES ('t-run', 'question:old', 0, 'Which scope?', '[]', 1)`)

	migrateFromThrough(t, db, asyncQuestionRunMigrationVersion-1, asyncQuestionRunMigrationVersion)

	var run string
	if err := db.QueryRow(`SELECT run_id FROM async_questions WHERE item_id = 'question:old'`).Scan(&run); err != nil {
		t.Fatal(err)
	}
	if run != "" {
		t.Fatalf("the old question's mark = %q, want none", run)
	}
	var notNull int
	var defaultValue string
	if err := db.QueryRow(`SELECT "notnull", dflt_value FROM pragma_table_info('async_questions') WHERE name = 'run_id'`).Scan(&notNull, &defaultValue); err != nil {
		t.Fatal(err)
	}
	if notNull != 1 || defaultValue != "''" {
		t.Fatalf("run_id is notnull %d default %s, want NOT NULL DEFAULT ''", notNull, defaultValue)
	}
	// The read a store makes with its run (UnansweredAsyncQuestions).
	migrateFrom(t, db, asyncQuestionRunMigrationVersion)
	rows, err := db.Query(unansweredAsyncQuestionsSQL, "t-run", "a-run")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var read int
	for rows.Next() {
		var itemID, title, options string
		var index int
		var created, revision int64
		var thisRun bool
		if err := rows.Scan(&itemID, &index, &title, &options, &created, &revision, &thisRun); err != nil {
			t.Fatal(err)
		}
		read++
		if thisRun {
			t.Fatalf("the old question %s reads as asked this run", itemID)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if read != 1 {
		t.Fatalf("read %d unanswered questions, want the old one", read)
	}
}
