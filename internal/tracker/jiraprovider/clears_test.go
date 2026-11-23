package jiraprovider

import (
	"net/http"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// clearEdits clear a rich text field, an option, a list and the parent.
var clearEdits = []tracker.FieldEdit{
	{Field: "description", Value: tracker.ClearField()},
	{Field: "priority", Value: tracker.ClearField()},
	{Field: "labels", Value: tracker.ClearField()},
	{Field: "parent", Value: tracker.ClearField()},
}

// TestCreateSendsClears proves a field a create clears reaches Jira as
// its empty value, never left out, where Jira could fill a default, and
// that a clear of a required field is refused before sending.
func TestCreateSendsClears(t *testing.T) {
	f, p := newSite(t)
	f.On("GET", "/rest/api/3/issue/createmeta/10000/issuetypes", jiratest.Reply(200, `{"issueTypes":[{"id":"10002","name":"Story"}],"total":1}`))
	f.On("GET", createMetaPath, jiratest.Reply(200, `{"total":5,"fields":[`+
		`{"fieldId":"parent",`+parentMeta[1:]+`,`+createMeta(false, "summary", "description", "priority", "labels")[len(`{"total":4,"fields":[`):]))
	f.On("POST", createPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"fields":{"project":{"id":"10000"},"issuetype":{"id":"10002"},"summary":"New",
			"description":null,"priority":null,"labels":[]},"update":{"parent":[{"set":{"none":true}}]}}`)
		jiratest.Reply(201, `{"id":"10001","key":"WEB-1"}`)(w, r, b)
	})
	res := checked(t, p.Create(t.Context(), tracker.CreateInput{Project: "10000", Type: "10002", Title: "New", Fields: clearEdits}))
	if states(res) != "create:applied" {
		t.Fatalf("got %s: %v", states(res), res.Parts[0].Outcome.Err())
	}

	f, p = createSite(t, true)
	res = checked(t, p.Create(t.Context(), tracker.CreateInput{Project: "10000", Type: "10002", Title: "New", Fields: []tracker.FieldEdit{
		{Field: "customfield_10050", Value: tracker.ClearField()},
	}}))
	if states(res) != "create:refused" || reason(res, 0) != tracker.ReasonInvalid || f.Count("POST", createPath) != 0 {
		t.Errorf("a required field cleared: %s %v", states(res), res.Parts[0].Outcome.Err())
	}
}

// TestMoveSendsClears proves the same of the fields of a move's screen.
func TestMoveSendsClears(t *testing.T) {
	f, p := editSite(t, storedDesc(adf.Doc(para("old"))))
	f.On("GET", "/rest/api/3/issue/10001/transitions", jiratest.Reply(200, `{"transitions":[
		{"id":"31","name":"Resolve","to":{"id":"10002","name":"Done","statusCategory":{"key":"done"}},"hasScreen":true,"isAvailable":true,"fields":{
		 "description":`+metaFields["description"]+`,"priority":`+metaFields["priority"]+`,"labels":`+metaFields["labels"]+`,"parent":`+parentMeta+`,
		 "resolution":{"key":"resolution","name":"Resolution","required":true,"schema":{"type":"resolution","system":"resolution"},"operations":["set"],"allowedValues":[{"id":"1","name":"Fixed"}]}}}]}`))
	f.On("POST", "/rest/api/3/issue/10001/transitions", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"transition":{"id":"31"},"fields":{"description":null,"priority":null,"labels":[]},"update":{"parent":[{"set":{"none":true}}]}}`)
		w.WriteHeader(http.StatusNoContent)
	})
	res := checked(t, p.Move(t.Context(), tracker.MoveInput{Issue: "10001", Move: "31", Fields: clearEdits}))
	if states(res) != "move:refused" || reason(res, 0) != tracker.ReasonMissing {
		t.Errorf("a move without its required resolution: %s", states(res))
	}
	res = checked(t, p.Move(t.Context(), tracker.MoveInput{Issue: "10001", Move: "31", Fields: []tracker.FieldEdit{{Field: "resolution", Value: tracker.ClearField()}}}))
	if states(res) != "move:refused" || reason(res, 0) != tracker.ReasonInvalid {
		t.Errorf("a required field cleared: %s", states(res))
	}
	if n := f.Count("POST", "/rest/api/3/issue/10001/transitions"); n != 0 {
		t.Fatalf("%d moves sent", n)
	}
	f.On("GET", "/rest/api/3/issue/10001/transitions", jiratest.Reply(200, `{"transitions":[
		{"id":"31","name":"Resolve","to":{"id":"10002","name":"Done","statusCategory":{"key":"done"}},"hasScreen":true,"isAvailable":true,"fields":{
		 "description":`+metaFields["description"]+`,"priority":`+metaFields["priority"]+`,"labels":`+metaFields["labels"]+`,"parent":`+parentMeta+`}}]}`))
	res = checked(t, p.Move(t.Context(), tracker.MoveInput{Issue: "10001", Move: "31", Fields: clearEdits}))
	if states(res) != "move:applied" {
		t.Errorf("got %s: %v", states(res), res.Parts[0].Outcome.Err())
	}
}
