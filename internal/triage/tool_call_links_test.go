package triage

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// itemUpserts returns the item upserts in an emission window, in order.
func itemUpserts(events []emitted) []store.Item {
	var rows []store.Item
	for _, e := range events {
		evt, ok := e.data.(ItemStreamEvent)
		if !ok || evt.Action != itemStreamActionUpsert || evt.Item == nil {
			continue
		}
		rows = append(rows, *evt.Item)
	}
	return rows
}

// seedAgentChain writes two nested agent launches straight to the store,
// as a session resumed after they were persisted finds them: agent-1 at
// the top level of turn 0 and agent-2 inside it.
func seedAgentChain(t *testing.T, st *store.Store, threadID string) {
	t.Helper()
	for _, row := range []store.Item{
		{ID: "agent-1", ThreadID: threadID, TurnIndex: 0, Kind: itemKindToolCall, Role: "assistant",
			Status: statusRunning, ToolName: "Agent", Summary: "Agent: outer", IsBackground: true, CreatedAt: 1, UpdatedAt: 1},
		{ID: "agent-2", ThreadID: threadID, TurnIndex: 0, Kind: itemKindToolCall, Role: "assistant",
			Status: statusRunning, ToolName: "Agent", Summary: "Agent: inner", ParentID: "agent-1", IsBackground: true, CreatedAt: 1, UpdatedAt: 1},
	} {
		if _, err := st.AppendItem(row); err != nil {
			t.Fatalf("seed %s: %v", row.ID, err)
		}
	}
}

// TestSubagentToolEventReadsItsRowOnce pins the store reads one live
// subagent tool call costs, with and without an open turn. Each event
// reads its own row once (the launch lookup, the completion's launch);
// every other read is a fixed, cheap one named below. The parent chain
// and the scope's turn come from the tool-call links after the first
// event pays one read per ancestor the session had not seen, and no
// event refreshes an anchor synchronously: each pushes exactly its own
// row, and the anchors follow at the quiet point.
func TestSubagentToolEventReadsItsRowOnce(t *testing.T) {
	for _, openTurn := range []bool{false, true} {
		t.Run(fmt.Sprintf("openTurn=%v", openTurn), func(t *testing.T) {
			router, st, emissions := newTestRouter(t)
			createTestThread(t, st, "t1")
			seedAgentChain(t, st, "t1")
			if openTurn {
				if err := router.Handle(provider.ProviderEvent{
					Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 1, Timestamp: time.Now(),
				}); err != nil {
					t.Fatalf("turn start: %v", err)
				}
			}
			startMeta, _ := json.Marshal(map[string]any{"toolName": "Bash", "input": map[string]any{"command": "echo hi"}})
			doneMeta, _ := json.Marshal(map[string]any{"exit_code": 0})
			for i := 0; i < 5; i++ {
				id := fmt.Sprintf("child-%d", i)
				emissions.reset()
				before := st.ReadCount()
				if err := router.Handle(provider.ProviderEvent{
					Kind: provider.EventToolStart, ThreadID: "t1", ItemID: id, ItemType: "Bash",
					Meta: startMeta, ParentToolUseID: "agent-2", Timestamp: time.Now(),
				}); err != nil {
					t.Fatalf("start %s: %v", id, err)
				}
				// The launch lookup and the decoration probe; the first
				// event also reads agent-2 (its turn) and agent-1 (the
				// parent chain) once each.
				wantStart := uint64(2)
				if i == 0 {
					wantStart += 2
				}
				if got := st.ReadCount() - before; got != wantStart {
					t.Errorf("%s start: %d store reads, want %d", id, got, wantStart)
				}
				if pushed := itemUpserts(emissions.snapshot()); len(pushed) != 1 || pushed[0].ID != id {
					t.Errorf("%s start pushed %v, want only its own row", id, pushedIDs(pushed))
				}

				emissions.reset()
				before = st.ReadCount()
				if err := router.Handle(provider.ProviderEvent{
					Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: id, Content: "hi\n",
					Meta: doneMeta, ParentToolUseID: "agent-2", Timestamp: time.Now(),
				}); err != nil {
					t.Fatalf("complete %s: %v", id, err)
				}
				// The launch lookup, the thread's provider and the
				// decoration probe.
				if got := st.ReadCount() - before; got != 3 {
					t.Errorf("%s complete: %d store reads, want 3", id, got)
				}
				pushed := itemUpserts(emissions.snapshot())
				if len(pushed) != 1 || pushed[0].ID != id || pushed[0].Status != statusCompleted {
					t.Errorf("%s complete pushed %+v, want only its own completed row", id, pushed)
				}
			}

			row, found, err := st.GetThreadItem("t1", "child-4")
			if err != nil || !found {
				t.Fatalf("read child-4: found=%v err=%v", found, err)
			}
			if row.ParentID != "agent-2" || row.TurnIndex != 0 {
				t.Fatalf("child-4 placed at parent %q turn %d, want agent-2 in turn 0", row.ParentID, row.TurnIndex)
			}

			emissions.reset()
			router.DrainWireItemRefresh()
			refreshed := map[string]store.Item{}
			for _, row := range itemUpserts(emissions.snapshot()) {
				refreshed[row.ID] = row
			}
			for _, anchor := range []string{"agent-1", "agent-2"} {
				row, ok := refreshed[anchor]
				if !ok {
					t.Fatalf("the refresh did not push anchor %s; pushed %v", anchor, mapKeys(refreshed))
				}
				if row.Rev == store.UnstampedItemRev || !strings.Contains(row.Meta, `"subagentDescendantCount"`) {
					t.Errorf("anchor %s refreshed as rev %d meta %s, want its decorated page read", anchor, row.Rev, row.Meta)
				}
			}
		})
	}
}

