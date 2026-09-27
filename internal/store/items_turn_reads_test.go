package store

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// seedNarrowTurnReads writes turn 4 of the keyed lookup thread (imported
// and local history) with sequenced user rows, running rows, an agent's
// child and provider item ids, and a pointer fork of the thread cut inside
// turn 4. Past the cut the fork writes rows of its own that match the same
// keys as the thread's rows past it (a running row, a streaming row and a
// provider item id), and the thread writes more matching rows after the
// fork: a read that loses the cut returns the thread's rows.
func seedNarrowTurnReads(t *testing.T, s *Store) {
	t.Helper()
	seedKeyedLookupThread(t, s)
	const at = int64(1_700_000_004_100)
	insert := func(index int, item Item) {
		t.Helper()
		item.ThreadID, item.TurnIndex, item.ItemIndex, item.CreatedAt = keyedThreadID, 4, index, at+int64(index)
		if item.Meta == "" {
			item.Meta = "{}"
		}
		if err := insertCarded(s, item); err != nil {
			t.Fatalf("seed %s: %v", item.ID, err)
		}
	}
	for i, item := range []Item{
		{ID: "user:4:flush:1", Kind: "user_text", Role: "user", Status: "completed"},
		{ID: "user:4:flush:10", Kind: "user_text", Role: "user", Status: "completed"},
		{ID: "user:4:flushed", Kind: "user_text", Role: "user", Status: "completed"},
		{ID: "user:4:steer:1", Kind: "user_text", Role: "user", Status: "completed"},
		{ID: "running-tool", Kind: "tool_call", ToolName: "Bash", Role: "assistant", Status: "running"},
		{ID: "top-answer", Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "the answer", Meta: `{"provider_item_id":"dup"}`},
		{ID: "agent-launch", Kind: "tool_call", ToolName: "Task", Role: "assistant", Status: "running", Meta: `{"task_id":"t"}`},
		{ID: "agent-answer", Kind: "assistant_text", Role: "assistant", Status: "streaming", ParentID: "agent-launch", Meta: `{"provider_item_id":"agent"}`},
	} {
		insert(2+i, item)
	}
	if err := s.CreatePointerFork(makeThread(keyedForkID, "claude"), keyedThreadID, ForkCut{BeforeItemID: "running-tool"}, testInterruptedSummary, 1); err != nil {
		t.Fatal(err)
	}
	for _, item := range []Item{
		{ID: "user:4:flush:3", Kind: "user_text", Role: "user", Status: "completed", Summary: "fork"},
		{ID: "fork-tool", Kind: "tool_call", ToolName: "Bash", Role: "assistant", Status: "running"},
		{ID: "fork-answer", Kind: "assistant_text", Role: "assistant", Status: "streaming", Meta: `{"provider_item_id":"dup"}`},
	} {
		item.ThreadID, item.TurnIndex = keyedForkID, 4
		if item.Meta == "" {
			item.Meta = "{}"
		}
		if _, err := appendCarded(s, item); err != nil {
			t.Fatalf("seed fork %s: %v", item.ID, err)
		}
	}
	insert(20, Item{ID: "user:4:flush:2", Kind: "user_text", Role: "user", Status: "completed"})
	insert(21, Item{ID: "late-answer", Kind: "assistant_text", Role: "assistant", Status: "streaming", Meta: `{"provider_item_id":"dup"}`})
}

// narrowViewRow is one row of a turn as the timeline_items view reads it.
type narrowViewRow struct {
	id, kind, parent, status, tool, provider string
	item                                     int
}

