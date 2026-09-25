package store

import (
	"testing"
)

// The newest direct assistant_text child of the root written since the
// run began is the report: an older one, one before the run, a blank one,
// a nested agent's text and a tool call do not win.
func TestLatestSubagentReportIsTheNewestDirectTextOfTheRun(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	rows := []Item{
		{ID: "root", Kind: "tool_call", ToolName: "Agent", Status: "running", IsBackground: true, Summary: "Agent: spike", CreatedAt: 1000},
		{ID: "older", Kind: "assistant_text", ParentID: "root", Summary: "Round one report", CreatedAt: 1100},
		{ID: "newest", Kind: "assistant_text", ParentID: "root", Summary: "Round two report", CreatedAt: 2100},
		{ID: "blank", Kind: "assistant_text", ParentID: "root", Summary: " \n\t ", CreatedAt: 2200},
		{ID: "tool", Kind: "tool_call", ToolName: "Read", ParentID: "root", Status: "completed", Summary: "Read: a.go", CreatedAt: 2300},
		{ID: "nested", Kind: "tool_call", ToolName: "Agent", ParentID: "root", Status: "running", IsBackground: true, Summary: "Agent: nested", CreatedAt: 2400},
		{ID: "nested-text", Kind: "assistant_text", ParentID: "nested", Summary: "Nested report", CreatedAt: 2500},
	}
	for i, row := range rows {
		row.ThreadID, row.TurnIndex, row.ItemIndex, row.Role = "t", 0, i, "assistant"
		row.UpdatedAt = row.CreatedAt
		if row.Status == "" {
			row.Status = "completed"
		}
		if err := insertCarded(s, row); err != nil {
			t.Fatalf("seed %s: %v", row.ID, err)
		}
	}

	for _, since := range []int64{0, 1100, 2100} {
		if id, found, err := s.LatestSubagentReport("t", "root", since); err != nil || !found || id != "newest" {
			t.Errorf("report since %d = %q found=%v err=%v, want newest", since, id, found, err)
		}
	}
	// A run that began after the last text has written no report.
	if id, found, err := s.LatestSubagentReport("t", "root", 2101); err != nil || found {
		t.Errorf("report since 2101 = %q found=%v err=%v, want none", id, found, err)
	}
	if id, found, err := s.LatestSubagentReport("t", "nested", 0); err != nil || !found || id != "nested-text" {
		t.Errorf("nested report = %q found=%v err=%v, want nested-text", id, found, err)
	}
	for _, root := range []string{"tool", "missing", ""} {
		if id, found, err := s.LatestSubagentReport("t", root, 0); err != nil || found {
			t.Errorf("report of %q = %q found=%v err=%v, want none", root, id, found, err)
		}
	}
}
