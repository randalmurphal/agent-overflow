package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// newTimelineDB builds a store-shaped database: the real `items`,
// `payloads` and `payload_chunks` column names, plus the two
// `timeline_*` views migration v61 introduced, which are what the reads
// under test actually go through.
//
// The imported half of those views (import_history_items /
// thread_import_chunks) is deliberately NOT modelled: what these tests
// pin is the SQL's thread scoping, its ordering and its windowing, and
// the shared-chunk join is the store package's own subject.
func newTimelineDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "timeline.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The production handle (openReadOnlyDB) is capped at ONE connection,
	// and the cap is load-bearing for these tests: a query nested inside an
	// open rows cursor deadlocks on it (observed live 2026-08-26 as a
	// silent from-thread hang — the payload reads used to run inside the
	// items scan loop). An uncapped test pool would happily hand the nested
	// query a second connection and green-light the regression.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	schema := []string{
		`CREATE TABLE items (
		    id               TEXT NOT NULL,
		    thread_id        TEXT NOT NULL,
		    turn_index       INTEGER NOT NULL,
		    item_index       INTEGER NOT NULL,
		    kind             TEXT NOT NULL,
		    role             TEXT NOT NULL,
		    summary          TEXT NOT NULL DEFAULT '',
		    payload_id       TEXT,
		    input_payload_id TEXT,
		    completion_of    TEXT NOT NULL DEFAULT '',
		    tool_name        TEXT NOT NULL DEFAULT '',
		    PRIMARY KEY (thread_id, id)
		)`,
		`CREATE TABLE payloads (
		    thread_id TEXT NOT NULL,
		    id        TEXT NOT NULL,
		    data      BLOB NOT NULL,
		    PRIMARY KEY (thread_id, id)
		)`,
		`CREATE TABLE payload_chunks (
		    thread_id   TEXT NOT NULL,
		    payload_id  TEXT NOT NULL,
		    chunk_index INTEGER NOT NULL,
		    data        BLOB NOT NULL,
		    PRIMARY KEY (thread_id, payload_id, chunk_index)
		)`,
		`CREATE VIEW timeline_items AS SELECT * FROM items`,
		`CREATE VIEW timeline_payloads AS SELECT * FROM payloads`,
	}
	for _, statement := range schema {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return db
}

type seedItem struct {
	id        string
	thread    string
	turn      int
	index     int
	kind      string
	role      string
	summary   string
	payload   string
	inputID   string
	completes string
	tool      string
}

