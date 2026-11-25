package trackerfake

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"strings"
	"testing"
)

// The changelog shapes are measured on a real site
// (docs/references/tracker-api-measurements.md, Jira Cloud, "Where a
// changelog item holds its values depends on the field"); each test reads
// the fake's changelog after the write that causes the item.

func changeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	if err := w.e.Seed(changeFixture()); err != nil {
		t.Fatal(err)
	}
	return w
}

// changeFixture is testFixture with two boards on WEB, a sprint and
// three issues.
func changeFixture() Fixture {
	f := testFixture()
	s := &f.Jira[0]
	s.Users = append(s.Users, JiraUser{AccountID: "acc-user2", DisplayName: "User 2", Email: "user2@example.com"})
	s.Fields = []JiraField{{ID: "customfield_10016", Name: "Story Points", Schema: JiraSchema{Type: "number", Custom: "com.atlassian.jira.plugin.system.customfieldtypes:float", CustomID: 10016}}}
	s.Projects[0].Components = []JiraNamed{{ID: "30001", Name: "UI"}}
	columns := []JiraColumn{{Name: "To Do", Statuses: []string{"10000"}}, {Name: "In Progress", Statuses: []string{"10001"}}, {Name: "Done", Statuses: []string{"10002"}}}
	s.Boards = []JiraBoard{
		{ID: 1, Name: "Points", Type: "scrum", Project: "WEB", Estimation: "customfield_10016", Columns: columns},
		{ID: 2, Name: "Time", Type: "scrum", Project: "WEB", Estimation: "timeoriginalestimate", Columns: columns},
	}
	s.Sprints = []JiraSprint{{ID: 1, Board: 1, Name: "Sprint 1", State: "future"}}
	s.Issues[0].Values = map[string]json.RawMessage{"customfield_10016": json.RawMessage(`3`)}
	s.Issues = append(s.Issues,
		JiraIssue{ID: "10102", Key: "WEB-2", Project: "WEB", Type: "10001", Summary: "Second"},
		JiraIssue{ID: "10103", Key: "WEB-3", Project: "WEB", Type: "10000", Summary: "An epic"})
	return f
}

// lastItem is the newest changelog item of key's issue whose fieldId, or
// field for an item with none, is field, as the wire carries it.
func (w *world) lastItem(t *testing.T, key, field string) map[string]any {
	t.Helper()
	r := w.jira(t, "GET", "/rest/api/3/issue/"+key+"/changelog", "")
	if r.status != 200 {
		t.Fatalf("changelog answered %d %s", r.status, r.body)
	}
	var page struct {
		Values []struct{ Items []map[string]any }
	}
	if err := json.Unmarshal([]byte(r.body), &page); err != nil {
		t.Fatal(err)
	}
	for i := len(page.Values) - 1; i >= 0; i-- {
		for _, it := range page.Values[i].Items {
			if it["fieldId"] == field || (it["fieldId"] == nil && it["field"] == field) {
				return it
			}
		}
	}
	t.Fatalf("no %s item in %s", field, r.body)
	return nil
}

// wantItem checks members of it; absent in want names a member that must
// not be on the wire.
func wantItem(t *testing.T, it map[string]any, want map[string]any) {
	t.Helper()
	for k, v := range want {
		got, ok := it[k]
		if v == absent {
			if ok {
				t.Fatalf("item %v has %s", it, k)
			}
			continue
		}
		if !ok || got != v {
			t.Fatalf("item %v: %s is %#v, want %#v", it, k, got, v)
		}
	}
}

const absent = "<absent>"

func (w *world) write(t *testing.T, method, path, body string, status int) reply {
	t.Helper()
	r := w.jira(t, method, path, body)
	if r.status != status {
		t.Fatalf("%s %s answered %d %s", method, path, r.status, r.body)
	}
	return r
}

