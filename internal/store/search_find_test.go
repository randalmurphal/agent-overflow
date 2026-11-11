package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// seedFindScopes writes a thread whose rows interleave the main transcript, a
// plan_update notification the timeline hides, an agent root with direct
// children, a resume carrier naming that root, and a nested agent's row.
func seedFindScopes(t *testing.T, s *Store) {
	t.Helper()
	if err := s.CreateThread(makeThread("find", "claude")); err != nil {
		t.Fatal(err)
	}
	rows := []Item{
		{ID: "main-a", TurnIndex: 0, ItemIndex: 0, Kind: "assistant_text", Summary: "needle main a"},
		{ID: "root", TurnIndex: 0, ItemIndex: 1, Kind: "tool_call", ToolName: "Task", Summary: "agent launch"},
		{ID: "child-a", TurnIndex: 0, ItemIndex: 2, Kind: "assistant_text", ParentID: "root", Summary: "needle child a"},
		{ID: "nested", TurnIndex: 0, ItemIndex: 3, Kind: "assistant_text", ParentID: "nested-root", Summary: "needle nested"},
		{ID: "plan", TurnIndex: 0, ItemIndex: 4, Kind: "notification", ToolName: "plan_update", Summary: "needle plan update"},
		{ID: "child-b", TurnIndex: 1, ItemIndex: 0, Kind: "assistant_text", ParentID: "root", Summary: "needle child b"},
		{ID: "carrier", TurnIndex: 1, ItemIndex: 1, Kind: "tool_call", ToolName: "SendMessage", Summary: "resume", Meta: `{"transcript_root_id":"root"}`},
		{ID: "main-b", TurnIndex: 1, ItemIndex: 2, Kind: "assistant_text", Summary: "needle main b"},
	}
	for _, row := range rows {
		row.ThreadID = "find"
		row.Role = "assistant"
		row.Status = "completed"
		row.CreatedAt = int64(row.TurnIndex*10 + row.ItemIndex)
		if err := insertCarded(s, row); err != nil {
			t.Fatalf("insert %s: %v", row.ID, err)
		}
	}
}

func findHitIDs(hits []ThreadMessageHit) string {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.ItemID
	}
	return fmt.Sprint(ids)
}

// The main selection answers the rows the thread pane shows: visible
// top-level rows, in document order, with the coordinates a reader's
// position orders against.
func TestSearchThreadItems_MainSelectionIsTheTopLevelTimeline(t *testing.T) {
	s := newTestStore(t)
	seedFindScopes(t, s)
	got, err := s.SearchThreadItems(context.Background(), "find", "needle", TimelineSelection{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ids := findHitIDs(got); ids != "[main-a main-b]" {
		t.Fatalf("main hits = %s", ids)
	}
	if got[1].TurnIndex != 1 || got[1].ItemIndex != 2 {
		t.Fatalf("main-b coordinates = (%d, %d), want (1, 2)", got[1].TurnIndex, got[1].ItemIndex)
	}
}

// An agent selection answers that agent's direct children only, in document
// order, whichever row of the agent names the scope.
func TestSearchThreadItems_AgentSelectionIsTheAgentTranscript(t *testing.T) {
	s := newTestStore(t)
	seedFindScopes(t, s)
	for _, root := range []string{"root", "carrier"} {
		got, err := s.SearchThreadItems(context.Background(), "find", "needle", TimelineSelection{ScopeRootID: root}, 0)
		if err != nil {
			t.Fatalf("scope %s: %v", root, err)
		}
		if ids := findHitIDs(got); ids != "[child-a child-b]" {
			t.Fatalf("scope %s hits = %s", root, ids)
		}
		if got[1].TurnIndex != 1 || got[1].ItemIndex != 0 {
			t.Fatalf("child-b coordinates = (%d, %d), want (1, 0)", got[1].TurnIndex, got[1].ItemIndex)
		}
	}
	limited, err := s.SearchThreadItems(context.Background(), "find", "needle", TimelineSelection{ScopeRootID: "root"}, 1)
	if err != nil || findHitIDs(limited) != "[child-a]" {
		t.Fatalf("limited scope hits = %s err=%v", findHitIDs(limited), err)
	}
}

func TestSearchThreadItems_GoneScopeIsAnError(t *testing.T) {
	s := newTestStore(t)
	seedFindScopes(t, s)
	if _, err := s.SearchThreadItems(context.Background(), "find", "needle", TimelineSelection{ScopeRootID: "missing"}, 0); !errors.Is(err, ErrTimelineScopeGone) {
		t.Fatalf("missing scope err = %v, want ErrTimelineScopeGone", err)
	}
	if _, err := s.SearchThreadItems(context.Background(), "find", "needle", TimelineSelection{Tools: true}, 0); err == nil {
		t.Fatal("a tools selection without a scope must be refused")
	}
}

// A find read belongs to the call that asked: a cancelled call (the
// client disconnected) stops the read rather than finishing the scan.
func TestSearchThreadItems_CancelledContextStopsTheSearch(t *testing.T) {
	s := newTestStore(t)
	seedFindScopes(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, selection := range []TimelineSelection{{}, {ScopeRootID: "root"}} {
		if _, err := s.SearchThreadItems(ctx, "find", "needle", selection, 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("selection %+v: err = %v, want context.Canceled", selection, err)
		}
	}
}

// A find read stops at its limit: each physical arm walks its ordering
// index and the merged compound takes the first hits, rather than reading
// every match of the view into a sorter.
func TestSearchThreadItems_WalksTheTimelineArms(t *testing.T) {
	s := newTestStore(t)
	seedTimelineParityThread(t, s)
	for _, selection := range []TimelineSelection{{}, {ScopeRootID: "root"}} {
		sel := threadFindSelection(timelineScope{selection: selection}, findPattern("needle"), 50)
		query, args := mustTimelineIDSelection(t, s, timelineParityThreadID, sel)
		assertLocalArmWalksAnIndex(t, s, fmt.Sprintf("thread find %+v", selection), query, args...)
	}
}
