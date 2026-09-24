package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type storedPayloadRow struct {
	Kind, Meta, Data, PreviewSpans, Spans string
	CreatedAt                             int64
}

func storedPayload(t *testing.T, s *Store, thread, id string) storedPayloadRow {
	t.Helper()
	var row storedPayloadRow
	if err := s.db.QueryRow(`SELECT kind, meta, CAST(data AS TEXT), preview_spans, spans, created_at FROM payloads WHERE thread_id = ? AND id = ?`, thread, id).
		Scan(&row.Kind, &row.Meta, &row.Data, &row.PreviewSpans, &row.Spans, &row.CreatedAt); err != nil {
		t.Fatalf("read payload %s/%s: %v", thread, id, err)
	}
	return row
}

// transcriptCase writes a launch and its completion, whose payload has the
// given kind, meta and data, with highlight spans.
type transcriptCase struct {
	id         string
	tool       string
	background bool
	kind       string
	meta       string
	data       string
	// detached writes a tool_completion row that names no launch.
	detached bool
}

const loadedTranscriptMeta = `{"outputFile":"/tmp/agent.output","outputFileState":"loaded","preview":"The report"}`

func writeTranscriptCase(t *testing.T, s *Store, thread string, turn int, c transcriptCase) {
	t.Helper()
	launch := Item{
		ID: c.id + "-launch", ThreadID: thread, TurnIndex: turn, ItemIndex: 0, Kind: "tool_call", Role: "assistant",
		Status: "running", IsBackground: c.background, ToolName: c.tool, Summary: c.tool + ": work", Meta: "{}", CreatedAt: 1, UpdatedAt: 1,
	}
	if _, err := s.UpsertItem(launch, nil); err != nil {
		t.Fatal(err)
	}
	completion := Item{
		ID: c.id, ThreadID: thread, TurnIndex: turn, ItemIndex: 1, Kind: "tool_completion", Role: "assistant",
		Status: "completed", IsBackground: c.background, CompletionOf: launch.ID, ToolName: c.tool, Summary: c.tool + ": done",
		Meta: "{}", CreatedAt: 2, UpdatedAt: 2,
	}
	if c.detached {
		completion.CompletionOf = ""
	}
	payload := &Payload{ID: "p-" + c.id, Kind: c.kind, Meta: c.meta, Data: []byte(c.data), CreatedAt: 3}
	if _, err := s.UpsertItem(completion, payload); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePayloadSpans(thread, payload.ID, `{"preview":1}`, `{"full":1}`); err != nil {
		t.Fatal(err)
	}
}

type itemRev struct {
	ID  string
	Rev int64
}