func seedItems(t *testing.T, db *sql.DB, items ...seedItem) {
	t.Helper()
	for _, item := range items {
		var payload, input any
		if item.payload != "" {
			payload = item.payload
		}
		if item.inputID != "" {
			input = item.inputID
		}
		if _, err := db.Exec(
			`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, summary, payload_id, input_payload_id, completion_of, tool_name)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			item.id, item.thread, item.turn, item.index, item.kind, item.role, item.summary,
			payload, input, item.completes, item.tool,
		); err != nil {
			t.Fatalf("seed item %s: %v", item.id, err)
		}
	}
}

// seedPayload writes the head plus its appended chunks, exactly as
// appendPayloadDataTx does: `payloads.data` is what the payload was born
// with, and each chunk is one flushed delta.
func seedPayload(t *testing.T, db *sql.DB, threadID, payloadID, head string, chunks ...string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO payloads (thread_id, id, data) VALUES (?,?,?)`, threadID, payloadID, head); err != nil {
		t.Fatalf("seed payload %s: %v", payloadID, err)
	}
	for i, chunk := range chunks {
		if _, err := db.Exec(
			`INSERT INTO payload_chunks (thread_id, payload_id, chunk_index, data) VALUES (?,?,?,?)`,
			threadID, payloadID, i, chunk,
		); err != nil {
			t.Fatalf("seed chunk %d of %s: %v", i, payloadID, err)
		}
	}
}

func TestReadPayloadPiecesReturnsTheHeadThenEveryChunkInOrder(t *testing.T) {
	db := newTimelineDB(t)
	seedPayload(t, db, "th", "p1", "head:", "one ", "two ", "three")

	pieces, err := readPayloadPieces(db, "th", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(pieces, []string{"head:", "one ", "two ", "three"}) {
		t.Fatalf("pieces = %v", pieces)
	}
}

// An empty head is not a piece. A streamed payload is born empty and
// grows by chunks, so counting the head would add a leading empty delta
// to essentially every streamed item.
func TestReadPayloadPiecesDropsAnEmptyHead(t *testing.T) {
	db := newTimelineDB(t)
	seedPayload(t, db, "th", "p1", "", "a", "b")

	pieces, err := readPayloadPieces(db, "th", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(pieces, []string{"a", "b"}) {
		t.Fatalf("pieces = %v", pieces)
	}
}

// Payloads have been keyed (thread_id, id) since migration v58 exactly
// because ids repeat across threads. A read scoped by id alone returns
// another thread's bytes, which is the bug
// frontend/scripts/generate-freeze-replay-fixture.mjs still carries and
// this verb must not copy.
func TestReadPayloadPiecesNeverCrossesThreads(t *testing.T) {
	db := newTimelineDB(t)
	seedPayload(t, db, "thread-a", "shared", "", "A-one", "A-two")
	seedPayload(t, db, "thread-b", "shared", "", "B-one")

	a, err := readPayloadPieces(db, "thread-a", "shared")
	if err != nil {
		t.Fatal(err)
	}
	b, err := readPayloadPieces(db, "thread-b", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(a, []string{"A-one", "A-two"}) {
		t.Errorf("thread-a pieces = %v", a)
	}
	if !equalStrings(b, []string{"B-one"}) {
		t.Errorf("thread-b pieces = %v", b)
	}
}

// A payload the GC took is a state, not a failure: the item still
// existed, and the synthesizer reports it as empty content.
func TestReadPayloadPiecesTreatsAMissingPayloadAsEmpty(t *testing.T) {
	db := newTimelineDB(t)
	pieces, err := readPayloadPieces(db, "th", "gone")
	if err != nil {
		t.Fatalf("a missing payload was an error: %v", err)
	}
	if len(pieces) != 0 {
		t.Fatalf("pieces = %v, want none", pieces)
	}
}

func seedFiveTurns(t *testing.T, db *sql.DB) {
	t.Helper()
	for turn := 1; turn <= 5; turn++ {
		payload := "p" + string(rune('0'+turn))
		seedPayload(t, db, "th", payload, "", "turn ", string(rune('0'+turn)))
		seedItems(t, db,
			seedItem{id: "u" + payload, thread: "th", turn: turn, index: 0, kind: kindUserText, role: "user",
				summary: "prompt " + string(rune('0'+turn))},
			seedItem{id: "a" + payload, thread: "th", turn: turn, index: 1, kind: kindAssistantText, role: "assistant",
				payload: payload},
		)
	}
	// A second thread at the same turn positions: every read must scope.
	seedItems(t, db, seedItem{id: "other", thread: "other", turn: 5, index: 0, kind: kindAssistantText, role: "assistant"})
}

func TestReadRecordedTurnsWindowsToTheLastN(t *testing.T) {
	db := newTimelineDB(t)
	seedFiveTurns(t, db)

	turns, err := readRecordedTurns(db, "th", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := turnIndexes(turns); !equalInts(got, []int{4, 5}) {
		t.Fatalf("turn indexes = %v, want [4 5]", got)
	}
	for _, turn := range turns {
		for _, item := range turn.Items {
			if strings.HasPrefix(item.ID, "other") {
				t.Fatalf("turn %d picked up another thread's item", turn.TurnIndex)
			}
		}
	}
}

func TestReadRecordedTurnsCapsAtWhatTheThreadHas(t *testing.T) {
	db := newTimelineDB(t)
	seedFiveTurns(t, db)

	turns, err := readRecordedTurns(db, "th", 50)
	if err != nil {
		t.Fatal(err)
	}
	if got := turnIndexes(turns); !equalInts(got, []int{1, 2, 3, 4, 5}) {
		t.Fatalf("turn indexes = %v", got)
	}
}

// The recorded user text is the drive half: without it the document is a
// script with no trigger.
func TestReadRecordedTurnsCarriesTheUserTextForTheSendRecipe(t *testing.T) {
	db := newTimelineDB(t)
	seedPayload(t, db, "th", "up", "please ", "do it")
	seedItems(t, db,
		seedItem{id: "u", thread: "th", turn: 1, index: 0, kind: kindUserText, role: "user",
			summary: "please do…", payload: "up"},
		seedItem{id: "a", thread: "th", turn: 1, index: 1, kind: kindAssistantText, role: "assistant"},
	)

	turns, err := readRecordedTurns(db, "th", 1)
	if err != nil {
		t.Fatal(err)
	}
	// The PAYLOAD wins over the summary, which is a truncated render.
	if turns[0].UserText != "please do it" {
		t.Fatalf("user text = %q", turns[0].UserText)
	}
	recipe := sendRecipe("th", turns)
	if len(recipe) != 1 || !strings.HasSuffix(recipe[0], `--wait 'please do it'`) {
		t.Fatalf("recipe = %v", recipe)
	}
}

// A user row with no payload still has to drive a send; the summary is
// the fallback.
func TestReadRecordedTurnsFallsBackToTheUserSummary(t *testing.T) {
	db := newTimelineDB(t)
	seedItems(t, db,
		seedItem{id: "u", thread: "th", turn: 1, index: 0, kind: kindUserText, role: "user", summary: "summary only"},
		seedItem{id: "a", thread: "th", turn: 1, index: 1, kind: kindAssistantText, role: "assistant"},
	)
	turns, err := readRecordedTurns(db, "th", 1)
	if err != nil {
		t.Fatal(err)
	}
	if turns[0].UserText != "summary only" {
		t.Fatalf("user text = %q", turns[0].UserText)
	}
}

// A tool_call's input is a second payload on the same row, and it has to
// be scoped the same way the content payload is.
func TestReadRecordedTurnsLoadsAToolCallInputPayload(t *testing.T) {
	db := newTimelineDB(t)
	seedPayload(t, db, "th", "in", `{"command":`, `"ls -la"}`)
	seedItems(t, db, seedItem{
		id: "tu", thread: "th", turn: 1, index: 0, kind: kindToolCall, role: "assistant",
		tool: "Bash", inputID: "in",
	})

	turns, err := readRecordedTurns(db, "th", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := turns[0].Items[0].Input; got != `{"command":"ls -la"}` {
		t.Fatalf("input = %q", got)
	}
	if got := turns[0].Items[0].ToolName; got != "Bash" {
		t.Fatalf("tool name = %q", got)
	}
}

// End to end over the pure halves: real rows in, a document the real
// parser accepts out, with the pairing the recording carried.
func TestRecordedTurnsRebuildIntoAValidScenario(t *testing.T) {
	db := newTimelineDB(t)
	seedPayload(t, db, "th", "out", "", "a", "b", "c")
	seedPayload(t, db, "th", "res", "tool said so")
	seedPayload(t, db, "th", "in", `{"command":"ls"}`)
	seedItems(t, db,
		seedItem{id: "u", thread: "th", turn: 7, index: 0, kind: kindUserText, role: "user", summary: "go"},
		seedItem{id: "tu", thread: "th", turn: 7, index: 1, kind: kindToolCall, role: "assistant", tool: "Bash", inputID: "in"},
		seedItem{id: "tc", thread: "th", turn: 7, index: 2, kind: kindToolCompletion, role: "user", completes: "tu", payload: "res"},
		seedItem{id: "a", thread: "th", turn: 7, index: 3, kind: kindAssistantText, role: "assistant", payload: "out"},
	)

	turns, err := readRecordedTurns(db, "th", 1)
	if err != nil {
		t.Fatal(err)
	}
	doc, stats, err := synthesizeScenario(synthOptions{
		Name: "from-thread-th", Provider: "claude", ThreadID: "th", Turns: turns, DelayMs: 15,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("the rebuilt document does not validate: %v", err)
	}
	if stats.Items != 3 || stats.Deltas != 3 {
		t.Fatalf("stats = %+v, want 3 items and 3 deltas", stats)
	}
	lines := linesOf(doc.Turns[0])
	if got := toolUseID(t, lines[0]); got != toolResultID(t, lines[1]) {
		t.Fatalf("tool pairing broke across the read: %q vs %q", got, toolResultID(t, lines[1]))
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
