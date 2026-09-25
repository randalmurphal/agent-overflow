package store

import (
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/itemmeta"
)

// mustPointerFork creates dst as a pointer fork of src cut at cut, settling
// inherited running rows at 999 the way the boot sweep does.
func mustPointerFork(t *testing.T, s *Store, src, dst string, cut ForkCut) {
	t.Helper()
	fork := makeThread(dst, "claude")
	fork.ForkedFromThreadID = src
	if err := s.CreatePointerFork(fork, src, cut, testInterruptedSummary, 999); err != nil {
		t.Fatalf("fork %s from %s: %v", dst, src, err)
	}
}

func throughTurn(turn int) ForkCut { return ForkCut{ThroughTurn: &turn} }

// forkRows lists a thread's timeline.
func forkRows(t *testing.T, s *Store, threadID string) []Item {
	t.Helper()
	rows, err := s.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems(%s): %v", threadID, err)
	}
	return rows
}

// ownRowCount counts the item rows a thread stores itself.
func ownRowCount(t *testing.T, s *Store, threadID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM items WHERE thread_id = ?`, threadID).Scan(&n); err != nil {
		t.Fatalf("count own rows of %s: %v", threadID, err)
	}
	return n
}

// seedForkSource creates src and inserts rows onto it, stamping the thread
// id so a fixture only spells out the shape.
func seedForkSource(t *testing.T, s *Store, src string, rows []Item) {
	t.Helper()
	mustCreateThread(t, s, src)
	for _, it := range rows {
		it.ThreadID = src
		it.CreatedAt, it.UpdatedAt = 1, 1
		if err := insertCarded(s, it); err != nil {
			t.Fatalf("InsertItem %s: %v", it.ID, err)
		}
	}
}

// forkRowsBySummary indexes a fork's rows by summary.
func forkRowsBySummary(t *testing.T, s *Store, threadID string) map[string]Item {
	t.Helper()
	rows := forkRows(t, s, threadID)
	bySummary := make(map[string]Item, len(rows))
	for _, it := range rows {
		if _, dup := bySummary[it.Summary]; dup {
			t.Fatalf("fixture summaries must be unique, %q repeats", it.Summary)
		}
		bySummary[it.Summary] = it
	}
	return bySummary
}

// assertForkLinksResolve pins the fork's result invariant: every non-empty
// parent_id / completion_of on a row it shows names another row it shows.
// `exempt` lists ids the source already carried dangling.
func assertForkLinksResolve(t *testing.T, s *Store, threadID string, exempt ...string) {
	t.Helper()
	rows, err := s.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems(%s): %v", threadID, err)
	}
	known := make(map[string]bool, len(rows)+len(exempt))
	for _, it := range rows {
		known[it.ID] = true
	}
	for _, id := range exempt {
		known[id] = true
	}
	for _, it := range rows {
		for _, link := range []struct{ field, id string }{
			{"parent_id", it.ParentID},
			{"completion_of", it.CompletionOf},
		} {
			if link.id != "" && !known[link.id] {
				t.Errorf("fork row %q has dangling %s = %q", it.Summary, link.field, link.id)
			}
		}
	}
}

func rowPositions(rows []Item) []string {
	out := make([]string, len(rows))
	for i, it := range rows {
		out[i] = fmt.Sprintf("%d:%d:%s", it.TurnIndex, it.ItemIndex, it.Role)
	}
	return out
}

// TestPointerForkInheritsThroughTheCutWithoutCopying pins the fork's shape:
// the rows before the cut read through from the source under the fork's
// thread id, the fork stores no row, and its thread row names the source
// and the cut.
func TestPointerForkInheritsThroughTheCutWithoutCopying(t *testing.T) {
	s := newTestStore(t)
	var rows []Item
	for turn := 0; turn < 3; turn++ {
		rows = append(rows,
			Item{ID: fmt.Sprintf("u%d", turn), TurnIndex: turn, ItemIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: fmt.Sprintf("t%d", turn)},
			Item{ID: fmt.Sprintf("a%d", turn), TurnIndex: turn, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: fmt.Sprintf("r%d", turn)},
		)
	}
	seedForkSource(t, s, "src", rows)

	mustPointerFork(t, s, "src", "sliced", throughTurn(1))
	got := forkRows(t, s, "sliced")
	if want := []string{"0:0:user", "0:1:assistant", "1:0:user", "1:1:assistant"}; fmt.Sprint(rowPositions(got)) != fmt.Sprint(want) {
		t.Fatalf("sliced fork rows = %v, want %v", rowPositions(got), want)
	}
	for _, it := range got {
		if it.ThreadID != "sliced" {
			t.Errorf("inherited row %s reads as thread %q, want the fork", it.ID, it.ThreadID)
		}
	}
	if n := ownRowCount(t, s, "sliced"); n != 0 {
		t.Errorf("fork stores %d rows, want none", n)
	}
	requireIDs(t, "sliced lineage", forkLineage(t, s, "sliced"), []string{"1:src:1:2"})
	var source, title string
	var cutTurn, cutItem int
	if err := s.db.QueryRow(`SELECT fork_source_thread_id, fork_source_title, fork_cut_turn_index, fork_cut_item_index FROM threads WHERE id = 'sliced'`).
		Scan(&source, &title, &cutTurn, &cutItem); err != nil || source != "src" || title != "Thread src" || cutTurn != 1 || cutItem != 2 {
		t.Errorf("fork origin = %q %q %d:%d, %v", source, title, cutTurn, cutItem, err)
	}

	mustPointerFork(t, s, "src", "full", ForkCut{})
	if got := forkRows(t, s, "full"); len(got) != len(rows) {
		t.Errorf("full fork rows = %d, want %d", len(got), len(rows))
	}

	mustPointerFork(t, s, "src", "none", throughTurn(-1))
	if got, err := s.ListItems("none"); err != nil || len(got) != 0 {
		t.Fatalf("fork before turn 0 = %d rows err=%v, want an empty ordinary thread", len(got), err)
	}
	var lineage int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM thread_fork_lineage WHERE thread_id = 'none'`).Scan(&lineage); err != nil || lineage != 0 {
		t.Fatalf("empty fork lineage rows = %d err=%v, want none", lineage, err)
	}

	if got, err := s.ListItems("src"); err != nil || len(got) != len(rows) {
		t.Fatalf("source rows = %d err=%v, want %d untouched", len(got), err, len(rows))
	}
}