func itemRevs(t *testing.T, s *Store, thread string) []itemRev {
	t.Helper()
	rows, err := s.db.Query(`SELECT id, rev FROM items WHERE thread_id = ? ORDER BY id`, thread)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var revs []itemRev
	for rows.Next() {
		var rev itemRev
		if err := rows.Scan(&rev.ID, &rev.Rev); err != nil {
			t.Fatal(err)
		}
		revs = append(revs, rev)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return revs
}

func runTranscriptBlank(t *testing.T, s *Store) (*deferredRun, int) {
	t.Helper()
	pauses := 0
	run := &deferredRun{host: DeferredHost{Pause: func() { pauses++ }}}
	if err := blankLegacyTranscriptCopies(context.Background(), s, run); err != nil {
		t.Fatal(err)
	}
	return run, pauses
}

// The step empties the data of a Claude background agent's transcript copy
// and keeps everything else about the payload and its rows. Every other
// payload keeps its data.
func TestBlankLegacyTranscriptCopies(t *testing.T) {
	s := newTestStore(t)
	for _, thread := range []Thread{makeThread("claude", "claude"), makeThread("codex", "codex")} {
		if err := s.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
	}
	blanked := []transcriptCase{
		{id: "agent", tool: "Agent", background: true, kind: "tool_call_result", meta: loadedTranscriptMeta, data: "agent transcript"},
		{id: "task", tool: "Task", background: true, kind: "tool_call_result", meta: loadedTranscriptMeta, data: "task transcript"},
		{id: "send", tool: "SendMessage", background: true, kind: "tool_call_result", meta: loadedTranscriptMeta, data: "resumed transcript"},
	}
	kept := []transcriptCase{
		// A current build's payload: already empty.
		{id: "current", tool: "Agent", background: true, kind: "tool_call_result", meta: loadedTranscriptMeta},
		// A background command's captured output.
		{id: "command", tool: "Bash", background: true, kind: "command_output", meta: `{"outputFileState":"loaded"}`, data: "command output"},
		// A Monitor's output is what it watched, not a transcript.
		{id: "monitor", tool: "Monitor", background: true, kind: "tool_call_result", meta: loadedTranscriptMeta, data: "monitor output"},
		// A completion body that did not come from the output file.
		{id: "body", tool: "Agent", background: true, kind: "tool_call_result", meta: `{"preview":"The report"}`, data: "terminal body"},
		// A foreground completion.
		{id: "foreground", tool: "Agent", kind: "tool_call_result", meta: loadedTranscriptMeta, data: "foreground result"},
		// Meta that is not JSON.
		{id: "badmeta", tool: "Agent", background: true, kind: "tool_call_result", meta: "not json", data: "unparsed"},
		// A completion row that completes no launch.
		{id: "detached", tool: "Agent", background: true, kind: "tool_call_result", meta: loadedTranscriptMeta, data: "detached result", detached: true},
	}
	for i, c := range append(append([]transcriptCase{}, blanked...), kept...) {
		writeTranscriptCase(t, s, "claude", i, c)
	}
	writeTranscriptCase(t, s, "codex", 0, transcriptCase{id: "codex", tool: "Agent", background: true, kind: "tool_call_result", meta: loadedTranscriptMeta, data: "codex result"})
	// A payload only a notification row names.
	notification := Item{ID: "note", ThreadID: "claude", TurnIndex: 20, ItemIndex: 0, Kind: "notification", Role: "system",
		Status: "completed", Summary: "Agent finished", Meta: "{}", CreatedAt: 1, UpdatedAt: 1}
	if _, err := s.UpsertItem(notification, &Payload{ID: "p-note", Kind: "tool_call_result", Meta: loadedTranscriptMeta, Data: []byte("notified transcript"), CreatedAt: 3}); err != nil {
		t.Fatal(err)
	}

	type view struct {
		payloads map[string]storedPayloadRow
		revs     []itemRev
		stamp    HistoryStamp
	}
	read := func() view {
		v := view{payloads: map[string]storedPayloadRow{}, revs: itemRevs(t, s, "claude"), stamp: historyStamp(t, s, "claude")}
		for _, c := range append(append([]transcriptCase{}, blanked...), kept...) {
			v.payloads[c.id] = storedPayload(t, s, "claude", "p-"+c.id)
		}
		v.payloads["note"] = storedPayload(t, s, "claude", "p-note")
		v.payloads["codex"] = storedPayload(t, s, "codex", "p-codex")
		return v
	}
	before := read()

	run, _ := runTranscriptBlank(t, s)
	if run.failures != 0 {
		t.Fatalf("the step failed %d items: %v", run.failures, run.first)
	}
	after := read()
	for id, was := range before.payloads {
		want := was
		for _, c := range blanked {
			if c.id == id {
				want.Data, want.Spans = "", ""
			}
		}
		if got := after.payloads[id]; got != want {
			t.Errorf("payload %s = %+v, want %+v", id, got, want)
		}
	}
	if data, err := s.GetPayloadData("claude", "p-agent"); err != nil || len(data) != 0 {
		t.Fatalf("emptied payload reads %q, %v; want an empty payload", data, err)
	}
	requireEmptyBlob(t, s, "payloads", "thread_id = ? AND id = ?", "claude", "p-agent")
	if !reflect.DeepEqual(after.revs, before.revs) || after.stamp != before.stamp {
		t.Fatalf("the step moved item revisions or the thread stamp:\n got %v %+v\nwant %v %+v", after.revs, after.stamp, before.revs, before.stamp)
	}

	// Idempotent: a second run finds nothing and changes nothing.
	run, pauses := runTranscriptBlank(t, s)
	if run.failures != 0 || pauses != 0 {
		t.Fatalf("second run: failures %d, pauses %d", run.failures, pauses)
	}
	if again := read(); !reflect.DeepEqual(again.payloads, after.payloads) || !reflect.DeepEqual(again.revs, after.revs) || again.stamp != after.stamp {
		t.Fatal("the second run changed the thread")
	}
}

// A payload listed before its thread or row changed is re-checked inside the
// blanking transaction and kept.
func TestBlankLegacyTranscriptBatchRechecksTheSelection(t *testing.T) {
	s := newTestStore(t)
	for _, thread := range []Thread{makeThread("switched", "claude"), makeThread("claude", "claude")} {
		if err := s.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
	}
	for _, thread := range []string{"switched", "claude"} {
		writeTranscriptCase(t, s, thread, 0, transcriptCase{id: "agent", tool: "Agent", background: true,
			kind: "tool_call_result", meta: loadedTranscriptMeta, data: "agent transcript"})
	}
	listed, err := s.legacyTranscriptPayloads(context.Background())
	if err != nil || len(listed) != 2 {
		t.Fatalf("listed = %+v, %v; want both payloads", listed, err)
	}
	if err := s.UpdateProvider("switched", "codex"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `UPDATE payloads SET meta = '{"outputFileState":"pending"}' WHERE thread_id = 'claude' AND id = 'p-agent'`)

	count, bytes, err := s.blankLegacyTranscriptBatch(listed)
	if err != nil || count != 0 || bytes != 0 {
		t.Fatalf("blanked %d payloads (%d bytes), %v; want none", count, bytes, err)
	}
	for _, thread := range []string{"switched", "claude"} {
		if got := storedPayload(t, s, thread, "p-agent"); got.Data != "agent transcript" {
			t.Fatalf("payload %s/p-agent = %+v, want its data kept", thread, got)
		}
	}
}

// Each transaction empties at most transcriptBlankRows payloads and
// historyRepairBytes bytes, with a pause between transactions. A batch that
// fails is reported and left, and the step goes on.
func TestBlankLegacyTranscriptCopiesIsPacedAndSkipsAFailingBatch(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("claude", "claude")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < transcriptBlankRows+1; i++ {
		writeTranscriptCase(t, s, "claude", i, transcriptCase{id: fmt.Sprintf("small-%03d", i), tool: "Agent", background: true,
			kind: "tool_call_result", meta: loadedTranscriptMeta, data: "transcript"})
	}
	large := strings.Repeat("x", historyRepairBytes/2+1)
	for i := 0; i < 2; i++ {
		writeTranscriptCase(t, s, "claude", 100+i, transcriptCase{id: fmt.Sprintf("tail-large-%d", i), tool: "Agent", background: true,
			kind: "tool_call_result", meta: loadedTranscriptMeta, data: large})
	}
	mustExec(t, s.db, `CREATE TRIGGER fail_blank BEFORE UPDATE OF data ON payloads WHEN OLD.id = 'p-small-000' BEGIN SELECT RAISE(ABORT, 'injected'); END`)

	run, pauses := runTranscriptBlank(t, s)
	// 64 small rows, 1 small row with the first large one, the second large one.
	if pauses != 2 {
		t.Fatalf("the step paused %d times, want 2 (three transactions)", pauses)
	}
	if run.failures != 1 || !strings.Contains(run.first.Error(), "claude/p-small-000") || !strings.Contains(run.first.Error(), "injected") {
		t.Fatalf("failures = %d, first = %v", run.failures, run.first)
	}
	if n := countRows(t, s, `SELECT count(*) FROM payloads WHERE id LIKE 'p-small-%' AND length(data) > 0`); n != transcriptBlankRows {
		t.Fatalf("%d small payloads kept their data, want the failed batch of %d", n, transcriptBlankRows)
	}
	if n := countRows(t, s, `SELECT count(*) FROM payloads WHERE id LIKE 'p-tail-%' AND length(data) > 0`); n != 0 {
		t.Fatalf("%d payloads after the failed batch kept their data", n)
	}

	mustExec(t, s.db, `DROP TRIGGER fail_blank`)
	if run, _ := runTranscriptBlank(t, s); run.failures != 0 {
		t.Fatalf("the retry failed: %v", run.first)
	}
	if n := countRows(t, s, `SELECT count(*) FROM payloads WHERE length(data) > 0`); n != 0 {
		t.Fatalf("%d payloads kept their data after the retry", n)
	}
}
