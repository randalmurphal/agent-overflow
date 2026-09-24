package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The newest direct assistant_text child of the root is the report: an
// older one, a blank one, a nested agent's text and a tool call do not
// win, and the preview is the head of the summary in runes.
func TestLatestSubagentReportIsTheNewestDirectText(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("t", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	long := strings.Repeat("é", SubagentReportPreviewRunes+40)
	rows := []Item{
		{ID: "root", Kind: "tool_call", ToolName: "Agent", Status: "running", IsBackground: true, Summary: "Agent: spike"},
		{ID: "older", Kind: "assistant_text", ParentID: "root", Summary: "Round one report"},
		{ID: "newest", Kind: "assistant_text", ParentID: "root", Summary: "\n  " + long + "  "},
		{ID: "blank", Kind: "assistant_text", ParentID: "root", Summary: " \n\t "},
		{ID: "tool", Kind: "tool_call", ToolName: "Read", ParentID: "root", Status: "completed", Summary: "Read: a.go"},
		{ID: "nested", Kind: "tool_call", ToolName: "Agent", ParentID: "root", Status: "running", IsBackground: true, Summary: "Agent: nested"},
		{ID: "nested-text", Kind: "assistant_text", ParentID: "nested", Summary: "Nested report"},
	}
	for i, row := range rows {
		row.ThreadID, row.TurnIndex, row.ItemIndex, row.Role = "t", 0, i, "assistant"
		if row.Status == "" {
			row.Status = "completed"
		}
		if err := insertCarded(s, row); err != nil {
			t.Fatalf("seed %s: %v", row.ID, err)
		}
	}

	report, found, err := s.LatestSubagentReport("t", "root")
	if err != nil || !found {
		t.Fatalf("report: found=%v err=%v", found, err)
	}
	if report.ID != "newest" {
		t.Errorf("report = %s, want newest", report.ID)
	}
	if got := utf8.RuneCountInString(report.Preview); got != SubagentReportPreviewRunes {
		t.Errorf("preview runes = %d, want %d", got, SubagentReportPreviewRunes)
	}
	if !strings.HasPrefix(report.Preview, "é") {
		t.Errorf("preview %q keeps the leading blank", report.Preview[:8])
	}

	if nested, found, err := s.LatestSubagentReport("t", "nested"); err != nil || !found || nested.ID != "nested-text" {
		t.Errorf("nested report = %+v found=%v err=%v, want nested-text", nested, found, err)
	}
	for _, root := range []string{"tool", "missing", ""} {
		if got, found, err := s.LatestSubagentReport("t", root); err != nil || found {
			t.Errorf("report of %q = %+v found=%v err=%v, want none", root, got, found, err)
		}
	}
}