func pushedIDs(rows []store.Item) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func mapKeys(rows map[string]store.Item) []string {
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	return ids
}

// TestToolCallLinksEvictUnreferencedFirst pins the bound and the clock:
// the map never holds more than maxToolCallLinksPerThread entries, and an
// entry looked up since the hand last passed survives a full rotation of
// new ones while unreferenced entries go.
func TestToolCallLinksEvictUnreferencedFirst(t *testing.T) {
	var links toolCallLinks
	for i := 0; i < maxToolCallLinksPerThread; i++ {
		links.put(fmt.Sprintf("leaf-%d", i), "root", 0)
	}
	links.put("root", "", 7)
	if len(links.byID) != maxToolCallLinksPerThread {
		t.Fatalf("holds %d links, want the bound %d", len(links.byID), maxToolCallLinksPerThread)
	}
	if _, ok := links.get("leaf-0"); ok {
		t.Fatal("the oldest unreferenced link survived the first eviction")
	}
	for i := 0; i < 3*maxToolCallLinksPerThread; i++ {
		if _, ok := links.get("root"); !ok {
			t.Fatalf("the referenced root was evicted after %d newer links", i)
		}
		links.put(fmt.Sprintf("new-%d", i), "root", 0)
		if len(links.byID) > maxToolCallLinksPerThread || len(links.ring) > maxToolCallLinksPerThread {
			t.Fatalf("grew past the bound: %d links, %d ring slots", len(links.byID), len(links.ring))
		}
	}
	link, ok := links.get("root")
	if !ok || link.turnIndex != 7 || link.parentID != "" {
		t.Fatalf("root link = %+v ok=%v, want turn 7 with no parent", link, ok)
	}
}