func TestChangelogCustomNumber(t *testing.T) {
	w := changeWorld(t)
	w.write(t, "PUT", "/rest/agile/1.0/issue/WEB-1/estimation?boardId=1", `{"value":"8"}`, 200)
	wantItem(t, w.lastItem(t, "WEB-1", "customfield_10016"), map[string]any{
		"fieldtype": "custom", "from": nil, "to": nil, "fromString": "3", "toString": "8",
	})
	w.write(t, "PUT", "/rest/agile/1.0/issue/WEB-1/estimation?boardId=1", `{"value":null}`, 200)
	wantItem(t, w.lastItem(t, "WEB-1", "customfield_10016"), map[string]any{"fromString": "8", "toString": ""})
	w.write(t, "PUT", "/rest/agile/1.0/issue/WEB-2/estimation?boardId=1", `{"value":"5"}`, 200)
	wantItem(t, w.lastItem(t, "WEB-2", "customfield_10016"), map[string]any{"fromString": nil, "toString": "5"})
}

func TestChangelogTimeEstimates(t *testing.T) {
	w := changeWorld(t)
	r := w.write(t, "PUT", "/rest/agile/1.0/issue/WEB-1/estimation?boardId=2", `{"value":"90m"}`, 200)
	if !bytes.Contains([]byte(r.body), []byte(`"value":"90m"`)) {
		t.Fatalf("estimation answered %s", r.body)
	}
	wantItem(t, w.lastItem(t, "WEB-1", "timeoriginalestimate"), map[string]any{"fieldtype": "jira", "from": nil, "to": "5400", "toString": "5400"})
	wantItem(t, w.lastItem(t, "WEB-1", "timeestimate"), map[string]any{"from": nil, "to": "5400"})
	if r := w.write(t, "PUT", "/rest/agile/1.0/issue/WEB-2/estimation?boardId=2", `{"value":"120m"}`, 200); !bytes.Contains([]byte(r.body), []byte(`"value":"2h"`)) {
		t.Fatalf("a whole-hour estimation answered %s", r.body)
	}
}

func TestChangelogSprint(t *testing.T) {
	w := changeWorld(t)
	w.write(t, "POST", "/rest/agile/1.0/sprint/1/issue", `{"issues":["WEB-1"]}`, 204)
	wantItem(t, w.lastItem(t, "WEB-1", "customfield_10020"), map[string]any{"fieldtype": "custom", "field": "Sprint", "from": "", "fromString": "", "to": "1", "toString": "Sprint 1"})
	w.write(t, "POST", "/rest/agile/1.0/backlog/issue", `{"issues":["WEB-1"]}`, 204)
	wantItem(t, w.lastItem(t, "WEB-1", "customfield_10020"), map[string]any{"from": "1", "to": "", "toString": ""})
}

func TestChangelogStatusResolutionAssignee(t *testing.T) {
	w := changeWorld(t)
	w.write(t, "POST", "/rest/api/3/issue/WEB-1/transitions", `{"transition":{"id":"31"}}`, 204)
	wantItem(t, w.lastItem(t, "WEB-1", "status"), map[string]any{"fieldtype": "jira", "from": "10000", "to": "10002", "toString": "Done"})
	wantItem(t, w.lastItem(t, "WEB-1", "resolution"), map[string]any{"from": nil, "to": "10000", "toString": "Done"})
	w.write(t, "PUT", "/rest/api/3/issue/WEB-1/assignee", `{"accountId":"acc-user2"}`, 204)
	wantItem(t, w.lastItem(t, "WEB-1", "assignee"), map[string]any{"from": nil, "to": "acc-user2"})
}

func TestChangelogTextFields(t *testing.T) {
	w := changeWorld(t)
	w.write(t, "PUT", "/rest/api/3/issue/WEB-1", `{"fields":{"summary":"Renamed","labels":["ui"],"description":`+testDoc+`,"priority":{"id":"2"}}}`, 204)
	wantItem(t, w.lastItem(t, "WEB-1", "summary"), map[string]any{"from": nil, "to": nil, "fromString": "First", "toString": "Renamed"})
	wantItem(t, w.lastItem(t, "WEB-1", "labels"), map[string]any{"from": nil, "to": nil, "fromString": "", "toString": "ui"})
	wantItem(t, w.lastItem(t, "WEB-1", "description"), map[string]any{"from": nil, "to": nil, "toString": "hi"})
	wantItem(t, w.lastItem(t, "WEB-1", "priority"), map[string]any{"to": "2", "toString": "High"})
}

