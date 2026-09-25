package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestTimelineDigestExecutionBoundaries(t *testing.T) {
	for _, imported := range []bool{false, true} {
		t.Run(fmt.Sprint(imported), func(t *testing.T) {
			s := newTestStore(t)
			const thread = "digest"
			newImportTargetThread(t, s, thread)
			rows := []Item{
				{ID: "root", Kind: "tool_call", ToolName: "Agent"},
				{ID: "prompt", Kind: "user_text", ParentID: "root"},
				{ID: "thought", Kind: "thinking", ParentID: "root"},
				{ID: "early", Kind: "assistant_text", ParentID: "root"},
				{ID: "tool", Kind: "tool_call", ToolName: "Bash", ParentID: "root"},
				{ID: "denied", Kind: "notification", ToolName: "permission_denied", ParentID: "root"},
				{ID: "retry", Kind: "notification", ToolName: "retry", ParentID: "root"},
				{ID: "answer", Kind: "assistant_text", ParentID: "root"},
				{ID: "done", Kind: "tool_completion", ToolName: "Agent", CompletionOf: "root"},
				{ID: "resume", Kind: "tool_call", ToolName: "SendMessage", Meta: `{"transcript_root_id":"root"}`},
				{ID: "prompt2", Kind: "user_text", ParentID: "root", Meta: `{"subagent_resume_prompt":true,"resume_carrier_id":"resume"}`},
				{ID: "tool2", Kind: "tool_call", ToolName: "Bash", ParentID: "root"},
				{ID: "answer2", Kind: "assistant_text", ParentID: "root"},
				{ID: "done2", Kind: "tool_completion", ToolName: "SendMessage", CompletionOf: "resume"},
			}
			batch := ImportBatch{}
			for i, row := range rows {
				row.ThreadID, row.ItemIndex, row.CreatedAt, row.Status, row.Role = thread, i, int64(i+1), "completed", "assistant"
				if imported {
					batch.Rows = append(batch.Rows, ImportRow{Item: row})
				} else if err := insertCarded(s, row); err != nil {
					t.Fatal(err)
				}
			}
			if imported {
				if err := s.ApplyImportBatch(thread, batch); err != nil {
					t.Fatal(err)
				}
			}
			for _, tc := range []struct {
				anchor string
				want   []string
			}{
				{"root", []string{"prompt", "tool", "denied", "answer"}},
				{"done", []string{"prompt", "tool", "denied", "answer"}},
				{"resume", []string{"prompt2", "tool2", "answer2"}},
				{"done2", []string{"prompt2", "tool2", "answer2"}},
			} {
				page, err := s.ListThreadSliceAround(context.Background(), thread, "", 40, 30, TimelineSelection{ScopeRootID: "root", DigestItemID: tc.anchor})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(itemIDs(page.Items), tc.want) {
					t.Fatalf("%s: %v, want %v", tc.anchor, itemIDs(page.Items), tc.want)
				}
				if page.Scope.Digest == nil || page.Scope.Digest.AnswerID != tc.want[len(tc.want)-1] {
					t.Fatalf("bad context: %+v", page.Scope)
				}
			}
			for _, selection := range []TimelineSelection{
				{DigestItemID: "done"}, {ScopeRootID: "root", DigestItemID: "prompt"},
				{ScopeRootID: "root", DigestItemID: "done", Tools: true},
				{ScopeRootID: "root", DigestItemID: strings.Repeat("x", maxHeldWindowIDBytes+1)},
			} {
				if _, err := s.ListThreadSliceAround(context.Background(), thread, "", 40, 10, selection); err == nil {
					t.Fatalf("accepted invalid selection: %+v", selection)
				}
			}
		})
	}
}