// TestPointerForkHidesLiveBackgroundWorkAndSettlesTheRest pins the fork's
// treatment of the source's unfinished rows. A background launch without a
// completion is the source process's live work: the fork hides it with its
// subtree. Every other running row is settled in the fork's own copy. The
// source is untouched.
func TestPointerForkHidesLiveBackgroundWorkAndSettlesTheRest(t *testing.T) {
	s := newTestStore(t)
	seedForkSource(t, s, "src", []Item{
		{ID: "user-0", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "hi"},
		{ID: "asst-1", TurnIndex: 1, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Summary: "hello", Status: "completed"},
		{ID: "bg-run", TurnIndex: 1, ItemIndex: 2, Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: true, Summary: "Agent: audit", ToolName: "Agent"},
		{ID: "bg-child", TurnIndex: 1, ItemIndex: 3, Kind: "tool_call", Role: "assistant", Status: "completed", ParentID: "bg-run", ToolName: "Read", Summary: "Agent > Read: a.go"},
		{ID: "bg-grandchild", TurnIndex: 1, ItemIndex: 4, Kind: "tool_completion", Role: "assistant", Status: "completed", ParentID: "bg-child", CompletionOf: "bg-child", ToolName: "Read", Summary: "Agent > Read: a.go -> done"},
		{ID: "bg-done", TurnIndex: 1, ItemIndex: 5, Kind: "tool_call", Role: "assistant", Status: "completed", IsBackground: true, Summary: "Bash: echo done", ToolName: "Bash"},
		{ID: "inline-run", TurnIndex: 1, ItemIndex: 6, Kind: "tool_call", Role: "assistant", Status: "running", Summary: "Read: /tmp/x", ToolName: "Read"},
		{ID: "stream", TurnIndex: 0, ItemIndex: 0, Kind: "assistant_text", Role: "assistant", Status: "streaming", Summary: ""},
	})

	mustPointerFork(t, s, "src", "fork", ForkCut{})

	dst := forkRowsBySummary(t, s, "fork")
	for _, gone := range []string{"Agent: audit", "Agent > Read: a.go", "Agent > Read: a.go -> done"} {
		if _, ok := dst[gone]; ok {
			t.Errorf("row %q shown, want the live launch's whole subtree hidden", gone)
		}
	}
	if len(dst) != 5 {
		t.Errorf("fork rows = %d, want 5", len(dst))
	}
	if got := dst["Read: /tmp/x — interrupted"]; got.Status != "errored" || got.UpdatedAt != 999 {
		t.Errorf("running inline row in fork = %+v, want settled errored at 999", got)
	}
	if got := dst["Interrupted"]; got.ID != "stream" || got.Status != "errored" {
		t.Errorf("blank streaming row in fork = %+v, want settled as Interrupted", got)
	}
	if got := dst["Bash: echo done"]; got.Status != "completed" {
		t.Errorf("completed background row = %+v, want it verbatim", got)
	}
	if n := ownRowCount(t, s, "fork"); n != 2 {
		t.Errorf("fork stores %d rows, want the two settled copies", n)
	}
	assertForkLinksResolve(t, s, "fork")

	for id, status := range map[string]string{"bg-run": "running", "inline-run": "running", "stream": "streaming"} {
		it, found, err := s.GetThreadItem("src", id)
		if err != nil || !found || it.Status != status {
			t.Errorf("source %s = %q found=%v err=%v, want %q untouched", id, it.Status, found, err, status)
		}
	}
}

// TestPointerForkNoBackgroundRowsShowsEverything guards the no-op branch:
// a source without background rows shows every row, links intact.
func TestPointerForkNoBackgroundRowsShowsEverything(t *testing.T) {
	s := newTestStore(t)
	items := []Item{
		{ID: "user-0", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "hi"},
		{ID: "asst-1", TurnIndex: 1, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Summary: "hello", Status: "completed"},
		{ID: "tool-run", TurnIndex: 1, ItemIndex: 2, Kind: "tool_call", Role: "assistant", Status: "running", Summary: "Edit: foo.ts", ToolName: "Edit"},
		{ID: "tool-done", TurnIndex: 1, ItemIndex: 3, Kind: "tool_call", Role: "assistant", Status: "completed", Summary: "Read: bar.ts", ToolName: "Read"},
		{ID: "sibling", TurnIndex: 1, ItemIndex: 4, Kind: "tool_completion", Role: "assistant", Status: "completed", CompletionOf: "tool-done", Summary: "Read: bar.ts -> done", ToolName: "Read"},
	}
	seedForkSource(t, s, "src", items)
	mustPointerFork(t, s, "src", "fork", ForkCut{})

	dst := forkRows(t, s, "fork")
	if len(dst) != len(items) {
		t.Fatalf("fork rows = %d, want %d", len(dst), len(items))
	}
	for i, it := range dst {
		if it.ID != items[i].ID {
			t.Errorf("fork row %d = %s, want %s", i, it.ID, items[i].ID)
		}
	}
	if dst[4].CompletionOf != "tool-done" {
		t.Errorf("completion_of = %q, want tool-done", dst[4].CompletionOf)
	}
	assertForkLinksResolve(t, s, "fork")
}

