package store

import (
	"errors"
	"testing"
)

// shownHistoryFixture: S holds two settled turns. u0 renders payload p,
// which has an appended chunk, and a0 payload p2; u1 opens turn 1 and run
// is still running there. F forks S whole: it shows every row of S but
// run, whose settled copy it owns, and turn 0's row (it owns turn 1's).
// late is a row S writes after the fork, past F's cut.
func shownHistoryFixture(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	mustCreateThread(t, s, "S")
	mustCreateThread(t, s, "H")
	for _, row := range []struct {
		item    Item
		payload string
	}{
		{Item{ID: "u0", TurnIndex: 0, ItemIndex: 0, Kind: "tool_call", Role: "assistant", ToolName: "Bash", PayloadID: "p"}, "p"},
		{Item{ID: "a0", TurnIndex: 0, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", PayloadID: "p2"}, "p2"},
		{Item{ID: "u1", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user"}, ""},
		{Item{ID: "run", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant", ToolName: "Read", Status: "running"}, ""},
	} {
		it := row.item
		it.ThreadID, it.Meta, it.CreatedAt, it.UpdatedAt = "S", "{}", 1, 1
		if it.Status == "" {
			it.Status = "completed"
		}
		var err error
		if row.payload == "" {
			err = insertCarded(s, it)
		} else {
			err = insertWithPayloadCarded(s, it, Payload{ID: row.payload, Kind: "text", Meta: "{}", Data: []byte("out")})
		}
		if err != nil {
			t.Fatalf("insert %s: %v", it.ID, err)
		}
	}
	if err := s.AppendPayloadData("S", "p", []byte(" more"), "{}", 1); err != nil {
		t.Fatal(err)
	}
	for _, turn := range []string{"S:0", "S:1"} {
		index := 0
		if turn == "S:1" {
			index = 1
		}
		if err := s.InsertTurn(Turn{TurnID: turn, ThreadID: "S", TurnIndex: index, StartedAt: 1}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateTurnCompleted(turn, 2, "end_turn", "am", `{"in":1}`, ""); err != nil {
			t.Fatal(err)
		}
	}
	mustPointerFork(t, s, "S", "F", ForkCut{})
	if err := insertCarded(s, Item{ID: "late", ThreadID: "S", TurnIndex: 2, ItemIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Meta: "{}"}); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestShownHistoryGuardsRefuseEveryColumn: each write that would change a
// row, payload, chunk or turn row F shows is refused, by the trigger the
// case names: with that trigger dropped, the same write goes through.
func TestShownHistoryGuardsRefuseEveryColumn(t *testing.T) {
	type refused struct {
		trigger, write string
	}
	cases := map[string]refused{}
	for column, value := range map[string]string{
		"id": "'renamed'", "summary": "'edited'", "status": "'errored'", "meta": `'{"x":1}'`,
		"kind": "'command_result'", "role": "'system'", "turn_index": "3", "item_index": "7",
		"payload_id": "'p2'", "input_payload_id": "'p2'", "parent_id": "'a0'", "is_background": "1",
		"completion_of": "'a0'", "tool_name": "'Read'", "decision": "'approved'",
		"created_at": "42", "updated_at": "42",
	} {
		cases["items."+column] = refused{"trg_items_shown_update", `UPDATE items SET ` + column + ` = ` + value + ` WHERE thread_id = 'S' AND id = 'u0'`}
	}
	cases["items delete"] = refused{"trg_items_shown_delete", `DELETE FROM items WHERE thread_id = 'S' AND id = 'u0'`}
	cases["payloads.data"] = refused{"trg_payloads_shown_update", `UPDATE payloads SET data = x'00' WHERE thread_id = 'S' AND id = 'p'`}
	cases["payloads.meta"] = refused{"trg_payloads_shown_update", `UPDATE payloads SET meta = '{"x":1}' WHERE thread_id = 'S' AND id = 'p'`}
	cases["payload_chunks insert"] = refused{"trg_payload_chunks_shown_insert",
		`INSERT INTO payload_chunks (thread_id, payload_id, chunk_index, start_offset, data, created_at) VALUES ('S', 'p', 9, 99, x'00', 1)`}
	for column, value := range map[string]string{"chunk_index": "chunk_index + 5", "start_offset": "start_offset + 1", "data": "x'00'"} {
		cases["payload_chunks."+column] = refused{"trg_payload_chunks_shown_update", `UPDATE payload_chunks SET ` + column + ` = ` + value + ` WHERE thread_id = 'S' AND payload_id = 'p'`}
	}
	cases["payload_chunks delete"] = refused{"trg_payload_chunks_shown_delete", `DELETE FROM payload_chunks WHERE thread_id = 'S' AND payload_id = 'p'`}
	for column, value := range map[string]string{
		"turn_index": "5", "started_at": "42", "completed_at": "42", "stop_reason": "'error'",
		"assistant_message_id": "'other'", "token_usage_json": "'{}'", "error_message": "'boom'", "provider_turn_id": "'wire'",
	} {
		cases["turns."+column] = refused{"trg_turns_shown_update", `UPDATE turns SET ` + column + ` = ` + value + ` WHERE turn_id = 'S:0'`}
	}
	cases["turns delete"] = refused{"trg_turns_shown_delete", `DELETE FROM turns WHERE turn_id = 'S:0'`}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := shownHistoryFixture(t)
			if _, err := s.db.Exec(c.write); !IsShownHistoryRefusal(err) {
				t.Fatalf("%s = %v, want refused", c.write, err)
			}
			mustExec(t, s.db, `DROP TRIGGER `+c.trigger)
			if _, err := s.db.Exec(c.write); err != nil {
				t.Fatalf("without %s, %s = %v; another guard refuses it", c.trigger, c.write, err)
			}
		})
	}
}

// TestShownHistoryGuardsAllowWhatNoForkShows: the guards pass a revision
// touch, a spans write, a move of a row to another thread (a holder taking
// it), a write that changes nothing, and any write to a row, payload or
// turn row no fork shows: past the cut, hidden by the fork, or the cut
// turn's row the fork owns.
func TestShownHistoryGuardsAllowWhatNoForkShows(t *testing.T) {
	s := shownHistoryFixture(t)
	for _, write := range []string{
		`UPDATE items SET updated_at = updated_at WHERE thread_id = 'S' AND id = 'u0'`,
		`UPDATE items SET summary = summary, meta = meta WHERE thread_id = 'S' AND id = 'u0'`,
		`UPDATE payloads SET spans = '[1]', preview_spans = '[1]' WHERE thread_id = 'S' AND id = 'p'`,
		`UPDATE items SET summary = 'edited', status = 'errored' WHERE thread_id = 'S' AND id = 'late'`,
		`UPDATE items SET status = 'completed', summary = 'done' WHERE thread_id = 'S' AND id = 'run'`,
		`UPDATE turns SET stop_reason = 'error', completed_at = 42 WHERE turn_id = 'S:1'`,
		`UPDATE items SET thread_id = 'H' WHERE thread_id = 'S' AND id = 'u1'`,
		`DELETE FROM items WHERE thread_id = 'S' AND id = 'late'`,
	} {
		if _, err := s.db.Exec(write); err != nil {
			t.Errorf("%s = %v, want allowed", write, err)
		}
	}
}

// TestShownHistoryFixStandsDownThePayloadGuards: inside fixShownHistoryTx
// a write to payload content F shows goes through, which each payload
// guard refuses outside it. The guards of rows and turn rows stay up.
func TestShownHistoryFixStandsDownThePayloadGuards(t *testing.T) {
	for _, c := range []struct {
		write   string
		allowed bool
	}{
		{`UPDATE payloads SET data = x'', spans = '' WHERE thread_id = 'S' AND id = 'p'`, true},
		{`UPDATE payloads SET meta = '{"x":1}' WHERE thread_id = 'S' AND id = 'p'`, true},
		{`INSERT INTO payload_chunks (thread_id, payload_id, chunk_index, start_offset, data, created_at) VALUES ('S', 'p', 9, 99, x'00', 1)`, true},
		{`UPDATE payload_chunks SET data = x'00' WHERE thread_id = 'S' AND payload_id = 'p'`, true},
		{`DELETE FROM payload_chunks WHERE thread_id = 'S' AND payload_id = 'p'`, true},
		{`UPDATE items SET summary = 'edited' WHERE thread_id = 'S' AND id = 'u0'`, false},
		{`UPDATE turns SET stop_reason = 'error' WHERE turn_id = 'S:0'`, false},
	} {
		s := shownHistoryFixture(t)
		if _, err := s.db.Exec(c.write); !IsShownHistoryRefusal(err) {
			t.Fatalf("%s = %v, want refused", c.write, err)
		}
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		err = fixShownHistoryTx(tx, func() error {
			_, err := tx.Exec(c.write)
			return err
		})
		if c.allowed && err != nil {
			t.Errorf("%s in a data fix = %v, want allowed", c.write, err)
		}
		if !c.allowed && !IsShownHistoryRefusal(err) {
			t.Errorf("%s in a data fix = %v, want refused", c.write, err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestShownHistoryFixNeverOutlivesItsTransaction: fixShownHistoryTx
// deletes its row after the fix whether the fix fails or not, so even a
// commit after a failed fix leaves the guards standing.
func TestShownHistoryFixNeverOutlivesItsTransaction(t *testing.T) {
	s := newTestStore(t)
	for _, fixErr := range []error{nil, errors.New("fix failed")} {
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		during := 0
		err = fixShownHistoryTx(tx, func() error {
			if err := tx.QueryRow(`SELECT count(*) FROM shown_history_fix`).Scan(&during); err != nil {
				return err
			}
			return fixErr
		})
		if !errors.Is(err, fixErr) {
			t.Fatalf("fixShownHistoryTx = %v, want %v", err, fixErr)
		}
		if during != 1 {
			t.Fatalf("the fix ran with %d rows in shown_history_fix, want 1", during)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if n := countRows(t, s, `SELECT count(*) FROM shown_history_fix`); n != 0 {
			t.Fatalf("after a fix that returned %v, shown_history_fix holds %d rows", fixErr, n)
		}
	}
}
