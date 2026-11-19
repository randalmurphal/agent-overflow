package store

import (
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// v155ResidueWakesReferenceSQL is the residue statement as first written:
// it reads every top-level user_text row and its meta. The parity test
// holds v155ResidueWakesSQL to its result.
const v155ResidueWakesReferenceSQL = `WITH residue(thread_id, send_id, joined) AS (
  SELECT u.thread_id,
         CASE WHEN json_valid(u.meta) THEN json_extract(u.meta, '$.sendId') END,
         COALESCE(CASE WHEN json_valid(u.meta) THEN
           CASE WHEN json_type(u.meta, '$.joinedSendIds') = 'array' THEN json_extract(u.meta, '$.joinedSendIds') END END, '[]')
    FROM items u
   WHERE u.kind = 'user_text' AND u.parent_id = ''
     AND (CASE WHEN json_valid(u.meta) THEN json_extract(u.meta, '$.pendingFlush') END) = 1
     AND u.created_at >= (SELECT t.created_at FROM threads t WHERE t.id = u.thread_id)
     AND NOT EXISTS (SELECT 1 FROM turns later WHERE later.thread_id = u.thread_id AND later.turn_index > u.turn_index)
     AND NOT EXISTS (SELECT 1 FROM items later
                      WHERE later.thread_id = u.thread_id AND later.turn_index > u.turn_index AND later.parent_id = ''))
INSERT OR IGNORE INTO v155_undelivered_wakes (thread_id, send_id)
SELECT thread_id, send_id FROM residue WHERE send_id GLOB 'thread-wake*'
UNION
SELECT r.thread_id, j.value FROM residue r JOIN json_each(r.joined) j WHERE j.type = 'text' AND j.value GLOB 'thread-wake*';`

// v155QueuedWakesReferenceSQL is the queued statement as first written: each
// queued wake reads every top-level user_text row of its thread.
const v155QueuedWakesReferenceSQL = `INSERT OR IGNORE INTO v155_undelivered_wakes (thread_id, send_id)
SELECT q.thread_id, q.send_id
  FROM flush_queue_items q
 WHERE q.send_id GLOB 'thread-wake*'
   AND NOT EXISTS (SELECT 1 FROM items d
        WHERE d.thread_id = q.thread_id AND d.kind = 'user_text' AND d.parent_id = ''
          AND (CASE WHEN json_valid(d.meta) THEN COALESCE(json_extract(d.meta, '$.provider_item_id'), '') END) <> ''
          AND (CASE WHEN json_valid(d.meta) THEN COALESCE(json_extract(d.meta, '$.wire_only'), 0) END) != 1
          AND ((CASE WHEN json_valid(d.meta) THEN json_extract(d.meta, '$.sendId') END) = q.send_id
            OR q.send_id IN (SELECT j.value FROM json_each(CASE WHEN json_valid(d.meta) THEN d.meta ELSE '{}' END, '$.joinedSendIds') j)));`

const v155UndeliveredWakesTableSQL = `CREATE TEMP TABLE v155_undelivered_wakes (
    thread_id TEXT NOT NULL,
    send_id   TEXT NOT NULL,
    PRIMARY KEY (thread_id, send_id)
) WITHOUT ROWID`

// seedV155WakeShapes writes, over several threads, every shape of user row
// the undelivered-wake statements must tell apart, and the queued wakes
// whose delivery they judge.
func seedV155WakeShapes(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	for _, th := range []struct {
		id      string
		created int
		turns   int
	}{
		// a: every meta shape, in its last turn.
		{"a", 100, 5},
		// b: wakes followed by a later turn or a later top-level row.
		{"b", 100, 3},
		// c: a wake older than the thread, and one not.
		{"c", 500, 2},
		// d: no turns at all.
		{"d", 100, 0},
		// e: a wake followed only by a nested row.
		{"e", 100, 2},
		// f: queued wakes.
		{"f", 100, 1},
	} {
		mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived)
			VALUES (?, 'p', ?, 'claude', '/p', '', ?, 1, 0)`, th.id, th.id, th.created)
		for turn := 1; turn <= th.turns; turn++ {
			mustExec(t, db, `INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at) VALUES (?, ?, ?, 1, 2)`,
				fmt.Sprintf("%s:%d", th.id, turn), th.id, turn)
		}
	}
	index := 0
	row := func(thread, id string, turn int, kind, parent, meta string, created int) {
		t.Helper()
		index++
		role := "user"
		if kind != "user_text" {
			role = "assistant"
		}
		mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary, parent_id, meta, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 'completed', 'text', ?, ?, ?, ?)`, id, thread, turn, index, kind, role, parent, meta, created, created)
	}
	user := func(thread, id string, turn int, meta string) { row(thread, id, turn, "user_text", "", meta, 200) }

	// Meta that json_valid rejects: JSON5, which json_extract reads. Malformed
	// meta cannot be stored, as idx_items_meta_task_id's predicate fails on it.
	user("a", "a-json5", 5, `{sendId:'thread-wake:a-json5',pendingFlush:true,joinedSendIds:['thread-wake:a-json5-joined']}`)
	user("a", "a-no-send", 5, `{"pendingFlush":true}`)
	user("a", "a-null-send", 5, `{"sendId":null,"pendingFlush":true}`)
	user("a", "a-number-send", 5, `{"sendId":5,"pendingFlush":true}`)
	user("a", "a-array-send", 5, `{"sendId":["thread-wake:a-array-send"],"pendingFlush":true}`)
	user("a", "a-object-send", 5, `{"sendId":{"id":"thread-wake:a-object-send"},"pendingFlush":true}`)
	user("a", "a-person", 5, `{"sendId":"person","pendingFlush":true}`)
	user("a", "a-wake", 5, `{"sendId":"thread-wake:a-wake","pendingFlush":true}`)
	user("a", "a-late", 5, `{"sendId":"thread-wake-late:a-late","pendingFlush":1}`)
	user("a", "a-prefix", 5, `{"sendId":"thread-wakeful","pendingFlush":true}`)
	user("a", "a-flush-0", 5, `{"sendId":"thread-wake:a-flush-0","pendingFlush":0}`)
	user("a", "a-flush-false", 5, `{"sendId":"thread-wake:a-flush-false","pendingFlush":false}`)
	user("a", "a-flush-string", 5, `{"sendId":"thread-wake:a-flush-string","pendingFlush":"1"}`)
	user("a", "a-flush-absent", 5, `{"sendId":"thread-wake:a-flush-absent"}`)
	user("a", "a-joined", 5, `{"sendId":"person","pendingFlush":true,
		"joinedSendIds":["person","thread-wake:a-joined",7,null,true,["thread-wake:a-member-array"],{"id":"thread-wake:a-member-object"}]}`)
	user("a", "a-joined-flush-0", 5, `{"pendingFlush":0,"joinedSendIds":["thread-wake:a-joined-flush-0"]}`)
	user("a", "a-joined-string", 5, `{"pendingFlush":true,"joinedSendIds":"thread-wake:a-joined-string"}`)
	user("a", "a-joined-object", 5, `{"pendingFlush":true,"joinedSendIds":{"id":"thread-wake:a-joined-object"}}`)
	user("a", "a-joined-null", 5, `{"sendId":"thread-wake:a-joined-null","pendingFlush":true,"joinedSendIds":null}`)
	user("a", "a-joined-empty", 5, `{"pendingFlush":true,"joinedSendIds":[]}`)
	user("a", "a-both", 5, `{"sendId":"thread-wake:a-both","pendingFlush":true,"joinedSendIds":["thread-wake:a-both","thread-wake:a-both-joined"]}`)
	user("a", "a-shared", 5, `{"sendId":"thread-wake:shared","pendingFlush":true}`)
	row("a", "a-nested", 5, "user_text", "a-wake", `{"sendId":"thread-wake:a-nested","pendingFlush":true,"joinedSendIds":["thread-wake:a-nested-joined"]}`, 200)
	row("a", "a-assistant", 5, "assistant_text", "", `{"sendId":"thread-wake:a-assistant","pendingFlush":true,"joinedSendIds":["thread-wake:a-assistant-joined"]}`, 200)

	user("b", "b-later-turn", 2, `{"sendId":"thread-wake:b-later-turn","pendingFlush":true,"joinedSendIds":["thread-wake:b-later-turn-joined"]}`)
	user("b", "b-later-item", 3, `{"sendId":"thread-wake:b-later-item","pendingFlush":true,"joinedSendIds":["thread-wake:b-later-item-joined"]}`)
	row("b", "b-after", 4, "assistant_text", "", `{}`, 200)

	row("c", "c-before", 2, "user_text", "", `{"sendId":"thread-wake:c-before","pendingFlush":true,"joinedSendIds":["thread-wake:c-before-joined"]}`, 400)
	row("c", "c-at", 2, "user_text", "", `{"sendId":"thread-wake:c-at","pendingFlush":true,"joinedSendIds":["thread-wake:c-at-joined"]}`, 500)

	user("d", "d-shared", 1, `{"sendId":"thread-wake:shared","pendingFlush":true}`)
	user("d", "d-joined", 1, `{"pendingFlush":true,"joinedSendIds":["thread-wake:d-joined"]}`)

	user("e", "e-wake", 2, `{"sendId":"thread-wake:e-wake","pendingFlush":true}`)
	row("e", "e-child", 3, "tool_call", "e-wake", `{}`, 200)

	queued := func(thread, sendID string) {
		t.Helper()
		mustExec(t, db, `INSERT INTO flush_queue_items (id, thread_id, send_id, message, enqueued_at) VALUES (?, ?, ?, 'text', 1)`,
			"queue-"+thread+"-"+sendID, thread, sendID)
	}
	for _, q := range []struct{ name, meta string }{
		{"delivered", `{"sendId":"thread-wake:delivered","provider_item_id":"w"}`},
		{"wire-only", `{"sendId":"thread-wake:wire-only","provider_item_id":"w","wire_only":true}`},
		{"no-provider", `{"sendId":"thread-wake:no-provider","provider_item_id":""}`},
		{"number-provider", `{"sendId":"thread-wake:number-provider","provider_item_id":5}`},
		{"joined", `{"sendId":"person","provider_item_id":"w","joinedSendIds":["person","thread-wake:joined"]}`},
		{"joined-wire-only", `{"provider_item_id":"w","wire_only":1,"joinedSendIds":["thread-wake:joined-wire-only"]}`},
		{"joined-string", `{"provider_item_id":"w","joinedSendIds":"thread-wake:joined-string"}`},
		{"joined-object", `{"provider_item_id":"w","joinedSendIds":{"id":"thread-wake:joined-object"}}`},
	} {
		queued("f", "thread-wake:"+q.name)
		user("f", "f-"+q.name, 1, q.meta)
	}
	queued("f", "thread-wake:invalid")
	user("f", "f-invalid", 1, `{sendId:'thread-wake:invalid',provider_item_id:'w'}`)
	queued("f", "thread-wake:nested")
	row("f", "f-nested", 1, "user_text", "f-delivered", `{"sendId":"thread-wake:nested","provider_item_id":"w"}`, 200)
	queued("f", "thread-wake:assistant")
	row("f", "f-assistant", 1, "assistant_text", "", `{"sendId":"thread-wake:assistant","provider_item_id":"w"}`, 200)
	queued("f", "thread-wake:none")
	queued("f", "person")
	// Delivered in thread e and queued in e and f: f's queued wake stays
	// undelivered.
	queued("f", "thread-wake:elsewhere")
	user("e", "e-elsewhere", 1, `{"sendId":"thread-wake:elsewhere","provider_item_id":"w"}`)
	queued("e", "thread-wake:elsewhere")
}