// TestShouldDropParentIDOverLinks keeps the parent_id guard's verdicts
// now that the chain is read through the links: a chain the session wrote
// costs no store read, and the self, cycle, non-tool_call, missing and
// depth verdicts are unchanged.
func TestShouldDropParentIDOverLinks(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	for _, row := range []store.Item{
		{ID: "a", ThreadID: "t1", Kind: itemKindToolCall, Role: "assistant", Status: statusRunning, ToolName: "Agent", CreatedAt: 1, UpdatedAt: 1},
		{ID: "b", ThreadID: "t1", Kind: itemKindToolCall, Role: "assistant", Status: statusRunning, ToolName: "Agent", ParentID: "a", CreatedAt: 1, UpdatedAt: 1},
		{ID: "text", ThreadID: "t1", Kind: itemKindAssistantText, Role: "assistant", Status: statusCompleted, Summary: "hi", CreatedAt: 1, UpdatedAt: 1},
	} {
		if err := router.persistItem(row, nil); err != nil {
			t.Fatalf("persist %s: %v", row.ID, err)
		}
	}

	before := st.ReadCount()
	if drop, reason := router.shouldDropParentID("t1", "c", "b"); drop {
		t.Fatalf("a valid chain was dropped: %s", reason)
	}
	if got := st.ReadCount() - before; got != 0 {
		t.Fatalf("walking a chain the session wrote cost %d store reads, want 0", got)
	}
	if drop, reason := router.shouldDropParentID("t1", "a", "b"); !drop || reason != "cycle detected" {
		t.Fatalf("a -> b -> a: drop=%v reason=%q, want a cycle", drop, reason)
	}
	if drop, reason := router.shouldDropParentID("t1", "c", "c"); !drop || reason != "self reference" {
		t.Fatalf("self parent: drop=%v reason=%q", drop, reason)
	}
	if drop, reason := router.shouldDropParentID("t1", "c", "text"); !drop || !strings.Contains(reason, `"assistant_text" is not tool_call`) {
		t.Fatalf("text parent: drop=%v reason=%q, want the non-tool_call drop", drop, reason)
	}
	if drop, _ := router.shouldDropParentID("t1", "c", "not-yet"); drop {
		t.Fatal("a parent that has not landed yet was dropped")
	}

	var links toolCallLinks
	for i := 0; i < 20; i++ {
		links.put(fmt.Sprintf("deep-%d", i), fmt.Sprintf("deep-%d", i+1), 0)
	}
	router.mu.Lock()
	router.state("t1").toolCalls = links
	router.mu.Unlock()
	if drop, reason := router.shouldDropParentID("t1", "c", "deep-0"); !drop || reason != "parent chain too deep" {
		t.Fatalf("20-deep chain: drop=%v reason=%q", drop, reason)
	}
}

