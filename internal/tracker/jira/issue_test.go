package jira

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"agent-overflow/internal/tracker/adf"
)

const fullIssueJSON = `{"id":"10001","key":"WEB-1","fields":{
 "summary":"Fix the login page",
 "status":{"id":"3","name":"In Progress","statusCategory":{"id":4,"key":"indeterminate","name":"In Progress","colorName":"yellow"}},
 "issuetype":{"id":"1","name":"Task","subtask":false,"hierarchyLevel":0},
 "priority":{"id":"2","name":"High"},
 "assignee":` + userOneJSON + `,
 "reporter":` + userTwoJSON + `,
 "parent":{"id":"10000","key":"WEB-100","fields":{"summary":"Login epic","status":{"id":"1","name":"To Do","statusCategory":{"key":"new"}},"issuetype":{"id":"9","name":"Epic","hierarchyLevel":1}}},
 "labels":["frontend","urgent"],
 "components":[{"id":"20","name":"Web"}],
 "fixVersions":[{"id":"30","name":"1.0","released":true,"releaseDate":"2026-10-01"}],
 "created":"2026-11-01T09:30:00.000+0000",
 "updated":"2026-11-15T17:45:12.345+0100",
 "duedate":"2026-12-01",
 "resolution":null,
 "project":{"id":"10000","key":"WEB","name":"Web","projectTypeKey":"software"},
 "description":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"Steps"}]}]},
 "attachment":[{"id":"501","filename":"shot.png","mimeType":"image/png","size":2048,"created":"2026-11-02T10:00:00.000+0000","author":` + userOneJSON + `}],
 "issuelinks":[{"id":"601","type":{"id":"7","name":"Blocks","inward":"is blocked by","outward":"blocks"},"outwardIssue":{"id":"10002","key":"WEB-2","fields":{"summary":"Other"}}},
               {"id":"602","type":{"id":"7","name":"Blocks","inward":"is blocked by","outward":"blocks"},"inwardIssue":{"id":"10003","key":"APP-3","fields":{"summary":"Blocker"}}}],
 "subtasks":[{"id":"10004","key":"WEB-4","fields":{"summary":"Child"}}],
 "customfield_10001":5.5,
 "customfield_10002":"short text",
 "customfield_10003":{"id":"100","value":"Red"},
 "customfield_10004":[{"id":"101","value":"A"},{"id":102,"value":"B"}],
 "customfield_10005":{"id":"200","value":"Hardware","child":{"id":"201","value":"Laptop"}},
 "customfield_10006":` + userTwoJSON + `,
 "customfield_10007":[` + userOneJSON + `,` + userTwoJSON + `],
 "customfield_10008":"2026-11-20",
 "customfield_10009":"2026-11-20T08:00:00.000+0000",
 "customfield_10010":[{"id":42,"name":"Sprint 42","state":"active","boardId":3,"goal":"Ship","startDate":"2026-11-10T09:00:00.000Z","endDate":"2026-11-24T09:00:00.000Z"}],
 "customfield_10011":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"Notes"}]}]},
 "customfield_10012":null
}}`

func TestGetIssueDecodesEveryTypedField(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("GET", "/rest/api/3/issue/WEB-1", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "fields", "*all")
		reply(200, fullIssueJSON)(w, r, b)
	})
	is, err := c.GetIssue(t.Context(), "WEB-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(is.Problems) != 0 {
		t.Fatalf("problems: %v", is.Problems)
	}
	checks := map[string]bool{
		"summary":     is.Summary == "Fix the login page",
		"status":      is.Status != nil && is.Status.Category.Key == "indeterminate",
		"type":        is.Type != nil && is.Type.Name == "Task",
		"priority":    is.Priority != nil && is.Priority.Name == "High",
		"assignee":    is.Assignee != nil && is.Assignee.AccountID == "acc-user1",
		"reporter":    is.Reporter != nil && is.Reporter.AccountID == "acc-user2",
		"parent":      is.Parent != nil && is.Parent.Key == "WEB-100" && is.Parent.Type.HierarchyLevel == 1 && is.Parent.Status.Category.Key == "new",
		"labels":      len(is.Labels) == 2,
		"components":  len(is.Components) == 1 && is.Components[0].Name == "Web",
		"fixVersions": len(is.FixVersions) == 1 && is.FixVersions[0].Released && is.FixVersions[0].ReleaseDate.String() == "2026-10-01",
		"created":     is.Created.Equal(time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)),
		"updated":     is.Updated.Equal(time.Date(2026, 11, 15, 16, 45, 12, 345e6, time.UTC)),
		"duedate":     is.DueDate == Date{2026, time.December, 1},
		"resolution":  is.Resolution == nil,
		"project":     is.Project != nil && is.Project.Key == "WEB" && !is.Project.ServiceDesk(),
		"description": adf.ToMarkdown(is.Description) == "Steps\n",
		"attachments": len(is.Attachments) == 1 && is.Attachments[0].ID == "501" && is.Attachments[0].Author.DisplayName == "User 1",
		"links": len(is.Links) == 2 && is.Links[0].Direction == Outward && is.Links[0].Issue.Key == "WEB-2" &&
			is.Links[1].Direction == Inward && is.Links[1].Issue.Key == "APP-3" && is.Links[1].Type.Inward == "is blocked by",
		"subtasks":     len(is.Subtasks) == 1 && is.Subtasks[0].Key == "WEB-4",
		"raw only":     is.Raw["summary"] == nil && is.Raw["customfield_10001"] != nil,
		"raw null too": is.Field("customfield_10012").Null(),
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s did not decode", name)
		}
	}
}

