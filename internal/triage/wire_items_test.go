package triage

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/itemwire"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

// TestSubagentTurnLeavesEveryPushedRowProvable is the whole-turn form of
// the emit-side contract (docs/architecture/thread-replica-sync.md §3.1):
// after a turn whose agent wrote children under its launch, and whose
// agent launched a nested agent that wrote below that, the LAST push of
// every top-level row (a client folds patches into the row it holds)
// must be the row a page reads, at its stored revision, so a client that
// builds its held window from those pushes gets `fresh` without a page.
// The launch and its completion sibling are the rows at stake: their
// descendants stamp them without writing them, and their page read
// carries the transitive aggregate.
func TestSubagentTurnLeavesEveryPushedRowProvable(t *testing.T) {
	st := storetest.Clone(t)
	last := &lastUpsertLog{}
	router := NewRouter(st, last.emit)
	t.Cleanup(router.flushAllUsage)
	t.Cleanup(router.DrainWireItemRefresh)
	createTestThread(t, st, "t1")

	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	startAgentLaunch(t, router, "t1", "agent-1", "", "task-1")
	deliverSubagentBlock(t, router, "t1", "agent-1", "child-text-1", "text", "the agent says hello")
	startAgentLaunch(t, router, "t1", "agent-2", "agent-1", "task-2")
	deliverSubagentBlock(t, router, "t1", "agent-2", "grandchild-text-1", "text", "the nested agent says hello")
	deliverSubagentBlock(t, router, "t1", "agent-1", "child-text-2", "text", "and goodbye")
	stashAgentTerminal(t, router, "t1", "agent-1", "task-1")
	notifyAgent(t, router, "t1", "agent-1", "task-1", "", nil)
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
	router.WaitForPendingSettles()
	router.DrainWireItemRefresh()

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var topLevel []store.Item
	for _, item := range items {
		if item.ParentID == "" {
			topLevel = append(topLevel, item)
		}
	}
	if len(topLevel) < 2 {
		t.Fatalf("turn left %d top-level rows, want the launch and its completion sibling at least: %+v", len(topLevel), topLevel)
	}
	ids := make([]string, 0, len(topLevel))
	for _, item := range topLevel {
		ids = append(ids, item.ID)
	}
	pageRows, err := st.ListWireItems("t1", ids)
	if err != nil {
		t.Fatalf("page read: %v", err)
	}
	pageByID := make(map[string]store.Item, len(pageRows))
	for _, row := range pageRows {
		pageByID[row.ID] = row
	}

	held := make([]store.WindowDigestRow, 0, len(topLevel))
	sawDecoratedAnchor := false
	sawDecoratedSibling := false
	for _, item := range topLevel {
		pushed, ok := last.get(item.ID)
		if !ok {
			t.Fatalf("top-level row %s (%s) was never pushed", item.ID, item.Kind)
		}
		page := pageByID[item.ID]
		if want := itemwire.Project(page, true); !reflect.DeepEqual(pushed, want) {
			t.Errorf("last push of %s (%s) is not its page read:\n got %+v\nwant %+v", item.ID, item.Kind, pushed, want)
		}
		if pushed.Rev != page.Rev {
			t.Errorf("last push of %s (%s) claims rev %d, stored %d", item.ID, item.Kind, pushed.Rev, page.Rev)
		}
		if item.ID == "agent-1" && strings.Contains(pushed.Meta, `"subagentDescendantCount":4`) {
			sawDecoratedAnchor = true
		}
		// The card of a detached launch sits at the sibling and reads its
		// count there, so the sibling's push must carry the same aggregate.
		if item.ID == ToolCompletionID("agent-1") && strings.Contains(pushed.Meta, `"subagentDescendantCount":4`) {
			sawDecoratedSibling = true
		}
		held = append(held, store.WindowDigestRow{ID: item.ID, Rev: pushed.Rev})
	}
	if !sawDecoratedAnchor {
		t.Fatal("the launch's last push does not carry its four transitive descendants; the anchor was never refreshed from a page read")
	}
	if !sawDecoratedSibling {
		t.Fatal("the completion sibling's last push does not carry the launch's four descendants; the card that sits at it would count zero")
	}

	stamp, _, err := st.ThreadHistoryStamp("t1")
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	stamp.Rev--
	window := store.HeldWindow{
		OldestItemID: topLevel[0].ID,
		NewestItemID: topLevel[len(topLevel)-1].ID,
		Count:        len(topLevel),
		Digest:       store.WindowDigest(held),
	}
	sync, err := st.SyncThreadWindow(context.Background(), "t1", "", 200, 200, stamp, &window)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if sync.Status != store.SyncFresh {
		t.Fatalf("window built from the pushed rows verified as %q, want fresh", sync.Status)
	}
}