// narrowViewTurn reads threadID's turn through the timeline_items view, in
// timeline order: the oracle the narrow reads are compared against.
func narrowViewTurn(t *testing.T, s *Store, threadID string, turn int) []narrowViewRow {
	t.Helper()
	rows, err := s.db.Query(`SELECT id, kind, parent_id, status, tool_name,
	        COALESCE(CASE WHEN json_valid(meta) THEN json_extract(meta, '$.provider_item_id') END, ''), item_index
	   FROM timeline_items WHERE thread_id = ? AND turn_index = ? ORDER BY item_index`, threadID, turn)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []narrowViewRow
	for rows.Next() {
		var r narrowViewRow
		if err := rows.Scan(&r.id, &r.kind, &r.parent, &r.status, &r.tool, &r.provider, &r.item); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The narrow turn reads return the rows the timeline_items view holds, in
// its order, on a thread with imported and local rows and on a pointer
// fork of it cut inside a turn. The hydrated rows are the ones the whole
// turn read serves.
func TestNarrowTurnReadsMatchTheView(t *testing.T) {
	s := newTestStore(t)
	seedNarrowTurnReads(t, s)
	viewIDs := func(view []narrowViewRow, keep func(narrowViewRow) bool) []string {
		var ids []string
		for _, row := range view {
			if keep(row) {
				ids = append(ids, row.id)
			}
		}
		return ids
	}
	sameRows := func(a, b Item) bool {
		return a.ID == b.ID && a.Summary == b.Summary && a.Rev == b.Rev && a.Meta == b.Meta
	}
	for _, threadID := range []string{keyedThreadID, keyedForkID} {
		for turn := range 6 {
			view := narrowViewTurn(t, s, threadID, turn)
			all, err := s.ListTurnItems(threadID, turn)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := itemIDs(all), viewIDs(view, func(narrowViewRow) bool { return true }); !slices.Equal(got, want) {
				t.Errorf("%s turn %d rows = %v, the view holds %v", threadID, turn, got, want)
			}
			served := func(item Item) Item {
				if j := slices.IndexFunc(all, func(row Item) bool { return row.ID == item.ID }); j >= 0 {
					return all[j]
				}
				return Item{}
			}

			for _, prefix := range []string{"user:4:flush:", "user:4:steer:", "answer-"} {
				got, err := s.TurnItemIDsWithPrefix(threadID, turn, prefix)
				if err != nil {
					t.Fatal(err)
				}
				want := viewIDs(view, func(row narrowViewRow) bool { return strings.HasPrefix(row.id, prefix) })
				slices.Sort(got)
				slices.Sort(want)
				if !slices.Equal(got, want) {
					t.Errorf("%s turn %d prefix %q = %v, the view holds %v", threadID, turn, prefix, got, want)
				}
			}

			unsettled, err := s.ListUnsettledTurnItems(threadID, turn)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := itemIDs(unsettled), viewIDs(view, func(row narrowViewRow) bool { return row.status == "running" || row.status == "streaming" }); !slices.Equal(got, want) {
				t.Errorf("%s turn %d unsettled = %v, the view holds %v", threadID, turn, got, want)
			}
			for _, item := range unsettled {
				if !sameRows(item, served(item)) {
					t.Errorf("%s turn %d unsettled row %+v, the turn serves %+v", threadID, turn, item, served(item))
				}
			}

			for _, kind := range []string{"user_text", "assistant_text", "tool_call"} {
				got, err := s.ListTurnItemsOfKind(threadID, turn, kind)
				if err != nil {
					t.Fatal(err)
				}
				if want := viewIDs(view, func(row narrowViewRow) bool { return row.kind == kind }); !slices.Equal(itemIDs(got), want) {
					t.Errorf("%s turn %d %s rows = %v, the view holds %v", threadID, turn, kind, itemIDs(got), want)
				}
				for _, item := range got {
					if !sameRows(item, served(item)) {
						t.Errorf("%s turn %d %s row %+v, the turn serves %+v", threadID, turn, kind, item, served(item))
					}
				}
			}

			wantLast := viewIDs(view, func(row narrowViewRow) bool { return row.kind == "assistant_text" && row.parent == "" })
			last, found, err := s.LastTopLevelTurnItem(threadID, turn, "assistant_text")
			if err != nil {
				t.Fatal(err)
			}
			if found != (len(wantLast) > 0) || (found && last.ID != wantLast[len(wantLast)-1]) {
				t.Errorf("%s turn %d last top-level answer = %q (found %v), the view holds %v", threadID, turn, last.ID, found, wantLast)
			}

			for _, key := range []struct{ kind, parent, provider string }{
				{"assistant_text", "", "dup"},
				{"assistant_text", "agent-launch", "agent"},
				{"assistant_text", "", "prov-" + strconv.Itoa(turn)},
			} {
				want := viewIDs(view, func(row narrowViewRow) bool {
					return row.kind == key.kind && row.parent == key.parent && row.provider == key.provider
				})
				got, found, err := s.FindStreamItemByProviderItemID(threadID, turn, key.kind, key.parent, key.provider)
				if err != nil {
					t.Fatal(err)
				}
				if found != (len(want) > 0) || (found && got.ID != want[0]) {
					t.Errorf("%s turn %d stream item %+v = %q (found %v), the view holds %v", threadID, turn, key, got.ID, found, want)
				}
			}

			maxIndex, found, err := s.MaxItemIndexForTurn(threadID, turn)
			if err != nil {
				t.Fatal(err)
			}
			if found != (len(view) > 0) || (found && maxIndex != view[len(view)-1].item) {
				t.Errorf("%s turn %d max item index = %d (found %v), the view's rows end at %v", threadID, turn, maxIndex, found, view)
			}
		}
	}

	// The fork's own rows past its cut match the keys the thread's rows past
	// it match, so a read that loses the cut disagrees with the view above.
	if got, _ := s.ListUnsettledTurnItems(keyedForkID, 4); !slices.Equal(itemIDs(got), []string{"fork-tool", "fork-answer"}) {
		t.Errorf("fork unsettled rows = %v, want only its own", itemIDs(got))
	}
	if got, _ := s.ListUnsettledTurnItems(keyedThreadID, 4); len(got) < 4 {
		t.Errorf("thread unsettled rows = %v; the fixture needs the thread's past the fork's cut", itemIDs(got))
	}
	if got, _, _ := s.FindStreamItemByProviderItemID(keyedForkID, 4, "assistant_text", "", "dup"); got.ID != "fork-answer" {
		t.Errorf("fork stream item = %q, want its own fork-answer", got.ID)
	}
	if got, _, _ := s.FindStreamItemByProviderItemID(keyedThreadID, 4, "assistant_text", "", "dup"); got.ID != "top-answer" {
		t.Errorf("thread stream item = %q, want top-answer, which sits past the fork's cut before fork-answer", got.ID)
	}
	if ids, err := s.TurnItemIDsWithPrefix(keyedForkID, 4, "user:4:flush:"); err != nil || len(ids) != 3 {
		t.Errorf("fork flush ids = %v, %v; want its own and the two it inherits", ids, err)
	}
	if _, err := s.TurnItemIDsWithPrefix(keyedThreadID, 4, ""); err == nil {
		t.Error("an empty prefix read the turn")
	}
}

// The narrow turn reads select their rows off the turn's partial or
// top-level indexes, on a thread and on a fork. A read the hydrator orders
// (queryHydratedTimelineItems) selects without a sorter or merge of its
// own, and the last top-level row is chosen on the covering top-level
// indexes, which hold no subagent child row.
func TestNarrowTurnReadsPlans(t *testing.T) {
	s := newTestStore(t)
	seedNarrowTurnReads(t, s)
	rec := recordStatements(t, s)
	for _, threadID := range []string{keyedThreadID, keyedForkID} {
		levels := 1
		if threadID == keyedForkID {
			levels = 2
		}
		reads := []struct {
			name     string
			read     func() error
			topLevel bool
		}{
			{"unsettled", func() error { _, err := s.ListUnsettledTurnItems(threadID, 4); return err }, false},
			{"of kind", func() error { _, err := s.ListTurnItemsOfKind(threadID, 4, "user_text"); return err }, false},
			{"last top-level", func() error { _, _, err := s.LastTopLevelTurnItem(threadID, 4, "assistant_text"); return err }, true},
		}
		for _, tc := range reads {
			t.Run(threadID+"/"+tc.name, func(t *testing.T) {
				var readErr error
				stmts := rec.capture(func() { readErr = tc.read() })
				if readErr != nil {
					t.Fatal(readErr)
				}
				checked := 0
				for _, stmt := range stmts {
					plan := explainPlan(t, s, stmt.query, stmt.args...)
					selection := planSubtree(plan, "MATERIALIZE selected")
					if len(selection) == 0 {
						continue
					}
					checked++
					local, imported := 0, 0
					for _, r := range selection {
						switch {
						case strings.HasPrefix(r.detail, "SEARCH items USING COVERING INDEX idx_items_top_level "):
							local++
						case strings.HasPrefix(r.detail, "SEARCH items USING COVERING INDEX idx_import_history_items_top_level "):
							imported++
						case tc.topLevel && (strings.HasPrefix(r.detail, "SEARCH items ") || strings.HasPrefix(r.detail, "SCAN items")):
							t.Errorf("the selection reads items off the top-level indexes: %q\n%s", r.detail, planText(plan))
						case !tc.topLevel && (strings.Contains(r.detail, "USE TEMP B-TREE") || strings.HasPrefix(r.detail, "MERGE")):
							t.Errorf("the selection orders rows the hydrator orders: %q\n%s", r.detail, planText(plan))
						}
					}
					if tc.topLevel && (local != levels || imported != levels) {
						t.Errorf("local arms on idx_items_top_level = %d, imported arms on idx_import_history_items_top_level = %d, want %d each\n%s",
							local, imported, levels, planText(plan))
					}
				}
				if checked != 1 {
					t.Errorf("found %d hydrated selections in %d statements, want 1", checked, len(stmts))
				}
			})
		}
	}
}

// planSubtree returns the plan nodes below the node whose detail is root.
func planSubtree(plan []planRow, root string) []planRow {
	in := map[int]bool{}
	var out []planRow
	for _, r := range plan {
		switch {
		case r.detail == root:
			in[r.id] = true
		case in[r.parent]:
			in[r.id] = true
			out = append(out, r)
		}
	}
	return out
}

// The rows a turn diff can upgrade are the turn's own rows whose payload is
// a summary-only tool result: what filtering the whole turn finds, on a
// thread and on a pointer fork's own turn.
func TestSummaryOnlyDiffItemsAreTheTurnsCandidates(t *testing.T) {
	s := newTestStore(t)
	const threadID, forkID = "t-diffs", "t-diffs-fork"
	mustCreateThread(t, s, threadID)
	const summaryOnly = `{"itemType":"file_change","inlineDiff":{"availability":"summary_only","files":[{"path":"a.go"}]}}`
	const exact = `{"itemType":"file_change","inlineDiff":{"availability":"exact_patch","files":[{"path":"a.go"}]}}`
	edit := func(thread, id string, turn, index int, kind, meta string) {
		t.Helper()
		item := Item{
			ID: id, ThreadID: thread, TurnIndex: turn, ItemIndex: index, Kind: "tool_call", ToolName: "file_change",
			Role: "assistant", Status: "completed", Summary: id, Meta: "{}", PayloadID: "tool-result:" + id,
			CreatedAt: int64(1_700_000_000_000 + turn*1000 + index),
		}
		if err := insertWithPayloadCarded(s, item, Payload{ID: item.PayloadID, Kind: kind, Meta: meta, Data: []byte("d"), CreatedAt: item.CreatedAt}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	edit(threadID, "summary-0", 0, 0, "tool_result", summaryOnly)
	edit(threadID, "summary-1", 1, 0, "tool_result", summaryOnly)
	edit(threadID, "exact-1", 1, 1, "tool_result", exact)
	edit(threadID, "diff-kind-1", 1, 2, "diff", summaryOnly)
	// A payload meta that is not JSON is stored and is no candidate.
	edit(threadID, "malformed-1", 1, 3, "tool_result", "not json")
	edit(threadID, "summary-1b", 1, 4, "tool_result", summaryOnly)
	if err := insertCarded(s, Item{ID: "answer-1", ThreadID: threadID, TurnIndex: 1, ItemIndex: 5, Kind: "assistant_text", Role: "assistant", Status: "completed", Meta: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePointerFork(makeThread(forkID, "claude"), threadID, ForkCut{}, testInterruptedSummary, 1); err != nil {
		t.Fatal(err)
	}
	edit(forkID, "fork-summary-2", 2, 0, "tool_result", summaryOnly)
	edit(forkID, "fork-exact-2", 2, 1, "tool_result", exact)

	for _, tc := range []struct {
		thread string
		turn   int
	}{{threadID, 0}, {threadID, 1}, {threadID, 2}, {forkID, 2}} {
		all, err := s.ListTurnItems(tc.thread, tc.turn)
		if err != nil {
			t.Fatal(err)
		}
		var want []Item
		for _, item := range all {
			var meta struct {
				InlineDiff struct {
					Availability string `json:"availability"`
				} `json:"inlineDiff"`
			}
			if item.PayloadKind == "tool_result" && json.Unmarshal([]byte(item.PayloadMeta), &meta) == nil && meta.InlineDiff.Availability == "summary_only" {
				want = append(want, item)
			}
		}
		got, err := s.ListTurnSummaryOnlyDiffItems(tc.thread, tc.turn)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.EqualFunc(got, want, func(a, b Item) bool {
			return a.ID == b.ID && a.PayloadKind == b.PayloadKind && a.PayloadMeta == b.PayloadMeta && a.Summary == b.Summary && a.Rev == b.Rev
		}) {
			t.Errorf("%s turn %d candidates = %v, the turn holds %v", tc.thread, tc.turn, itemIDs(got), itemIDs(want))
		}
	}
	if got, err := s.ListTurnSummaryOnlyDiffItems(threadID, 1); err != nil || !slices.Equal(itemIDs(got), []string{"summary-1", "summary-1b"}) {
		t.Errorf("turn 1 candidates = %v, %v; want summary-1 and summary-1b", itemIDs(got), err)
	}
	// The fork shows the source's turn 1, candidates included, but a turn
	// diff upgrades only the thread's own rows.
	inherited, err := s.ListTurnItems(forkID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(inherited, func(item Item) bool { return item.ID == "summary-1" }) {
		t.Fatalf("fork turn 1 = %v; the fixture must show the inherited candidate", itemIDs(inherited))
	}
	if got, err := s.ListTurnSummaryOnlyDiffItems(forkID, 1); err != nil || len(got) != 0 {
		t.Errorf("fork turn 1 candidates = %v, %v; want none, the fork owns no row of it", itemIDs(got), err)
	}
}
