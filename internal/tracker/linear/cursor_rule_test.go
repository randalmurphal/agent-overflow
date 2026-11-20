package linear

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker/linear/lineartest"
)

// A drain page that says more follow from the cursor it was asked from
// fails: the stored position would not move and every later poll would
// deliver the same page.
func TestDrainCursorMustAdvance(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cursor := "c1"
	vars := `{"first":250,"after":"c1","sort":[{"createdAt":{"order":"Ascending"}}],"filter":{"and":[{"updatedAt":{"eq":"2026-11-01T00:00:00.000Z"}}]}}`
	issue := lineartest.IssueJSON(uuid1, "WEB-1", "2026-11-01T00:00:00.000Z", 1)
	f := newFake(t, lineartest.Exchange{Op: "Issues", Vars: vars, Body: lineartest.Data(lineartest.PageJSON([]string{issue}, true, "c1"))})
	_, err := f.client(t).PollIssues(context.Background(), PollQuery{From: PollCursor{Position: Position{Bound: at, Drain: &cursor}}})
	if err == nil || !strings.Contains(err.Error(), "did not advance") {
		t.Fatalf("a drain page from the cursor it was asked from: %v", err)
	}
}

// Every document that declares $after is marked paged, and fetchPage
// refuses to send one with variables that hold no cursor, so no cursor
// walk can be written that skips the rule above.
func TestPagedDocumentsCarryTheirCursor(t *testing.T) {
	marked := 0
	for name, op := range registered {
		declared := strings.Contains(op.text, "$after")
		if declared != op.paged {
			t.Errorf("%s: declares $after %v, marked paged %v", name, declared, op.paged)
		}
		if op.paged {
			marked++
		}
	}
	if marked == 0 {
		t.Fatal("no document is marked paged")
	}
	type bare struct {
		First int `json:"first"`
	}
	c := newFake(t).client(t)
	if _, err := fetchPage(context.Background(), c, opIssues, bare{First: 1}, wireIssue.typed); err == nil || !strings.Contains(err.Error(), "hold no cursor") {
		t.Fatalf("a paged document sent with variables that hold no cursor: %v", err)
	}
}