// runUndeliveredWakes runs stmt into an empty v155_undelivered_wakes and
// returns its rows.
func runUndeliveredWakes(t *testing.T, db *sql.DB, stmt string) []string {
	t.Helper()
	mustExec(t, db, `DROP TABLE IF EXISTS temp.v155_undelivered_wakes`)
	mustExec(t, db, v155UndeliveredWakesTableSQL)
	mustExec(t, db, stmt)
	got, err := queryIDs(db, `SELECT thread_id || '|' || send_id FROM v155_undelivered_wakes ORDER BY thread_id, send_id`)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `DROP TABLE v155_undelivered_wakes`)
	return got
}

// v155's undelivered-wake statements find exactly the send ids the
// statements first written found, over every row shape: the residue rows
// of each thread's last turn, and the queued wakes no delivered row of
// their thread answers for.
func TestMigrationV155UndeliveredWakesParity(t *testing.T) {
	db := migrateThrough(t, assistantsMigrationVersion-1)
	seedV155WakeShapes(t, db)
	for _, c := range []struct {
		name, stmt, reference string
		want                  []string
	}{
		{"residue", v155ResidueWakesSQL, v155ResidueWakesReferenceSQL, []string{
			"a|thread-wake-late:a-late", "a|thread-wake:a-both", "a|thread-wake:a-both-joined", "a|thread-wake:a-joined",
			"a|thread-wake:a-joined-null", "a|thread-wake:a-wake", "a|thread-wake:shared", "a|thread-wakeful",
			"c|thread-wake:c-at", "c|thread-wake:c-at-joined",
			"d|thread-wake:d-joined", "d|thread-wake:shared",
			"e|thread-wake:e-wake",
		}},
		{"queued", v155QueuedWakesSQL, v155QueuedWakesReferenceSQL, []string{
			"f|thread-wake:assistant", "f|thread-wake:elsewhere", "f|thread-wake:invalid", "f|thread-wake:joined-wire-only",
			"f|thread-wake:nested", "f|thread-wake:no-provider", "f|thread-wake:none", "f|thread-wake:wire-only",
		}},
	} {
		got := runUndeliveredWakes(t, db, c.stmt)
		reference := runUndeliveredWakes(t, db, c.reference)
		if !slices.Equal(got, reference) {
			t.Errorf("%s statement:\n got %q\nreference %q", c.name, got, reference)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s statement:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// queryPlan is the EXPLAIN QUERY PLAN of stmt, one "SCAN ..." or
// "SEARCH ..." detail per line.
func queryPlan(t *testing.T, db *sql.DB, stmt string) []string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN " + stmt)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan
}

// v155's undelivered-wake statements read items only through the sparse
// send-id indexes, on a schema with no sqlite_stat1: the residue arms scan
// idx_items_send_id and idx_items_joined_send_ids for their candidates
// rather than every top-level user_text row, and each queued wake probes
// its thread's entries in the same two indexes. The other reads of the
// residue rule are keyed probes per candidate.
func TestMigrationV155UndeliveredWakesReadSparseIndexes(t *testing.T) {
	db := migrateThrough(t, assistantsMigrationVersion-1)
	mustExec(t, db, v155UndeliveredWakesTableSQL)
	for _, c := range []struct {
		name, stmt string
		// alias is the items alias whose every read must be one of want,
		// each read once.
		alias string
		want  []string
	}{
		{"residue", v155ResidueWakesSQL, "u", []string{
			"SCAN u USING INDEX idx_items_send_id",
			"SCAN u USING INDEX idx_items_joined_send_ids",
		}},
		{"queued", v155QueuedWakesSQL, "d", []string{
			"SEARCH d USING INDEX idx_items_send_id (thread_id=? AND <expr>=?)",
			"SEARCH d USING INDEX idx_items_joined_send_ids (thread_id=?)",
		}},
	} {
		plan := queryPlan(t, db, c.stmt)
		text := strings.Join(plan, "\n")
		reads := regexp.MustCompile(`^(SCAN|SEARCH) ` + c.alias + `\b`)
		var got []string
		for _, detail := range plan {
			if reads.MatchString(detail) {
				got = append(got, detail)
			}
		}
		slices.Sort(got)
		want := slices.Sorted(slices.Values(c.want))
		if !slices.Equal(got, want) {
			t.Errorf("the %s statement reads items as %q, want %q:\n%s", c.name, got, want, text)
		}
		for _, detail := range plan {
			if strings.HasPrefix(detail, "SCAN ") && !reads.MatchString(detail) && !strings.Contains(detail, "VIRTUAL TABLE") &&
				!strings.HasPrefix(detail, "SCAN q") {
				t.Errorf("the %s statement scans %q:\n%s", c.name, detail, text)
			}
		}
	}

	// The send-id arm tests the wake prefix on the index entry: from the
	// deferred seek of a candidate's table row, the glob runs before any
	// column of that row is read, so a row whose send id is no wake is never
	// read from the table.
	var root int64
	if err := db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name = 'idx_items_send_id'`).Scan(&root); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("EXPLAIN " + v155ResidueWakesSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var program strings.Builder
	index, table := int64(-1), int64(-1)
	tested, read := false, false
	for rows.Next() {
		var addr, p1, p2, p3 int64
		var opcode string
		var p4, p5, comment sql.NullString
		if err := rows.Scan(&addr, &opcode, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&program, "%d %s %d %d %d %s\n", addr, opcode, p1, p2, p3, p4.String)
		switch {
		case index < 0 && opcode == "OpenRead" && p2 == root:
			index = p1
		case index >= 0 && table < 0 && opcode == "DeferredSeek" && p1 == index:
			table = p3
		case table >= 0 && !tested && !read && opcode == "Function" && strings.HasPrefix(p4.String, "glob("):
			tested = true
		case table >= 0 && !tested && !read && opcode == "Column" && p1 == table:
			read = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if table < 0 || !tested || read {
		t.Errorf("the send-id arm reads a candidate's table row before its wake prefix test (index cursor %d, table cursor %d):\n%s",
			index, table, program.String())
	}
}
