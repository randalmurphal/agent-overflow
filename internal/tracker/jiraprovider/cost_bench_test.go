package jiraprovider

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// The provider's mapping costs at realistic sizes, after the client has
// decoded: a 100-row page into neutral rows, 20 issues' histories of 100
// changes each, and one issue's detail fields against a 1,500-field
// catalog.

func benchSite(fields int) site {
	var list []jira.Field
	for i := range fields {
		id := fmt.Sprintf("customfield_%d", 10100+i)
		list = append(list, jira.Field{ID: id, Name: fmt.Sprintf("Field %d", i), Custom: true,
			Schema: jira.Schema{Type: "string", Custom: "com.atlassian.jira.plugin.system.customfieldtypes:textfield"}})
	}
	list = append(list,
		jira.Field{ID: "customfield_10020", Name: "Sprint", Custom: true, Schema: jira.Schema{Type: "array", Items: "json", Custom: sprintType}},
		jira.Field{ID: "customfield_10019", Name: "Rank", Custom: true, Schema: jira.Schema{Type: "any", Custom: rankType}},
		jira.Field{ID: "customfield_10016", Name: "Story Points", Custom: true, Schema: jira.Schema{Type: "number", Custom: "com.atlassian.jira.plugin.system.customfieldtypes:float"}},
	)
	return site{fields: jira.NewCatalog(list), sprint: "customfield_10020"}
}

func benchIssues(n int) []*jira.Issue {
	out := make([]*jira.Issue, n)
	at := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	for i := range out {
		out[i] = &jira.Issue{
			ID: decimal(int64(10000 + i)), Key: fmt.Sprintf("WEB-%d", i+1), Summary: "Issue with a title of ordinary length",
			Status:   &jira.Status{ID: "10001", Name: "In Progress", Category: jira.StatusCategory{Key: "indeterminate"}},
			Type:     &jira.IssueType{ID: "10002", Name: "Task"},
			Priority: &jira.Priority{ID: "3", Name: "Medium"},
			Assignee: &jira.User{AccountID: fmt.Sprintf("acc-user%d", i%3+1), DisplayName: "User"},
			Project:  &jira.Project{ID: "10000", Key: "WEB"}, Labels: []string{"frontend"},
			Created: at.Add(-time.Hour), Updated: at.Add(time.Duration(i) * time.Millisecond),
			Raw: map[string]json.RawMessage{
				"customfield_10020": json.RawMessage(`[{"id":1,"name":"Sprint 1","state":"active","boardId":1}]`),
				"customfield_10019": json.RawMessage(fmt.Sprintf(`"0|i%05d:"`, i)),
				"customfield_10016": json.RawMessage(`3`),
				"resolutiondate":    json.RawMessage(`null`),
			},
		}
	}
	return out
}

// BenchmarkRowsOfPage maps a decoded 100-issue page to rows, as
// ChangedRows and BoardRows do.
func BenchmarkRowsOfPage(b *testing.B) {
	s := benchSite(1500)
	bd := board{id: "1", rank: "customfield_10019", cfg: jira.BoardConfig{BoardID: 1, RankFieldID: 10019,
		Estimation: jira.Estimation{Type: jira.EstimateField, FieldID: "customfield_10016"}}}
	issues := benchIssues(100)
	b.ReportAllocs()
	for b.Loop() {
		cat := newCatalog()
		for _, is := range issues {
			if _, err := bd.rowOf(s, is, cat); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkIssueHistory maps 20 issues' changelogs of 100 status,
// assignee, sprint and estimate changes each.
func BenchmarkIssueHistory(b *testing.B) {
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	logs := make([][]jira.ChangeEntry, 20)
	for i := range logs {
		for c := range 100 {
			e := jira.ChangeEntry{ID: decimal(int64(i*1000 + c)), Author: &jira.User{AccountID: fmt.Sprintf("acc-user%d", c%3+1)}, Created: at.Add(time.Duration(c) * time.Minute)}
			switch c % 4 {
			case 0:
				e.Items = []jira.ChangeItem{{FieldID: "status", From: "10000", To: "10001"}}
			case 1:
				e.Items = []jira.ChangeItem{{FieldID: "assignee", From: "acc-user1", To: "acc-user2"}}
			case 2:
				e.Items = []jira.ChangeItem{{FieldID: "customfield_10020", From: "1", To: "1, 2"}}
			case 3:
				e.Items = []jira.ChangeItem{{FieldID: "customfield_10016", FromText: "3", ToText: "5"}}
			}
			logs[i] = append(logs[i], e)
		}
	}
	ests := []boardEstimate{{board: "1", field: "customfield_10016", text: true}}
	b.ReportAllocs()
	for b.Loop() {
		cat := newCatalog()
		for i, entries := range logs {
			if _, err := issueHistory(tracker.IssueID(decimal(int64(i))), entries, "customfield_10020", ests, cat); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkDetailFields maps one issue of 150 set custom fields, with an
// edit screen of 40, against a 1,500-field catalog.
func BenchmarkDetailFields(b *testing.B) {
	s := benchSite(1500)
	is := benchIssues(1)[0]
	var screen jira.Screen
	for i := range 150 {
		id := fmt.Sprintf("customfield_%d", 10100+i*10)
		is.Raw[id] = json.RawMessage(`"some value"`)
		if i < 40 {
			screen.Fields = append(screen.Fields, jira.FieldMeta{ID: id, Name: fmt.Sprintf("Field %d", i*10), Schema: jira.Schema{Type: "string"}})
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if out := detailFields(s, is, screen, newCatalog()); len(out) < 150 {
			b.Fatalf("%d fields", len(out))
		}
	}
}
