package store

import (
	"reflect"
	"testing"
)

func TestWindowCompletionKeepsLaunchContextOutsideMembership(t *testing.T) {
	s := newTestStore(t)
	for _, threadID := range []string{"t", "other"} {
		if err := s.CreateThread(makeThread(threadID, "claude")); err != nil {
			t.Fatal(err)
		}
		launch := Item{ID: "agent", ThreadID: threadID, TurnIndex: 0, ItemIndex: 0, Kind: "tool_call", Role: "assistant", ToolName: "Agent", Status: "running", Summary: threadID + " agent", IsBackground: true, Meta: `{"input":{"subagent_type":"general-purpose"}}`}
		if err := s.InsertItem(launch); err != nil {
			t.Fatal(err)
		}
	}
	seedItem(t, s, "t", "prose", 0, 1, "")
	seedCompletionSibling(t, s, "t", "complete:agent", "agent", 2, 10)
	page, err := s.ListThreadSliceAround("t", "", 1, 10, TimelineSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if got := collectIDs(page.Items); !reflect.DeepEqual(got, []string{"complete:agent"}) {
		t.Fatalf("page rows: %v", got)
	}
	completion := page.Items[0]
	if completion.CompletionLaunch == nil || completion.CompletionLaunch.ID != "agent" || completion.CompletionLaunch.ThreadID != "t" || completion.CompletionLaunch.Summary != "t agent" || !completion.CompletionLaunch.IsBackground {
		t.Fatalf("launch context: %+v", completion.CompletionLaunch)
	}
	if len(page.Runs) != 1 || page.Runs[0].MemberCount != 1 {
		t.Fatalf("context changed run membership: %+v", page.Runs)
	}
	members, err := s.ListActivityRunMembers("t", ActivityRunMembersRequest{RunFirstItemID: "complete:agent", Direction: ActivityRunMembersBefore, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(members.Items[0].CompletionLaunch, completion.CompletionLaunch) {
		t.Fatal("members lost launch context")
	}
	wire, err := s.ListWireItems("t", []string{"complete:agent"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wire[0].CompletionLaunch, completion.CompletionLaunch) {
		t.Fatal("wire lost launch context")
	}
	if !ItemReadIsDecorated(completion) {
		t.Fatal("completion can bypass decorated wire read")
	}
}