// A Claude card is the agent as of the stop it sits at: the digest at a
// parked stop holds every row up to the park, and the digest at the ending
// stop holds every row up to the end, the parked run's rows included.
func TestTimelineDigestClaudeCardCoversEveryRowUpToItsStop(t *testing.T) {
	s := newTestStore(t)
	const thread = "digest"
	newImportTargetThread(t, s, thread)
	rows := []Item{
		{ID: "root", Kind: "tool_call", ToolName: "Agent", Status: "running", IsBackground: true},
		{ID: "prompt", Kind: "user_text", ParentID: "root", Status: "completed"},
		{ID: "tool1", Kind: "tool_call", ToolName: "Bash", ParentID: "root", Status: "completed"},
		{ID: "report", Kind: "assistant_text", ParentID: "root", Status: "completed"},
		{ID: "parked", Kind: "tool_completion", ToolName: "Agent", CompletionOf: "root", Status: ItemStatusParked, IsBackground: true},
		{ID: "wake", Kind: "user_text", ParentID: "root", Status: "completed", Meta: `{"subagent_wake_prompt":true}`},
		{ID: "tool2", Kind: "tool_call", ToolName: "Bash", ParentID: "root", Status: "completed"},
		{ID: "answer", Kind: "assistant_text", ParentID: "root", Status: "completed"},
		{ID: "done", Kind: "tool_completion", ToolName: "Agent", CompletionOf: "root", Status: "completed", IsBackground: true},
		{ID: "late", Kind: "tool_call", ToolName: "Bash", ParentID: "root", Status: "completed"},
	}
	for i, row := range rows {
		row.ThreadID, row.ItemIndex, row.CreatedAt, row.Role = thread, i, int64(i+1), "assistant"
		if err := insertCarded(s, row); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		anchor string
		want   []string
		answer string
	}{
		{"parked", []string{"prompt", "tool1", "report"}, "report"},
		{"done", []string{"prompt", "tool1", "tool2", "answer"}, "answer"},
	} {
		page, err := s.ListThreadSliceAround(context.Background(), thread, "", 40, 30, TimelineSelection{ScopeRootID: "root", DigestItemID: tc.anchor})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(itemIDs(page.Items), tc.want) {
			t.Fatalf("%s: %v, want %v", tc.anchor, itemIDs(page.Items), tc.want)
		}
		if d := page.Scope.Digest; d == nil || d.PromptID != "prompt" || d.AnswerID != tc.answer {
			t.Fatalf("%s: digest context %+v, want prompt %q and answer %q", tc.anchor, d, "prompt", tc.answer)
		}
	}
}

// A Codex completion without saved execution bounds (a mailbox delivery)
// covers the rows since the launch's previous completion, while a Claude
// ending card from the same row shape covers every row from the launch.
func TestTimelineDigestBoundlessCodexCompletionStartsAfterThePreviousOne(t *testing.T) {
	for _, launch := range []Item{
		{ID: "root", Kind: "tool_call", ToolName: "collab_agent", Status: "completed", IsBackground: true, Meta: `{"input":{"tool":"spawn_agent"}}`},
		{ID: "root", Kind: "tool_call", ToolName: "Agent", Status: "running", IsBackground: true},
	} {
		t.Run(launch.ToolName, func(t *testing.T) {
			s := newTestStore(t)
			const thread = "digest"
			newImportTargetThread(t, s, thread)
			rows := []Item{
				launch,
				{ID: "tool1", Kind: "tool_call", ToolName: "Bash", ParentID: "root", Status: "completed"},
				{ID: "answer1", Kind: "assistant_text", ParentID: "root", Status: "completed"},
				{ID: "done1", Kind: "tool_completion", ToolName: launch.ToolName, CompletionOf: "root", Status: "completed", IsBackground: true},
				{ID: "tool2", Kind: "tool_call", ToolName: "Bash", ParentID: "root", Status: "completed"},
				{ID: "answer2", Kind: "assistant_text", ParentID: "root", Status: "completed"},
				{ID: "done2", Kind: "tool_completion", ToolName: launch.ToolName, CompletionOf: "root", Status: "completed", IsBackground: true},
			}
			for i, row := range rows {
				row.ThreadID, row.ItemIndex, row.CreatedAt, row.Role = thread, i, int64(i+1), "assistant"
				if err := insertCarded(s, row); err != nil {
					t.Fatal(err)
				}
			}
			want := []string{"tool2", "answer2"}
			if launch.ToolName != "collab_agent" {
				want = []string{"tool1", "tool2", "answer2"}
			}
			page, err := s.ListThreadSliceAround(context.Background(), thread, "", 40, 30, TimelineSelection{ScopeRootID: "root", DigestItemID: "done2"})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(itemIDs(page.Items), want) {
				t.Fatalf("done2: %v, want %v", itemIDs(page.Items), want)
			}
		})
	}
}