// lastUpsertLog holds what a client that applied every push in order
// would hold: the last upsert of each row with every later patch folded
// into it, revision included, the way the frontend folds a patch.
type lastUpsertLog struct {
	mu   sync.Mutex
	rows map[string]store.Item
}

func (l *lastUpsertLog) emit(channel eventchan.Channel, data any) {
	if channel.String() != "provider:item_event" {
		return
	}
	evt, ok := data.(ItemStreamEvent)
	if !ok {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rows == nil {
		l.rows = make(map[string]store.Item)
	}
	switch evt.Action {
	case itemStreamActionUpsert:
		if evt.Item != nil {
			l.rows[evt.Item.ID] = *evt.Item
		}
	case itemStreamActionPatch:
		row, held := l.rows[evt.ItemID]
		if !held || evt.Patch == nil {
			return
		}
		if evt.Patch.Status != nil {
			row.Status = *evt.Patch.Status
		}
		if evt.Patch.Summary != nil {
			row.Summary = *evt.Patch.Summary
		}
		if evt.Patch.Meta != nil {
			row.Meta = *evt.Patch.Meta
		}
		if evt.Patch.Decision != nil {
			row.Decision = *evt.Patch.Decision
		}
		if evt.Patch.UpdatedAt != nil {
			row.UpdatedAt = *evt.Patch.UpdatedAt
		}
		row.Rev = evt.Patch.Rev
		l.rows[evt.ItemID] = row
	}
}

func (l *lastUpsertLog) get(id string) (store.Item, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, ok := l.rows[id]
	return row, ok
}

// TestWireItemRefreshAccountingUnderConcurrency drives notes, timer
// flushes, synchronous flushes and thread teardown against each other
// under the race detector. The assertion is that the drains return: a
// leaked or doubled refreshWG slot would hang or panic here.
func TestWireItemRefreshAccountingUnderConcurrency(t *testing.T) {
	st := storetest.Clone(t)
	router := NewRouter(st, func(eventchan.Channel, any) {})
	t.Cleanup(router.flushAllUsage)
	createTestThread(t, st, "t1")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	launch := store.Item{
		ID: "agent-1", ThreadID: "t1", Kind: itemKindToolCall, Role: "assistant",
		Status: statusRunning, ToolName: "Agent", Summary: "Agent: work", CreatedAt: 1, UpdatedAt: 1,
	}
	if err := router.persistItem(launch, nil); err != nil {
		t.Fatalf("launch: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for n := 0; n < 50; n++ {
				switch (i + n) % 4 {
				case 0:
					router.noteWireItemEmitted("t1", "agent-1", int64(n))
				case 1:
					router.flushWireItemRefresh("t1")
				case 2:
					router.noteWireItemEmitted("t1", "agent-1", store.UnstampedItemRev)
				case 3:
					router.CleanupThread("t1")
					router.MarkThreadActive("t1")
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()

	done := make(chan struct{})
	go func() {
		router.DrainWireItemRefresh()
		if err := router.Wait(context.Background()); err != nil {
			t.Errorf("wait: %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh drain did not return: a refreshWG slot leaked")
	}
}