func TestChangelogComponents(t *testing.T) {
	w := changeWorld(t)
	w.write(t, "PUT", "/rest/api/3/issue/WEB-1", `{"fields":{"components":[{"id":"30001"}]}}`, 204)
	wantItem(t, w.lastItem(t, "WEB-1", "components"), map[string]any{"field": "Component", "from": nil, "to": "30001", "toString": "UI"})
	w.write(t, "PUT", "/rest/api/3/issue/WEB-1", `{"fields":{"components":[]}}`, 204)
	wantItem(t, w.lastItem(t, "WEB-1", "components"), map[string]any{"from": "30001", "to": nil, "fromString": "UI"})
}

func TestChangelogParent(t *testing.T) {
	w := changeWorld(t)
	w.write(t, "PUT", "/rest/api/3/issue/WEB-1", `{"fields":{"parent":{"key":"WEB-3"}}}`, 204)
	wantItem(t, w.lastItem(t, "WEB-1", "IssueParentAssociation"), map[string]any{"fieldId": absent, "fieldtype": "jira", "from": nil, "to": "10103"})
}

func TestChangelogLink(t *testing.T) {
	w := changeWorld(t)
	w.write(t, "POST", "/rest/api/3/issueLink", `{"type":{"name":"Blocks"},"outwardIssue":{"key":"WEB-1"},"inwardIssue":{"key":"WEB-2"}}`, 201)
	wantItem(t, w.lastItem(t, "WEB-1", "Link"), map[string]any{"fieldId": absent, "fieldtype": "jira", "from": nil, "to": "WEB-2"})
	var is struct {
		Fields struct{ Issuelinks []struct{ ID string } }
	}
	if err := json.Unmarshal([]byte(w.jira(t, "GET", "/rest/api/3/issue/WEB-1?fields=issuelinks", "").body), &is); err != nil || len(is.Fields.Issuelinks) != 1 {
		t.Fatalf("links %+v, %v", is, err)
	}
	w.write(t, "DELETE", "/rest/api/3/issueLink/"+is.Fields.Issuelinks[0].ID, "", 204)
	wantItem(t, w.lastItem(t, "WEB-2", "Link"), map[string]any{"from": "WEB-1", "to": nil})
}

func TestChangelogRank(t *testing.T) {
	w := changeWorld(t)
	w.write(t, "PUT", "/rest/agile/1.0/issue/rank", `{"issues":["WEB-2"],"rankBeforeIssue":"WEB-1"}`, 204)
	wantItem(t, w.lastItem(t, "WEB-2", "customfield_10019"), map[string]any{"fieldtype": "custom", "from": "", "to": "", "fromString": ""})
}

func TestChangelogAttachment(t *testing.T) {
	w := changeWorld(t)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	h := basic(testJiraToken)
	h["Content-Type"], h["X-Atlassian-Token"] = mw.FormDataContentType(), "no-check"
	r, err := send(context.Background(), "POST", w.srv.JiraURL(testSite)+"/rest/api/3/issue/WEB-1/attachments", h, body.String())
	if err != nil || r.status != 200 {
		t.Fatalf("attach: %d %s %v", r.status, r.body, err)
	}
	var atts []struct{ ID string }
	if err := json.Unmarshal([]byte(r.body), &atts); err != nil || len(atts) != 1 {
		t.Fatalf("attachments %s: %v", r.body, err)
	}
	wantItem(t, w.lastItem(t, "WEB-1", "attachment"), map[string]any{"from": nil, "to": atts[0].ID, "toString": "notes.txt"})
	w.write(t, "DELETE", "/rest/api/3/attachment/"+atts[0].ID, "", 204)
	wantItem(t, w.lastItem(t, "WEB-1", "attachment"), map[string]any{"from": atts[0].ID, "to": nil})
}