func TestTimelineDigestCodexCompletionDoesNotGrow(t *testing.T) {
	s := newTestStore(t)
	newImportTargetThread(t, s, "digest")
	seedAnchorItem(t, s, "digest", "root", 2, 0)
	for i := 1; i <= 8; i++ {
		seedToolChildItem(t, s, "digest", fmt.Sprint(i), 2, i, "root", "tool", "completed")
	}
	for _, tc := range []struct {
		id, meta string
		index    int
		want     []string
	}{
		{"done", `{"codex_execution_child_start_index":0,"codex_execution_child_end_index":3}`, 9, []string{"1", "2", "3"}},
		{"done2", `{"codex_execution_child_start_index":3,"codex_execution_child_end_index":6}`, 10, []string{"4", "5", "6"}},
	} {
		if err := insertCarded(s, Item{ID: tc.id, ThreadID: "digest", Kind: "tool_completion", Role: "assistant", Status: "completed", TurnIndex: 3, ItemIndex: tc.index, CompletionOf: "root", Meta: tc.meta}); err != nil {
			t.Fatal(err)
		}
		page, err := s.ListThreadSliceAround(context.Background(), "digest", "", 40, 10, TimelineSelection{ScopeRootID: "root", DigestItemID: tc.id})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(itemIDs(page.Items), tc.want) {
			t.Fatalf("%s: %v", tc.id, itemIDs(page.Items))
		}
	}
}

func TestTimelineDigestLoadsBoundedRunAndPagesEveryMember(t *testing.T) {
	s := newTestStore(t)
	newImportTargetThread(t, s, "digest")
	seedAnchorItem(t, s, "digest", "root", 0, 0)
	for i := 1; i <= 1200; i++ {
		seedToolChildItem(t, s, "digest", fmt.Sprintf("tool-%04d", i), 0, i, "root", "tool", "completed")
	}
	selection := TimelineSelection{ScopeRootID: "root", DigestItemID: "root"}
	page, err := s.ListThreadSliceAround(context.Background(), "digest", "", 200, 30, selection)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 30 || len(page.Runs) != 1 || page.Runs[0].UnshippedBefore != 1170 {
		t.Fatalf("unbounded or incomplete initial page: %d items, %+v", len(page.Items), page.Runs)
	}
	seen := map[string]bool{}
	for _, it := range page.Items {
		seen[it.ID] = true
	}
	run := page.Runs[0]
	for run.UnshippedBefore > 0 {
		members, err := s.ListActivityRunMembers(context.Background(), "digest", ActivityRunMembersRequest{Selection: selection, RunFirstItemID: run.FirstItemID, LoadedFirstItemID: run.LoadedFirstItemID, LoadedLastItemID: run.LoadedLastItemID, Direction: ActivityRunMembersBefore, Limit: 25})
		if err != nil {
			t.Fatal(err)
		}
		if len(members.Items) > 25 || members.Stub.UnshippedBefore >= run.UnshippedBefore {
			t.Fatalf("member page did not advance within budget: %+v", members.Stub)
		}
		for _, it := range members.Items {
			seen[it.ID] = true
		}
		run = members.Stub
	}
	if len(seen) != 1200 {
		t.Fatalf("reached %d tools", len(seen))
	}
}