// TestPointerForkKeepsSettledBackgroundLaunchSubtree: a background launch's
// terminal is its completion sibling (invariant 24), so a running launch
// with a sibling inside the cut is finished history and shows whole.
func TestPointerForkKeepsSettledBackgroundLaunchSubtree(t *testing.T) {
	s := newTestStore(t)
	seedForkSource(t, s, "src", []Item{
		{ID: "user-0", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "go audit"},
		{ID: "bg-launch", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: true, ToolName: "Agent", Summary: "Agent: audit"},
		{ID: "bg-child", TurnIndex: 1, ItemIndex: 2, Kind: "tool_call", Role: "assistant", Status: "completed", ParentID: "bg-launch", ToolName: "Read", Summary: "Agent > Read: a.go"},
		{ID: "bg-child-done", TurnIndex: 1, ItemIndex: 3, Kind: "tool_completion", Role: "assistant", Status: "completed", ParentID: "bg-child", CompletionOf: "bg-child", ToolName: "Read", Summary: "Agent > Read: a.go -> done"},
		{ID: "bg-launch-done", TurnIndex: 1, ItemIndex: 4, Kind: "tool_completion", Role: "assistant", Status: "completed", IsBackground: true, CompletionOf: "bg-launch", ToolName: "Agent", Summary: "Agent: audit -> done"},
		{ID: "asst-tail", TurnIndex: 1, ItemIndex: 5, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "meanwhile"},
	})
	mustPointerFork(t, s, "src", "fork", ForkCut{})

	dst := forkRowsBySummary(t, s, "fork")
	if len(dst) != 6 {
		t.Errorf("fork rows = %d, want all 6", len(dst))
	}
	if launch := dst["Agent: audit"]; launch.Status != "running" || !launch.IsBackground {
		t.Errorf("launch = status %q bg=%v, want running background verbatim", launch.Status, launch.IsBackground)
	}
	if n := ownRowCount(t, s, "fork"); n != 0 {
		t.Errorf("fork stores %d rows, want none", n)
	}
	assertForkLinksResolve(t, s, "fork")
}

// TestPointerForkHidesLaunchWhoseSiblingIsBeyondTheCut: the sibling only
// settles its launch when it sits inside the cut.
func TestPointerForkHidesLaunchWhoseSiblingIsBeyondTheCut(t *testing.T) {
	s := newTestStore(t)
	seedForkSource(t, s, "src", []Item{
		{ID: "user-0", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "go audit"},
		{ID: "bg-launch", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: true, ToolName: "Bash", Summary: "Bash: make test"},
		{ID: "user-1", TurnIndex: 2, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "and then"},
		{ID: "bg-launch-done", TurnIndex: 3, ItemIndex: 0, Kind: "tool_completion", Role: "assistant", Status: "completed", IsBackground: true, CompletionOf: "bg-launch", ToolName: "Bash", Summary: "Bash: make test -> done"},
	})
	mustPointerFork(t, s, "src", "fork", throughTurn(2))

	dst := forkRowsBySummary(t, s, "fork")
	if _, ok := dst["Bash: make test"]; ok {
		t.Error("launch shown although its settling sibling is beyond the cut")
	}
	if len(dst) != 2 {
		t.Errorf("fork rows = %d, want 2 (user-0 + user-1)", len(dst))
	}
	assertForkLinksResolve(t, s, "fork")
}

// A parked stop settles nothing, so one inside the cut does not keep a
// launch whose ending sibling is beyond it: the launch and its parked stop
// hide together.
func TestPointerForkHidesLaunchWhoseOnlyStopInsideTheCutIsParked(t *testing.T) {
	s := newTestStore(t)
	seedForkSource(t, s, "src", []Item{
		{ID: "user-0", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "go audit"},
		{ID: "bg-launch", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: true, ToolName: "Agent", Summary: "Agent: audit"},
		{ID: "bg-launch-parked", TurnIndex: 2, ItemIndex: 0, Kind: "tool_completion", Role: "assistant", Status: ItemStatusParked, IsBackground: true, CompletionOf: "bg-launch", ToolName: "Agent", Summary: "Agent: audit -> parked"},
		{ID: "user-1", TurnIndex: 2, ItemIndex: 1, Kind: "user_text", Role: "user", Summary: "and then"},
		{ID: "bg-launch-done", TurnIndex: 3, ItemIndex: 0, Kind: "tool_completion", Role: "assistant", Status: "completed", IsBackground: true, CompletionOf: "bg-launch", ToolName: "Agent", Summary: "Agent: audit -> done"},
	})
	mustPointerFork(t, s, "src", "fork", throughTurn(2))

	dst := forkRowsBySummary(t, s, "fork")
	for _, gone := range []string{"Agent: audit", "Agent: audit -> parked"} {
		if _, ok := dst[gone]; ok {
			t.Errorf("row %q shown although the launch's ending sibling is beyond the cut", gone)
		}
	}
	if len(dst) != 2 {
		t.Errorf("fork rows = %d, want 2 (user-0 + user-1)", len(dst))
	}
	assertForkLinksResolve(t, s, "fork")
}

// TestPointerForkHidesNestedRunningLaunchOnly: a live launch nested under a
// finished one takes only its own subtree with it.
func TestPointerForkHidesNestedRunningLaunchOnly(t *testing.T) {
	s := newTestStore(t)
	seedForkSource(t, s, "src", []Item{
		{ID: "user-0", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "review"},
		{ID: "ok-launch", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "completed", ToolName: "Agent", Summary: "Agent: review"},
		{ID: "ok-child", TurnIndex: 1, ItemIndex: 2, Kind: "assistant_text", Role: "assistant", Status: "completed", ParentID: "ok-launch", Summary: "Agent > thinking"},
		{ID: "nested-bg", TurnIndex: 1, ItemIndex: 3, Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: true, ParentID: "ok-launch", ToolName: "Agent", Summary: "Agent > Agent: deep"},
		{ID: "nested-bg-child", TurnIndex: 1, ItemIndex: 4, Kind: "assistant_text", Role: "assistant", Status: "completed", ParentID: "nested-bg", Summary: "Agent > Agent > thinking"},
		{ID: "ok-tool", TurnIndex: 1, ItemIndex: 5, Kind: "tool_call", Role: "assistant", Status: "completed", ParentID: "ok-launch", ToolName: "Read", Summary: "Agent > Read: b.go"},
		{ID: "ok-tool-done", TurnIndex: 1, ItemIndex: 6, Kind: "tool_completion", Role: "assistant", Status: "completed", ParentID: "ok-launch", CompletionOf: "ok-tool", ToolName: "Read", Summary: "Agent > Read: b.go -> done"},
		{ID: "ok-launch-done", TurnIndex: 1, ItemIndex: 7, Kind: "tool_completion", Role: "assistant", Status: "completed", CompletionOf: "ok-launch", ToolName: "Agent", Summary: "Agent: review -> done"},
	})
	mustPointerFork(t, s, "src", "fork", ForkCut{})

	dst := forkRowsBySummary(t, s, "fork")
	for _, gone := range []string{"Agent > Agent: deep", "Agent > Agent > thinking"} {
		if _, ok := dst[gone]; ok {
			t.Errorf("row %q shown, want the nested live launch's subtree hidden", gone)
		}
	}
	if len(dst) != 6 {
		t.Errorf("fork rows = %d, want 6", len(dst))
	}
	assertForkLinksResolve(t, s, "fork")
}

