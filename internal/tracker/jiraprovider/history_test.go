package jiraprovider

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// changeEntry is a bulk changelog entry by User 2 at created (a Jira
// timestamp) with items. The bulk fetch sends created as epoch
// milliseconds, a number (measured).
func changeEntry(id, created string, items ...map[string]any) map[string]any {
	at, err := time.Parse("2006-01-02T15:04:05.000-0700", created)
	if err != nil {
		panic(err)
	}
	return map[string]any{"id": id, "author": json.RawMessage(user2JSON), "created": at.UnixMilli(), "items": items}
}

// The items below are in the shapes measured on a site
// (docs/references/tracker-api-measurements.md, "Where a changelog item
// holds its values depends on the field"); members the measurement does
// not describe are left out.

func statusItem(from, to string) map[string]any {
	return map[string]any{"field": "status", "fieldId": "status", "fieldtype": "jira", "from": from, "to": to}
}

func assigneeItem(from, to any) map[string]any {
	return map[string]any{"field": "assignee", "fieldId": "assignee", "fieldtype": "jira", "from": from, "to": to}
}

// sprintItem is a Sprint change; no sprint is the empty string.
func sprintItem(from, to string) map[string]any {
	return map[string]any{"field": "Sprint", "fieldId": "customfield_10020", "fieldtype": "custom", "from": from, "to": to}
}

// pointsItem is a custom number field's change: from and to null, the
// numbers in the text members.
func pointsItem(from, to string) map[string]any {
	return map[string]any{"field": "Story points", "fieldId": "customfield_10016", "fieldtype": "custom", "from": nil, "fromString": from, "to": nil, "toString": to}
}

// timeItem is a time estimate change: seconds in from and to, equal to
// the text members.
func timeItem(from, to string) map[string]any {
	return map[string]any{"field": "timeoriginalestimate", "fieldId": "timeoriginalestimate", "fieldtype": "jira", "from": from, "fromString": from, "to": to, "toString": to}
}

func TestHistory(t *testing.T) {
	f, p := newSite(t)
	f.On("POST", changelogsPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issueIdsOrKeys":["10001","10002","10003"],"fieldIds":["status","assignee","customfield_10020","customfield_10016","timeoriginalestimate"],"maxResults":1000}`)
		jiratest.Reply(200, mustJSON(map[string]any{"issueChangeLogs": []any{
			map[string]any{"issueId": "10001", "changeHistories": []any{
				changeEntry("100", "2026-11-02T09:00:00.000+0000",
					statusItem("10000", "10001"),
					assigneeItem(nil, "acc-user1"),
					sprintItem("", "7"),
					pointsItem("3", "5"),
				),
				changeEntry("101", "2026-11-03T09:00:00.000+0000",
					sprintItem("7", "10, 8"),
					timeItem("3600", "7200"),
					pointsItem("5", "8"),
				),
			}},
		}}))(w, r, b)
	})
	// 10002 has no changes; 10003 is not visible.
	f.On("POST", bulkFetchPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issueIdsOrKeys":["10002","10003"],"fields":["status"]}`)
		jiratest.Reply(200, page("", issue("10002", "WEB-2", "2026-11-01T09:00:00.000+0000", nil)))(w, r, b)
	})
	// The clock reads 12:00 when the read begins and an hour later once Jira
	// has answered, so a CompleteTo taken after the read is caught.
	began := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	calls := 0
	p.now = func() time.Time {
		calls++
		return began.Add(time.Duration(calls-1) * time.Hour)
	}
	h, err := p.History(t.Context(), tracker.HistoryQuery{Issues: []tracker.IssueID{"10001", "10002", "10003", "10001"}, Boards: []tracker.BoardID{"3", "4"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, ih := range h.Issues {
		if want := time.Date(2026, 11, 10, 11, 59, 0, 0, time.UTC); !ih.CompleteTo.Equal(want) {
			t.Errorf("issue %s final through %v, want %v", ih.Issue, ih.CompleteTo, want)
		}
	}
	if len(h.Issues) != 2 || h.Issues[0].Issue != "10001" || h.Issues[1].Issue != "10002" {
		t.Fatalf("issues %+v", h.Issues)
	}
	if empty := h.Issues[1]; empty.Status != nil || empty.Sprint != nil || empty.Estimate != nil || empty.Assignee != nil || !empty.CompleteFrom.IsZero() {
		t.Errorf("an issue without changes %+v", empty)
	}
	one := h.Issues[0]
	// Times compare by instant, whatever zone they decoded in.
	for i := range one.Status {
		one.Status[i].At = one.Status[i].At.UTC()
	}
	for i := range one.Assignee {
		one.Assignee[i].At = one.Assignee[i].At.UTC()
	}
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	ch := tracker.Change{At: at, Author: "acc-user2"}
	if !slices.Equal(one.Status, []tracker.StatusChange{{Change: ch, From: "10000", To: "10001"}}) {
		t.Errorf("status %+v", one.Status)
	}
	if !slices.Equal(one.Assignee, []tracker.AssigneeChange{{Change: ch, From: "", To: "acc-user1"}}) {
		t.Errorf("assignee %+v", one.Assignee)
	}
	if len(one.Sprint) != 2 || !slices.Equal(one.Sprint[0].To, []tracker.SprintID{"7"}) || one.Sprint[0].From != nil ||
		!slices.Equal(one.Sprint[1].To, []tracker.SprintID{"10", "8"}) {
		t.Errorf("sprints %+v", one.Sprint)
	}
	var ests []string
	for _, e := range one.Estimate {
		ests = append(ests, string(e.Board)+":"+num(e.From)+">"+num(e.To))
	}
	if want := []string{"3:3>5", "4:3600>7200", "3:5>8"}; !slices.Equal(ests, want) {
		t.Errorf("estimates %v, want %v", ests, want)
	}
	if len(h.Users) != 1 || h.Users[0].ID != "acc-user2" {
		t.Errorf("users %+v", h.Users)
	}
}

func num(f *float64) string {
	if f == nil {
		return ""
	}
	return decimal(int64(*f))
}

func TestHistoryRefusals(t *testing.T) {
	f, p := newSite(t)
	if _, err := p.History(t.Context(), tracker.HistoryQuery{}); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Errorf("no issues: %v", err)
	}
	if _, err := p.History(t.Context(), tracker.HistoryQuery{Issues: []tracker.IssueID{"WEB-1"}}); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Errorf("a key: %v", err)
	}
	f.On("POST", changelogsPath, jiratest.Reply(200, mustJSON(map[string]any{"issueChangeLogs": []any{
		map[string]any{"issueId": "10001", "changeHistories": []any{changeEntry("100", "2026-11-02T09:00:00.000+0000", sprintItem("x", "7"))}},
	}})))
	if _, err := p.History(t.Context(), tracker.HistoryQuery{Issues: []tracker.IssueID{"10001"}}); err == nil {
		t.Error("a sprint change that does not read was taken")
	}
	f.On("POST", changelogsPath, jiratest.Reply(200, `{"issueChangeLogs":[]}`))
	f.On("POST", bulkFetchPath, jiratest.Reply(200, `{"issues":[],"issueErrors":[{"id":"10001","errorMessage":"busy"}]}`))
	if _, err := p.History(t.Context(), tracker.HistoryQuery{Issues: []tracker.IssueID{"10001"}}); err == nil {
		t.Error("an issue Jira could not read was taken as not visible")
	}
}
