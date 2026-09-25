package codexthread

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/triage"
)

// TestRetireBackgroundRuntimePushesThePageRead pins that a retired spawn
// row reaches clients as a page reads it: with its descendant aggregate
// and at the revision the retirement wrote, so a client can later prove
// a window holding it fresh (docs/architecture/thread-replica-sync.md
// §3.1). The read-failure branch of emitRetiredItems (rows pushed
// unstamped) has no seam to drive from a test: the retirement's own
// write would fail first.
func TestRetireBackgroundRuntimePushesThePageRead(t *testing.T) {
	st := storetest.Clone(t)
	now := time.Now().UnixMilli()
	if _, err := st.CreateProject(store.Project{
		ID: costTestProjectID, Path: "/tmp/codexthread", Name: "Codex Thread Test",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := st.CreateThread(testCostThread("t-retire")); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := st.InsertTurn(store.Turn{TurnID: "t-retire:0", ThreadID: "t-retire", TurnIndex: 0, StartedAt: now}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	spawn := store.Item{
		ID: "spawn-1", ThreadID: "t-retire", TurnIndex: 0, ItemIndex: 0, Kind: "tool_call", Role: "assistant",
		Status: "running", ToolName: "spawn_agent", Summary: "spawn", IsBackground: true, CreatedAt: now, UpdatedAt: now,
	}
	child := store.Item{
		ID: "child-1", ThreadID: "t-retire", TurnIndex: 0, ItemIndex: 1, Kind: "assistant_text", Role: "assistant",
		Status: "completed", Summary: "child", ParentID: "spawn-1", CreatedAt: now, UpdatedAt: now,
	}
	for _, row := range []store.Item{spawn, child} {
		if err := storetest.WithParentCard(st, row, st.InsertItem); err != nil {
			t.Fatalf("InsertItem %s: %v", row.ID, err)
		}
	}

	var pushed []store.Item
	svc := New(Deps{
		Context: func() context.Context { return context.Background() },
		Store:   st,
		Emit: func(channel eventchan.Channel, data any) {
			if evt, ok := data.(triage.ItemStreamEvent); ok && evt.Item != nil {
				pushed = append(pushed, *evt.Item)
			}
		},
		Session: func(string) (LiveSession, bool) { return LiveSession{}, false },
	})
	if err := svc.RetireBackgroundRuntime("t-retire"); err != nil {
		t.Fatalf("RetireBackgroundRuntime: %v", err)
	}
	if len(pushed) != 1 || pushed[0].ID != "spawn-1" {
		t.Fatalf("pushed %+v, want the retired spawn row", pushed)
	}
	if pushed[0].Status != "errored" {
		t.Fatalf("pushed status %q, want errored", pushed[0].Status)
	}
	if !strings.Contains(pushed[0].Meta, `"subagentDescendantCount":1`) {
		t.Fatalf("pushed row is the write read-back, not the page read: meta = %s", pushed[0].Meta)
	}
	page, err := st.ListWireItems("t-retire", []string{"spawn-1"})
	if err != nil || len(page) != 1 {
		t.Fatalf("page read: rows=%d err=%v", len(page), err)
	}
	if pushed[0].Rev != page[0].Rev || pushed[0].Rev <= 0 {
		t.Fatalf("pushed rev %d, page rev %d", pushed[0].Rev, page[0].Rev)
	}
}

// TestRecoverBackgroundRuntimeOnStartupPushesPastAFailedFlush pins that the
// startup sweep pushes the rows it retired when the card flush before it
// fails, and returns the failure: the retirement commits either way, and a
// client holding the rows must not keep showing them as running.
func TestRecoverBackgroundRuntimeOnStartupPushesPastAFailedFlush(t *testing.T) {
	path := storetest.ClonePath(t)
	st, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	now := time.Now().UnixMilli()
	if _, err := st.CreateProject(store.Project{
		ID: costTestProjectID, Path: "/tmp/codexthread", Name: "Codex Thread Test",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	held := testCostThread("t-held")
	held.Provider = "claude"
	for _, thread := range []store.Thread{held, testCostThread("t-codex")} {
		if err := st.CreateThread(thread); err != nil {
			t.Fatalf("CreateThread %s: %v", thread.ID, err)
		}
	}
	// A card holding a row its flush cannot write.
	launch := store.Item{ID: "L", ThreadID: "t-held", Kind: "tool_call", Role: "assistant", Status: "running",
		ToolName: "Agent", Summary: "Agent: l", CreatedAt: now, UpdatedAt: now}
	if err := st.InsertItem(launch); err != nil {
		t.Fatal(err)
	}
	card, err := st.OpenSubagentCard("t-held", "L")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertItem(store.Item{ID: "L-b1", ThreadID: "t-held", ItemIndex: 1, Kind: "tool_call", Role: "assistant",
		Status: "completed", ToolName: "Bash", Summary: "Bash: one", ParentID: "L", SubagentCard: card,
		CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertItem(store.Item{ID: "B", ThreadID: "t-codex", Kind: "tool_call", Role: "assistant", Status: "running",
		ToolName: "Bash", Summary: "Bash: b", IsBackground: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER fail_flush BEFORE INSERT ON subagent_aggregates BEGIN SELECT RAISE(ABORT, 'injected flush failure'); END`); err != nil {
		t.Fatal(err)
	}

	var pushed []store.Item
	svc := New(Deps{
		Context: func() context.Context { return context.Background() },
		Store:   st,
		Emit: func(channel eventchan.Channel, data any) {
			if evt, ok := data.(triage.ItemStreamEvent); ok && evt.Item != nil {
				pushed = append(pushed, *evt.Item)
			}
		},
		Session: func(string) (LiveSession, bool) { return LiveSession{}, false },
	})
	if err := svc.RecoverBackgroundRuntimeOnStartup(); err == nil || !strings.Contains(err.Error(), "injected flush failure") {
		t.Fatalf("RecoverBackgroundRuntimeOnStartup() = %v, want the flush failure", err)
	}
	if len(pushed) != 1 || pushed[0].ID != "B" || pushed[0].Status != "errored" {
		t.Fatalf("pushed %+v, want the retired B", pushed)
	}

	if _, err := raw.Exec(`DROP TRIGGER fail_flush`); err != nil {
		t.Fatal(err)
	}
	if err := card.Close(); err != nil {
		t.Fatalf("close the card once the flush can write: %v", err)
	}
}