// TestPointerForkPassesThroughUnknownReferences: a reference to an id the
// source never had is pre-existing corruption, shown verbatim.
func TestPointerForkPassesThroughUnknownReferences(t *testing.T) {
	s := newTestStore(t)
	seedForkSource(t, s, "src", []Item{
		{ID: "user-0", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "hi"},
		{ID: "orphan", TurnIndex: 1, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Status: "completed", ParentID: "ghost-parent", Summary: "orphaned child"},
		{ID: "orphan-completion", TurnIndex: 1, ItemIndex: 2, Kind: "tool_completion", Role: "assistant", Status: "completed", CompletionOf: "ghost-call", ToolName: "Read", Summary: "orphaned completion"},
	})
	mustPointerFork(t, s, "src", "fork", ForkCut{})

	dst := forkRowsBySummary(t, s, "fork")
	if len(dst) != 3 {
		t.Fatalf("fork rows = %d, want 3", len(dst))
	}
	if got := dst["orphaned child"].ParentID; got != "ghost-parent" {
		t.Errorf("parent_id = %q, want the unknown reference preserved", got)
	}
	assertForkLinksResolve(t, s, "fork", "ghost-parent", "ghost-call")
}

// TestPointerForkHidesWhatHangsOffHiddenRowsInAnyOrder: a row the fork shows
// never references one it hides, through parent_id or completion_of, and
// whether the reference precedes its target or follows it.
func TestPointerForkHidesWhatHangsOffHiddenRowsInAnyOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []Item
		gone []string
	}{
		{name: "parent before launch", rows: []Item{
			{ID: "early-child", TurnIndex: 1, ItemIndex: 0, Kind: "assistant_text", Role: "assistant", Status: "completed", ParentID: "bg-launch", Summary: "child before its launch"},
			{ID: "bg-launch", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: true, ToolName: "Agent", Summary: "Agent: audit"},
			{ID: "kept", TurnIndex: 1, ItemIndex: 2, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "kept"},
		}, gone: []string{"early-child", "bg-launch"}},
		{name: "completion before its tool", rows: []Item{
			{ID: "early-done", TurnIndex: 1, ItemIndex: 0, Kind: "tool_completion", Role: "assistant", Status: "completed", CompletionOf: "nested-tool", ToolName: "Read", Summary: "done before its tool"},
			{ID: "bg-launch", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", Role: "assistant", Status: "running", IsBackground: true, ToolName: "Agent", Summary: "Agent: audit"},
			{ID: "nested-tool", TurnIndex: 1, ItemIndex: 2, Kind: "tool_call", Role: "assistant", Status: "completed", ParentID: "bg-launch", ToolName: "Read", Summary: "Agent > Read: a.go"},
			{ID: "kept", TurnIndex: 1, ItemIndex: 3, Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: "kept"},
		}, gone: []string{"early-done", "bg-launch", "nested-tool"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			seedForkSource(t, s, "src", tc.rows)
			mustPointerFork(t, s, "src", "fork", ForkCut{})
			shown := map[string]bool{}
			for _, it := range forkRows(t, s, "fork") {
				shown[it.ID] = true
			}
			for _, id := range tc.gone {
				if shown[id] {
					t.Errorf("row %s shown, want it hidden with the live launch", id)
				}
			}
			if !shown["kept"] {
				t.Error("unrelated row hidden")
			}
			assertForkLinksResolve(t, s, "fork")
		})
	}
}