// TestSearchShapes: no ORDER BY is the fake's own order, neither by ID
// nor by creation; a search that matches nothing answers 200 with no
// warnings member.
func TestSearchShapes(t *testing.T) {
	w := changeWorld(t)
	f := testFixture()
	for n := range 6 {
		id := 20000 + n
		f.Jira[0].Issues = append(f.Jira[0].Issues, JiraIssue{ID: itoa(id), Key: "WEB-" + itoa(10+n), Project: "WEB", Type: "10001", Summary: "More"})
	}
	if err := w.e.Seed(f); err != nil {
		t.Fatal(err)
	}
	var page struct {
		Issues []struct{ ID string }
	}
	r := w.write(t, "POST", "/rest/api/3/search/jql", `{"jql":"project = WEB","fields":["id"]}`, 200)
	if err := json.Unmarshal([]byte(r.body), &page); err != nil {
		t.Fatal(err)
	}
	up, down := true, true
	for i := 1; i < len(page.Issues); i++ {
		up = up && page.Issues[i-1].ID < page.Issues[i].ID
		down = down && page.Issues[i-1].ID > page.Issues[i].ID
	}
	if len(page.Issues) != 7 || up || down {
		t.Fatalf("an unordered search came back by ID: %s", r.body)
	}
	r = w.write(t, "POST", "/rest/api/3/search/jql", `{"jql":"project = NOPE","fields":["id"]}`, 200)
	if bytes.Contains([]byte(r.body), []byte("warnings")) || !bytes.Contains([]byte(r.body), []byte(`"issues":[]`)) {
		t.Fatalf("a search for a missing project answered %s", r.body)
	}
}

func itoa(n int) string { return string(mustJSON(n)) }

// TestTransitionScreenResolution proves a transition whose screen takes
// the resolution lists the site's resolutions as its allowed values, sets
// the one picked, and records one resolution item for it.
func TestTransitionScreenResolution(t *testing.T) {
	f := changeFixture()
	s := &f.Jira[0]
	s.Resolutions = []JiraNamed{{ID: "10000", Name: "Done"}, {ID: "10001", Name: "Fixed"}}
	s.Projects[0].Transitions = []JiraTransition{
		{ID: "11", Name: "To Do", To: "10000"},
		{ID: "31", Name: "Resolve", To: "10002", Screen: []JiraScreenField{{Field: "resolution", Required: true}}},
	}
	w := newWorld(t)
	if err := w.e.Seed(f); err != nil {
		t.Fatal(err)
	}
	r := w.jira(t, "GET", "/rest/api/3/issue/WEB-1/transitions?expand=transitions.fields", "")
	var got struct {
		Transitions []struct {
			ID     string
			Fields map[string]struct {
				Required      bool
				Operations    []string
				AllowedValues []struct{ ID, Name string }
			}
		}
	}
	if err := json.Unmarshal([]byte(r.body), &got); err != nil || len(got.Transitions) != 2 {
		t.Fatalf("transitions %s %v", r.body, err)
	}
	res := got.Transitions[1].Fields["resolution"]
	if !res.Required || len(res.Operations) != 1 || res.Operations[0] != "set" || len(res.AllowedValues) != 2 || res.AllowedValues[1].Name != "Fixed" {
		t.Fatalf("Resolve's resolution %+v", res)
	}
	w.write(t, "POST", "/rest/api/3/issue/WEB-1/transitions", `{"transition":{"id":"31"},"fields":{"resolution":{"id":"10001"}}}`, 204)
	wantItem(t, w.lastItem(t, "WEB-1", "resolution"), map[string]any{"from": nil, "to": "10001", "toString": "Fixed"})
	if body := w.jira(t, "GET", "/rest/api/3/issue/WEB-1?fields=resolution", "").body; !strings.Contains(body, `"name":"Fixed"`) {
		t.Fatalf("the issue after Resolve: %s", body)
	}
	var changes struct {
		Values []struct{ Items []map[string]any }
	}
	if err := json.Unmarshal([]byte(w.jira(t, "GET", "/rest/api/3/issue/WEB-1/changelog", "").body), &changes); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, v := range changes.Values {
		for _, it := range v.Items {
			if it["fieldId"] == "resolution" {
				n++
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d resolution items, want 1", n)
	}
	if r := w.write(t, "POST", "/rest/api/3/issue/WEB-1/transitions", `{"transition":{"id":"11"}}`, 204); r.status != 204 {
		t.Fatalf("back to To Do: %d", r.status)
	}
	w.write(t, "POST", "/rest/api/3/issue/WEB-1/transitions", `{"transition":{"id":"31"},"fields":{"resolution":{"id":"99"}}}`, 400)
}