func TestCustomFieldReaders(t *testing.T) {
	var v issueJSON
	if err := jsonUnmarshal(fullIssueJSON, &v); err != nil {
		t.Fatal(err)
	}
	is := v.decode()
	if n, err := is.Field("customfield_10001").Number(); err != nil || n != 5.5 {
		t.Errorf("number: %v %v", n, err)
	}
	if s, err := is.Field("customfield_10002").Text(); err != nil || s != "short text" {
		t.Errorf("text: %v %v", s, err)
	}
	if o, err := is.Field("customfield_10003").Option(); err != nil || o.ID != "100" || o.Value != "Red" {
		t.Errorf("option: %+v %v", o, err)
	}
	if os, err := is.Field("customfield_10004").Options(); err != nil || len(os) != 2 || os[1].ID != "102" {
		t.Errorf("options: %+v %v", os, err)
	}
	if o, err := is.Field("customfield_10005").Option(); err != nil || o.Child == nil || o.Child.Value != "Laptop" {
		t.Errorf("cascading: %+v %v", o, err)
	}
	if u, err := is.Field("customfield_10006").User(); err != nil || u.AccountID != "acc-user2" {
		t.Errorf("user: %+v %v", u, err)
	}
	if us, err := is.Field("customfield_10007").Users(); err != nil || len(us) != 2 {
		t.Errorf("users: %+v %v", us, err)
	}
	if d, err := is.Field("customfield_10008").Date(); err != nil || d.String() != "2026-11-20" {
		t.Errorf("date: %v %v", d, err)
	}
	if dt, err := is.Field("customfield_10009").DateTime(); err != nil || !dt.Equal(time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("date-time: %v %v", dt, err)
	}
	ss, err := is.Field("customfield_10010").Sprints()
	if err != nil || len(ss) != 1 || ss[0].ID != 42 || ss[0].State != "active" || ss[0].Start.IsZero() || !ss[0].Done.IsZero() {
		t.Errorf("sprints: %+v %v", ss, err)
	}
	if doc, err := is.Field("customfield_10011").RichText(); err != nil || adf.ToMarkdown(doc) != "Notes\n" {
		t.Errorf("rich text: %v", err)
	}
	for _, id := range []string{"customfield_10012", "customfield_99999"} {
		if _, err := is.Field(id).Number(); !errors.Is(err, ErrNoValue) {
			t.Errorf("%s: %v, want ErrNoValue", id, err)
		}
	}
	if _, err := is.Field("customfield_10002").Number(); err == nil || errors.Is(err, ErrNoValue) {
		t.Errorf("a text read as a number: %v", err)
	}
	if _, err := is.Field("customfield_10002").RichText(); err == nil {
		t.Error("a text read as a document")
	}
}

// TestOneBadFieldKeepsTheIssue proves an unexpected shape in one typed
// field leaves it zero, keeps it raw, names it, and decodes the rest.
func TestOneBadFieldKeepsTheIssue(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("GET", "/rest/api/3/issue/WEB-1", reply(200, `{"id":"10001","key":"WEB-1","fields":{
		"summary":"Still here","status":"Done","description":{"type":"paragraph"},"created":"yesterday",
		"customfield_10001":{"anything":[1,2]}}}`))
	is, err := c.GetIssue(t.Context(), "WEB-1", "summary", "status", "description", "created", "customfield_10001")
	if err != nil {
		t.Fatal(err)
	}
	if is.Summary != "Still here" || is.Status != nil || is.Description != nil || !is.Created.IsZero() {
		t.Errorf("got %+v", is)
	}
	if len(is.Problems) != 3 || string(is.Raw["status"]) != `"Done"` {
		t.Errorf("problems %v raw %s", is.Problems, is.Raw)
	}
	if string(is.Field("customfield_10001").JSON()) != `{"anything":[1,2]}` {
		t.Error("an open-ended field lost its value")
	}
}

func TestGetIssueRefusesPathText(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	for _, ref := range []string{"", "..", "WEB-1/comment", "WEB-1?x=1", "WEB 1"} {
		if _, err := c.GetIssue(t.Context(), ref); err == nil {
			t.Errorf("%q reached a path", ref)
		}
	}
}

func TestDateJSON(t *testing.T) {
	d, err := ParseDate("2026-02-30")
	if err == nil {
		t.Errorf("an impossible date parsed: %v", d)
	}
	d, _ = ParseDate("2026-02-03")
	raw, _ := d.MarshalJSON()
	var back Date
	if string(raw) != `"2026-02-03"` || back.UnmarshalJSON(raw) != nil || back != d {
		t.Errorf("round trip: %s %v", raw, back)
	}
	raw, _ = Date{}.MarshalJSON()
	if string(raw) != "null" {
		t.Errorf("zero: %s", raw)
	}
}