// TestPointerForkPayloadsStayWithTheirRows pins the payload half: the fork
// reads an inherited row's payloads from the source, the source cannot
// rewrite a payload its forks show, and an edit snapshot is a cache
// written where the payload lives.
func TestPointerForkPayloadsStayWithTheirRows(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "src")
	if err := insertWithPayloadCarded(s, Item{
		ID: "edit", ThreadID: "src", TurnIndex: 0, ItemIndex: 0, Kind: "tool_call", Role: "assistant",
		Status: "completed", Summary: "Edit foo.go", ToolName: "Edit", PayloadID: "p-out", CreatedAt: 1, UpdatedAt: 1,
	}, Payload{ID: "p-out", Kind: "tool_result", Meta: "{}", Data: []byte("result"), CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := seedPayloadRow(s, "src", Payload{ID: "p-in", Kind: "tool_call_input", Meta: "{}", Data: []byte(`{"old_string":"a",`), CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendPayloadData("src", "p-in", []byte(`"new_string":"b"}`), "{}", 1); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `UPDATE items SET input_payload_id = 'p-in' WHERE thread_id = 'src' AND id = 'edit'`)
	if err := s.PutEditFileSnapshot("src", "p-in", "foo.go", "source at fork", 1); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "src", "fork", ForkCut{})
	mustPointerFork(t, s, "src", "other", ForkCut{})

	got, found, err := s.GetThreadItemByPayloadID("fork", "p-in")
	if err != nil || !found || got.ID != "edit" || got.InputPayloadID != "p-in" {
		t.Fatalf("fork payload lookup = %+v found=%v err=%v", got, found, err)
	}
	assertFork := func(stage string) {
		t.Helper()
		if data, err := s.GetPayloadData("fork", "p-in"); err != nil || string(data) != `{"old_string":"a","new_string":"b"}` {
			t.Fatalf("%s: fork input payload = %q err=%v", stage, data, err)
		}
		if data, err := s.GetPayloadData("fork", "p-out"); err != nil || string(data) != "result" {
			t.Fatalf("%s: fork output payload = %q err=%v", stage, data, err)
		}
		if snapshot, found, err := s.GetEditFileSnapshot("fork", "p-in", "foo.go"); err != nil || !found || snapshot != "source at fork" {
			t.Fatalf("%s: fork snapshot = %q found=%v err=%v", stage, snapshot, found, err)
		}
	}
	assertFork("inherited")
	if n := ownRowCount(t, s, "fork"); n != 0 {
		t.Fatalf("reading payloads copied rows: fork stores %d", n)
	}

	// A fork's snapshot write fills the cache on the source's payload row.
	if err := s.PutEditFileSnapshot("other", "p-in", "bar.go", "bar", 2); err != nil {
		t.Fatal(err)
	}
	if n := ownRowCount(t, s, "other"); n != 0 {
		t.Fatalf("snapshot cache write copied rows: other stores %d", n)
	}
	if snapshot, found, err := s.GetEditFileSnapshot("src", "p-in", "bar.go"); err != nil || !found || snapshot != "bar" {
		t.Fatalf("snapshot not written to the holder: %q found=%v err=%v", snapshot, found, err)
	}

	if err := s.ReplacePayloadData("src", "p-in", []byte("source changed"), "{}", 3); err == nil || !strings.Contains(err.Error(), shownHistoryImmutable) {
		t.Fatalf("source rewrite of a payload its forks show = %v, want refused", err)
	}
	assertFork("after the refused source rewrite")
	if data, err := s.GetPayloadData("src", "p-in"); err != nil || string(data) != `{"old_string":"a","new_string":"b"}` {
		t.Fatalf("source payload = %q err=%v", data, err)
	}
}

// TestBuildForkedThreadCopiesEverythingButSessionState pins the fork-row
// builder contract.
func TestBuildForkedThreadCopiesEverythingButSessionState(t *testing.T) {
	source := Thread{
		ID:                         "source-id",
		ProjectID:                  "p-1",
		Title:                      "Build feature",
		Provider:                   "claude",
		WorkspacePath:              "/tmp/workspace",
		Model:                      "claude-opus-4-7",
		WorktreePath:               "/tmp/wt",
		Branch:                     "feature/login",
		Mode:                       "plan",
		ReasoningEffort:            "xhigh",
		FastMode:                   true,
		ContextWindow:              1000000,
		AutoCompactStandardPercent: 80,
		AutoCompactExtendedPercent: 70,
		RuntimeMode:                "full-access",
		SessionRef:                 "live-session",
		PendingForkRef:             "pending-fork",
		LastTokenUsage:             `{"usedTokens":98765,"maxTokens":1000000,"contextPercent":9.876}`,
		ForkedFromThreadID:         "previous-fork",
		CreatedAt:                  1,
		UpdatedAt:                  2,
	}

	fork := BuildForkedThread(source)

	if fork.ID == "" || fork.ID == source.ID {
		t.Errorf("fork.ID = %q, want fresh non-empty value", fork.ID)
	}
	if fork.Title != "Build feature (fork)" {
		t.Errorf("fork.Title = %q, want %q", fork.Title, "Build feature (fork)")
	}
	if fork.ForkedFromThreadID != source.ID {
		t.Errorf("ForkedFromThreadID = %q, want %q", fork.ForkedFromThreadID, source.ID)
	}
	if fork.ProjectID != source.ProjectID {
		t.Errorf("ProjectID = %q, want %q", fork.ProjectID, source.ProjectID)
	}
	if fork.Provider != source.Provider || fork.Model != source.Model {
		t.Errorf("provider/model not copied: %+v", fork)
	}
	if fork.WorkspacePath != source.WorkspacePath || fork.WorktreePath != source.WorktreePath || fork.Branch != source.Branch {
		t.Errorf("workspace fields not copied: %+v", fork)
	}
	if fork.Mode != source.Mode || fork.RuntimeMode != source.RuntimeMode {
		t.Errorf("mode fields not copied: %+v", fork)
	}
	if fork.ReasoningEffort != source.ReasoningEffort || fork.FastMode != source.FastMode || fork.ContextWindow != source.ContextWindow {
		t.Errorf("model-config fields not copied: %+v", fork)
	}
	if fork.SessionRef != "" || fork.PendingForkRef != "" {
		t.Errorf("session state leaked: %q %q", fork.SessionRef, fork.PendingForkRef)
	}
	if fork.AutoCompactStandardPercent != 0 || fork.AutoCompactExtendedPercent != 0 {
		t.Errorf("AutoCompact percents leaked: std=%d ext=%d, want 0 0",
			fork.AutoCompactStandardPercent, fork.AutoCompactExtendedPercent)
	}
	if fork.LastTokenUsage != source.LastTokenUsage {
		t.Errorf("LastTokenUsage = %q, want copied source value %q", fork.LastTokenUsage, source.LastTokenUsage)
	}
	if fork.CreatedAt == 0 || fork.CreatedAt != fork.UpdatedAt {
		t.Errorf("CreatedAt/UpdatedAt = (%d, %d), want non-zero and equal", fork.CreatedAt, fork.UpdatedAt)
	}
}

// TestPointerForkTurnRows pins the turn half: the fork owns the row of the
// turn its cut falls in and every later one, as `<fork>:<turn_index>` with
// the provider turn id kept, and reads earlier turns from the source.
func TestPointerForkTurnRows(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "src")
	for i, wireID := range []string{"wire-turn-0", "wire-turn-1", "wire-turn-2"} {
		if err := s.InsertTurn(Turn{TurnID: wireID, ProviderTurnID: wireID, ThreadID: "src", TurnIndex: i, StartedAt: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateTurnCompleted(wireID, int64(i+2), "end_turn", "", "", ""); err != nil {
			t.Fatal(err)
		}
		if err := insertCarded(s, Item{ID: fmt.Sprintf("u%d", i), ThreadID: "src", TurnIndex: i, Kind: "user_text", Role: "user", Status: "completed", CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}

	mustPointerFork(t, s, "src", "sliced", throughTurn(1))
	for i, want := range []struct{ id, wire string }{{"wire-turn-0", "wire-turn-0"}, {"sliced:1", "wire-turn-1"}} {
		turn, found, err := s.GetTurnByThreadIndex("sliced", i)
		if err != nil || !found {
			t.Fatalf("fork turn %d: found=%v err=%v", i, found, err)
		}
		if turn.TurnID != want.id || turn.ProviderTurnID != want.wire || turn.ThreadID != "sliced" {
			t.Errorf("fork turn %d = %s/%s thread %s, want %s/%s", i, turn.TurnID, turn.ProviderTurnID, turn.ThreadID, want.id, want.wire)
		}
	}
	if _, found, err := s.GetTurnByThreadIndex("sliced", 2); err != nil || found {
		t.Fatalf("fork shows turn 2 beyond its cut: found=%v err=%v", found, err)
	}
	assertTurnsRemaining(t, s, "sliced", []int{1})

	mustPointerFork(t, s, "src", "full", ForkCut{})
	last, found, err := s.GetTurnByThreadIndex("full", 2)
	if err != nil || !found || last.TurnID != "full:2" || last.ProviderTurnID != "wire-turn-2" {
		t.Fatalf("full fork turn 2 = %+v found=%v err=%v", last, found, err)
	}
	if src, found, err := s.GetTurnByThreadIndex("src", 1); err != nil || !found || src.TurnID != "wire-turn-1" {
		t.Fatalf("source turn 1 = %+v found=%v err=%v", src, found, err)
	}
}

// forkHistoryFixture seeds a source thread with three settled turns whose
// middle turn mixes a prompt, a partial reply, two queued flush user rows,
// and the interrupted round's persisted tail.
func forkHistoryFixture(t *testing.T, s *Store, src string, promotedAnchors bool) {
	t.Helper()
	now := int64(1_000)
	mustCreateThread(t, s, src)
	anchorMeta := ""
	if promotedAnchors {
		var err error
		if anchorMeta, err = itemmeta.MarkPromotedAtInterrupt(""); err != nil {
			t.Fatalf("mark promoted: %v", err)
		}
	}
	rows := []Item{
		{ID: "u0", TurnIndex: 0, ItemIndex: 0, Kind: "user_text", Role: "user", CreatedAt: now},
		{ID: "a0", TurnIndex: 0, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", CreatedAt: now + 1},
		{ID: "prompt", TurnIndex: 1, ItemIndex: 0, Kind: "user_text", Role: "user", CreatedAt: now + 10},
		{ID: "pre", TurnIndex: 1, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", CreatedAt: now + 11},
		{ID: "anchor", TurnIndex: 1, ItemIndex: 2, Kind: "user_text", Role: "user", Meta: anchorMeta, CreatedAt: now + 12},
		{ID: "queued2", TurnIndex: 1, ItemIndex: 3, Kind: "user_text", Role: "user", Meta: anchorMeta, CreatedAt: now + 13},
		{ID: "tail", TurnIndex: 1, ItemIndex: 4, Kind: "assistant_text", Role: "assistant", CreatedAt: now + 14},
		{ID: "u2", TurnIndex: 2, ItemIndex: 0, Kind: "user_text", Role: "user", CreatedAt: now + 20},
		{ID: "a2", TurnIndex: 2, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", CreatedAt: now + 21},
	}
	for _, it := range rows {
		it.ThreadID = src
		if err := insertCarded(s, it); err != nil {
			t.Fatalf("insert %s: %v", it.ID, err)
		}
	}
	for turn := 0; turn <= 2; turn++ {
		if err := s.InsertTurn(Turn{
			TurnID: fmt.Sprintf("%s:%d", src, turn), ThreadID: src,
			TurnIndex: turn, StartedAt: now + int64(turn*10),
		}); err != nil {
			t.Fatalf("insert turn %d: %v", turn, err)
		}
		if err := s.UpdateTurnCompleted(
			fmt.Sprintf("%s:%d", src, turn), now+int64(turn*10)+100,
			"end_turn", fmt.Sprintf("am-%d", turn), `{"in":5}`, "",
		); err != nil {
			t.Fatalf("settle turn %d: %v", turn, err)
		}
	}
}

// TestPointerForkBeforePlainAnchorKeepsPrefix: a plain anchor keeps earlier
// turns and the anchor turn's strict prefix, and trims the fork's copy of
// the anchor turn's settle metadata (completed_at back to the last kept
// row, assistant_message_id cleared, usage and stop reason kept). The
// source is untouched.
func TestPointerForkBeforePlainAnchorKeepsPrefix(t *testing.T) {
	s := newTestStore(t)
	forkHistoryFixture(t, s, "src", false)
	mustPointerFork(t, s, "src", "fork", ForkCut{BeforeItemID: "anchor"})

	if got, want := rowPositions(forkRows(t, s, "fork")), []string{"0:0:user", "0:1:assistant", "1:0:user", "1:1:assistant"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("fork rows = %v, want %v", got, want)
	}
	requireIDs(t, "fork lineage", forkLineage(t, s, "fork"), []string{"1:src:1:2"})
	turn1, ok, err := s.GetTurnByThreadIndex("fork", 1)
	if err != nil || !ok {
		t.Fatalf("fork turn 1: ok=%v err=%v", ok, err)
	}
	if turn1.CompletedAt == nil || *turn1.CompletedAt != 1_011 || turn1.AssistantMessageID != "" {
		t.Errorf("fork turn 1 = completed %v am %q, want 1011 and cleared", turn1.CompletedAt, turn1.AssistantMessageID)
	}
	if turn1.TokenUsageJSON != `{"in":5}` || turn1.StopReason != "end_turn" {
		t.Errorf("fork turn 1 usage/stop rewritten: %+v", turn1)
	}
	turn0, ok, err := s.GetTurnByThreadIndex("fork", 0)
	if err != nil || !ok || turn0.CompletedAt == nil || *turn0.CompletedAt != 1_100 || turn0.AssistantMessageID != "am-0" {
		t.Errorf("fork turn 0 should read the source's verbatim: %+v ok=%v err=%v", turn0, ok, err)
	}
	if _, ok, err := s.GetTurnByThreadIndex("fork", 2); err != nil || ok {
		t.Errorf("fork shows turn 2: ok=%v err=%v", ok, err)
	}
	srcTurn1, ok, err := s.GetTurnByThreadIndex("src", 1)
	if err != nil || !ok || srcTurn1.CompletedAt == nil || *srcTurn1.CompletedAt != 1_110 || srcTurn1.AssistantMessageID != "am-1" {
		t.Errorf("source turn 1 must stay untouched: %+v", srcTurn1)
	}
	if rows, err := s.ListItems("src"); err != nil || len(rows) != 9 {
		t.Errorf("source items = %d err=%v, want 9", len(rows), err)
	}
}

// TestPointerForkBeforePromotedAnchorKeepsTail: an interrupt-promoted anchor
// keeps its turn's content successors, drops same-turn user successors, and
// leaves the turn's settle metadata alone.
func TestPointerForkBeforePromotedAnchorKeepsTail(t *testing.T) {
	s := newTestStore(t)
	forkHistoryFixture(t, s, "src", true)
	mustPointerFork(t, s, "src", "fork", ForkCut{BeforeItemID: "anchor"})

	if got, want := rowPositions(forkRows(t, s, "fork")), []string{"0:0:user", "0:1:assistant", "1:0:user", "1:1:assistant", "1:4:assistant"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("fork rows = %v, want %v", got, want)
	}
	turn1, ok, err := s.GetTurnByThreadIndex("fork", 1)
	if err != nil || !ok || turn1.CompletedAt == nil || *turn1.CompletedAt != 1_110 || turn1.AssistantMessageID != "am-1" {
		t.Errorf("promoted fork must keep turn 1 settle metadata: %+v ok=%v err=%v", turn1, ok, err)
	}
}

// TestPointerForkBeforePromotedAnchorHidesDescendantsOfExcludedRows: the
// promoted cut hides a queued top-level user row among kept content, and
// with it everything parented to that row.
func TestPointerForkBeforePromotedAnchorHidesDescendantsOfExcludedRows(t *testing.T) {
	s := newTestStore(t)
	forkHistoryFixture(t, s, "src", true)
	if err := insertCarded(s, Item{
		ID: "queued2-child", ThreadID: "src", TurnIndex: 1, ItemIndex: 5,
		Kind: "assistant_text", Role: "assistant", ParentID: "queued2", CreatedAt: 1_015,
	}); err != nil {
		t.Fatal(err)
	}
	mustPointerFork(t, s, "src", "fork", ForkCut{BeforeItemID: "anchor"})
	for _, it := range forkRows(t, s, "fork") {
		switch it.ID {
		case "anchor", "queued2", "queued2-child":
			t.Errorf("row %s shown, want it hidden", it.ID)
		}
	}
	assertForkLinksResolve(t, s, "fork")
}

// TestPointerForkBeforePromotedBoundaryCutsResponse: a promoted anchor with
// an echo boundary keeps the interrupted tail up to the boundary, excludes
// the response past it, and trims the turn's settle metadata.
func TestPointerForkBeforePromotedBoundaryCutsResponse(t *testing.T) {
	s := newTestStore(t)
	forkHistoryFixture(t, s, "src", true)
	if _, _, err := s.UpdateItemMetaMerge("src", "anchor", func(raw string) (string, error) {
		return itemmeta.MarkPromotedEchoBoundary(raw, 5)
	}, 2_000); err != nil {
		t.Fatal(err)
	}
	if err := insertCarded(s, Item{
		ID: "subprompt", ThreadID: "src", TurnIndex: 1, ItemIndex: 5,
		Kind: "user_text", Role: "user", ParentID: "pre", CreatedAt: 1_015,
	}); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"resp1", "resp2"} {
		if err := insertCarded(s, Item{
			ID: id, ThreadID: "src", TurnIndex: 1, ItemIndex: 6 + i,
			Kind: "assistant_text", Role: "assistant", CreatedAt: 1_016 + int64(i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	mustPointerFork(t, s, "src", "fork", ForkCut{BeforeItemID: "anchor"})

	if got, want := rowPositions(forkRows(t, s, "fork")), []string{"0:0:user", "0:1:assistant", "1:0:user", "1:1:assistant", "1:4:assistant", "1:5:user"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("fork rows = %v, want %v", got, want)
	}
	turn1, ok, err := s.GetTurnByThreadIndex("fork", 1)
	if err != nil || !ok || turn1.CompletedAt == nil || *turn1.CompletedAt != 1_015 || turn1.AssistantMessageID != "" {
		t.Errorf("fork turn 1 = %+v ok=%v err=%v, want completed 1015 and cleared", turn1, ok, err)
	}
}

// TestPointerForkBeforeTurnOpeningAnchor: an anchor that opens turn 0 keeps
// nothing; a missing anchor errors and creates no thread.
func TestPointerForkBeforeTurnOpeningAnchor(t *testing.T) {
	s := newTestStore(t)
	forkHistoryFixture(t, s, "src", false)
	mustPointerFork(t, s, "src", "empty", ForkCut{BeforeItemID: "u0"})
	if rows, err := s.ListItems("empty"); err != nil || len(rows) != 0 {
		t.Fatalf("turn-0 anchor fork = %d rows err=%v, want none", len(rows), err)
	}
	assertTurnsRemaining(t, s, "empty", nil)

	fork := makeThread("missing", "claude")
	if err := s.CreatePointerFork(fork, "src", ForkCut{BeforeItemID: "no-such-item"}, testInterruptedSummary, 1); err == nil {
		t.Fatal("expected error for missing anchor")
	}
	if _, err := s.GetThread("missing"); err == nil {
		t.Fatal("a refused fork left its thread row behind")
	}
}

// TestPointerForkSettlesRunningTurnAsInterrupted: rows and the turn still
// running in the source are settled in the fork's copies with the standard
// interrupted treatment; the source's stay running.
func TestPointerForkSettlesRunningTurnAsInterrupted(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForTurn(t, s, "src")
	seedItemWithStatus(t, s, "src", "done", 0, 0, "assistant_text", "completed", "reply 0", false)
	if err := s.InsertTurn(makeInflightTurn("src:0", "src", 0, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTurnCompleted("src:0", 100, "end_turn", "m0", "", ""); err != nil {
		t.Fatal(err)
	}
	seedItemWithStatus(t, s, "src", "old-running", 0, 1, "tool_call", "running", "Grep: foo", false)
	seedItemWithStatus(t, s, "src", "stream", 1, 0, "assistant_text", "streaming", "partial", false)
	seedItemWithStatus(t, s, "src", "fg-tool", 1, 1, "tool_call", "running", "Read: /tmp/x", false)
	seedItemWithStatus(t, s, "src", "bg-tool", 1, 2, "tool_call", "running", "Bash: sleep 600", true)
	seedItemWithStatus(t, s, "src", "bg-done", 1, 3, "tool_call", "completed", "Bash: echo hi", true)
	seedItemWithStatus(t, s, "src", "blank", 1, 4, "assistant_text", "streaming", "", false)
	if err := s.InsertTurn(makeInflightTurn("src:1", "src", 1, 200)); err != nil {
		t.Fatal(err)
	}

	mustPointerFork(t, s, "src", "fork", ForkCut{})

	byID := map[string]Item{}
	for _, it := range forkRows(t, s, "fork") {
		byID[it.ID] = it
	}
	for _, tc := range []struct{ id, status, summary string }{
		{"stream", "errored", "partial — interrupted"},
		{"fg-tool", "errored", "Read: /tmp/x — interrupted"},
		{"old-running", "errored", "Grep: foo — interrupted"},
		{"blank", "errored", "Interrupted"},
		{"bg-done", "completed", "Bash: echo hi"},
		{"done", "completed", "reply 0"},
	} {
		if got := byID[tc.id]; got.Status != tc.status || got.Summary != tc.summary {
			t.Errorf("fork item %s = %q/%q, want %q/%q", tc.id, got.Status, got.Summary, tc.status, tc.summary)
		}
	}
	if _, ok := byID["bg-tool"]; ok {
		t.Error("fork shows the source's live background launch")
	}
	settled, found, err := s.GetTurnByThreadIndex("fork", 1)
	if err != nil || !found || settled.CompletedAt == nil || *settled.CompletedAt != 999 || settled.StopReason != "interrupted" {
		t.Errorf("fork turn 1 = %+v found=%v err=%v, want 999/interrupted", settled, found, err)
	}
	if kept, found, err := s.GetTurnByThreadIndex("fork", 0); err != nil || !found || kept.StopReason != "end_turn" {
		t.Errorf("fork turn 0 = %+v, want the source's settled row", kept)
	}
	if open, found, err := s.GetTurnByThreadIndex("src", 1); err != nil || !found || open.CompletedAt != nil {
		t.Errorf("source turn 1 = %+v, want still open", open)
	}
	for _, id := range []string{"stream", "fg-tool", "old-running", "blank"} {
		if it, _, err := s.GetThreadItem("src", id); err != nil || it.Status == "errored" {
			t.Errorf("source %s settled by the fork: %+v err=%v", id, it, err)
		}
	}

	// An idle source's fork stores nothing but its turn row.
	mustCreateThreadForTurn(t, s, "idle")
	seedItemWithStatus(t, s, "idle", "a0", 0, 0, "assistant_text", "completed", "reply", false)
	mustPointerFork(t, s, "idle", "idle-fork", ForkCut{})
	if n := ownRowCount(t, s, "idle-fork"); n != 0 {
		t.Errorf("idle fork stores %d rows, want none", n)
	}
}

// TestCreatePointerForkRefusesMalformedRequests pins the argument checks.
func TestCreatePointerForkRefusesMalformedRequests(t *testing.T) {
	s := newTestStore(t)
	seedForkSource(t, s, "src", []Item{{ID: "u", Kind: "user_text", Role: "user", Status: "completed"}})
	turn := 0
	for name, call := range map[string]func() error{
		"nil summarise": func() error {
			return s.CreatePointerFork(makeThread("a", "claude"), "src", ForkCut{}, nil, 1)
		},
		"both cuts": func() error {
			return s.CreatePointerFork(makeThread("b", "claude"), "src", ForkCut{ThroughTurn: &turn, BeforeItemID: "u"}, testInterruptedSummary, 1)
		},
		"missing source": func() error {
			return s.CreatePointerFork(makeThread("c", "claude"), "nope", ForkCut{}, testInterruptedSummary, 1)
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: fork succeeded", name)
		}
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, err := s.GetThread(id); err == nil {
			t.Errorf("refused fork %s left a thread row", id)
		}
	}
	if err := s.CreatePointerFork(makeThread("d", "claude"), "src", ForkCut{}, testInterruptedSummary, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetThread("d"); err != nil {
		t.Fatalf("fork row: %v", err)
	}
}