// TestForgetToolCallLinksAfterCut: a live cut deletes a scope whose turn
// the links hold. Forgetting them sends a late event for that scope to
// the current turn, as the store would, instead of the deleted turn.
func TestForgetToolCallLinksAfterCut(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	for turn := 0; turn <= 2; turn++ {
		if _, err := st.AppendItem(store.Item{
			ID: fmt.Sprintf("user-%d", turn), ThreadID: "t1", TurnIndex: turn, Kind: itemKindUserText, Role: "user",
			Status: statusCompleted, Summary: "go", CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("seed turn %d: %v", turn, err)
		}
	}
	if err := router.persistItem(store.Item{
		ID: "agent-1", ThreadID: "t1", TurnIndex: 2, Kind: itemKindToolCall, Role: "assistant",
		Status: statusRunning, ToolName: "Agent", CreatedAt: 1, UpdatedAt: 1,
	}, nil); err != nil {
		t.Fatalf("persist launch: %v", err)
	}
	if turn, err := router.turnIndexForScope("t1", "agent-1"); err != nil || turn != 2 {
		t.Fatalf("scope turn = %d err=%v, want 2", turn, err)
	}
	if _, _, err := st.DeleteConversationFromTurn("t1", 2); err != nil {
		t.Fatalf("cut: %v", err)
	}
	router.ForgetToolCallLinks("t1")
	turn, err := router.turnIndexForScope("t1", "agent-1")
	if err != nil {
		t.Fatalf("scope turn after cut: %v", err)
	}
	if turn != 1 {
		t.Fatalf("a late event for the cut scope lands in turn %d, want the current turn 1", turn)
	}
}

// TestToolCallLinksNotMintedForStoppedThread: a write after teardown (a
// host-synthesized settle) must not give the thread state back, or the
// links would outlive the session with nothing to sweep them.
func TestToolCallLinksNotMintedForStoppedThread(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	router.MarkThreadActive("t1")
	router.CleanupThread("t1")
	if err := router.persistItem(store.Item{
		ID: "late", ThreadID: "t1", Kind: itemKindToolCall, Role: "assistant",
		Status: statusCompleted, ToolName: "Bash", CreatedAt: 1, UpdatedAt: 1,
	}, nil); err != nil {
		t.Fatalf("persist: %v", err)
	}
	router.mu.Lock()
	st1 := router.threadStateIfPresent("t1")
	router.mu.Unlock()
	if st1 != nil {
		t.Fatal("a write after teardown minted thread state for its link")
	}
}

// TestDecoratedRowPushedUnstampedThenRefreshed: a write to an anchor with
// children goes out as written, marked unstamped, with no decorated read
// on the write's path; the next refresh pushes the decorated page read at
// the stored revision. The field-patch path does the same instead of
// patching a decorated row.
func TestDecoratedRowPushedUnstampedThenRefreshed(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedAgentChain(t, st, "t1")

	launch, found, err := st.GetThreadItem("t1", "agent-1")
	if err != nil || !found {
		t.Fatalf("read agent-1: found=%v err=%v", found, err)
	}
	launch.Summary = "Agent: outer (renamed)"
	emissions.reset()
	if err := router.persistItem(launch, nil); err != nil {
		t.Fatalf("persist anchor: %v", err)
	}
	pushed := itemUpserts(emissions.snapshot())
	if len(pushed) != 1 || pushed[0].ID != "agent-1" {
		t.Fatalf("write pushed %v, want only agent-1", pushedIDs(pushed))
	}
	if pushed[0].Rev != store.UnstampedItemRev || strings.Contains(pushed[0].Meta, "subagentDescendantCount") {
		t.Fatalf("anchor pushed at rev %d meta %s, want unstamped and undecorated", pushed[0].Rev, pushed[0].Meta)
	}

	current, _, err := st.GetThreadItem("t1", "agent-1")
	if err != nil {
		t.Fatalf("reread agent-1: %v", err)
	}
	meta := mergeItemMetaJSON(current.Meta, []byte(`{"marker":1}`))
	emissions.reset()
	if err := router.persistItemFieldsAndPatch(current, store.ItemPartialUpdate{Meta: &meta}); err != nil {
		t.Fatalf("patch anchor: %v", err)
	}
	events := emissions.snapshot()
	pushed = itemUpserts(events)
	if len(events) != 1 || len(pushed) != 1 || pushed[0].Rev != store.UnstampedItemRev || !strings.Contains(pushed[0].Meta, `"marker":1`) {
		t.Fatalf("field write to a decorated row emitted %+v, want one unstamped upsert of the written row", events)
	}

	emissions.reset()
	router.DrainWireItemRefresh()
	var refreshed *store.Item
	for _, row := range itemUpserts(emissions.snapshot()) {
		if row.ID == "agent-1" {
			row := row
			refreshed = &row
		}
	}
	stored, _, err := st.GetThreadItem("t1", "agent-1")
	if err != nil {
		t.Fatalf("reread agent-1: %v", err)
	}
	if refreshed == nil || refreshed.Rev != stored.Rev || !strings.Contains(refreshed.Meta, `"subagentDescendantCount"`) {
		t.Fatalf("refresh pushed %+v, want agent-1's decorated page read at rev %d", refreshed, stored.Rev)
	}
}

// TestThreadWithoutLiveStateDefersItsRefresh: a thread with no live
// state takes the debounced refresh too. The write pushes its own row and
// nothing else, and the pending refresh is gone once it has run.
func TestThreadWithoutLiveStateDefersItsRefresh(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedAgentChain(t, st, "t1")

	emissions.reset()
	if err := router.persistItem(store.Item{
		ID: "child-text", ThreadID: "t1", Kind: itemKindAssistantText, Role: "assistant",
		Status: statusCompleted, Summary: "hello", ParentID: "agent-2", CreatedAt: 2, UpdatedAt: 2,
	}, nil); err != nil {
		t.Fatalf("persist child: %v", err)
	}
	if pushed := itemUpserts(emissions.snapshot()); len(pushed) != 1 || pushed[0].ID != "child-text" {
		t.Fatalf("write pushed %v, want only its own row", pushedIDs(pushed))
	}
	router.mu.Lock()
	pending := router.wireRefresh["t1"] != nil
	router.mu.Unlock()
	if !pending {
		t.Fatal("the write armed no refresh")
	}

	emissions.reset()
	router.DrainWireItemRefresh()
	ids := pushedIDs(itemUpserts(emissions.snapshot()))
	if !containsString(ids, "agent-1") || !containsString(ids, "agent-2") {
		t.Fatalf("the refresh pushed %v, want both anchors the write stamped", ids)
	}
	router.mu.Lock()
	left := len(router.wireRefresh)
	router.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d pending refreshes remain after the drain", left)
	}
}
