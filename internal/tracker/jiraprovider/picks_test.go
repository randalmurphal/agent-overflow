package jiraprovider

import (
	"net/http"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// pickSite is newSite with issue 10001's edit screen offering Severity
// with one option, ID 22, named "20", and Story points: an option picked
// as ID 20 from an older list is gone.
func pickSite(t *testing.T) (*jiratest.Server, *Provider) {
	t.Helper()
	f, p := newSite(t)
	f.On("GET", issuePath, jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"summary":"S"},"editmeta":{"fields":{
"customfield_10050":{"key":"customfield_10050","name":"Severity","schema":{"type":"option","custom":"com.atlassian.jira.plugin.system.customfieldtypes:select","customId":10050},"operations":["set"],"allowedValues":[{"id":"22","value":"20"}]},
"customfield_10016":`+metaFields["customfield_10016"]+`}}}`))
	return f, p
}

// TestAPickedIDIsNeverReadAsAName: a picked option ID matches an ID
// exactly or is refused as stale, before anything is sent, even when
// another option is named by it; typed text still finds the option by
// name.
func TestAPickedIDIsNeverReadAsAName(t *testing.T) {
	f, p := pickSite(t)
	edit := func(v tracker.Edit) tracker.Result {
		return p.Edit(t.Context(), tracker.EditInput{Issue: "10001", Fields: []tracker.FieldEdit{{Field: "customfield_10050", Value: v}}})
	}
	stale := edit(tracker.PickValues("20"))
	if states(stale) != "edit:refused" || reason(stale, 0) != tracker.ReasonStale {
		t.Fatalf("picked ID 20: %s, %v; want refused as stale", states(stale), stale.Parts[0].Outcome.Err())
	}
	if ref, _ := stale.Parts[0].Outcome.Refusal(); len(ref.Candidates) != 1 || ref.Candidates[0].ID != "22" {
		t.Errorf("the stale pick offers %+v, want option 22", ref.Candidates)
	}
	var sent []string
	f.On("PUT", issuePath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		sent = append(sent, string(b))
		w.WriteHeader(http.StatusNoContent)
	})
	for _, v := range []tracker.Edit{tracker.PickValues("22"), tracker.TypedValues("20")} {
		if res := checked(t, edit(v)); states(res) != "edit:applied" {
			t.Fatalf("%v: %s, %v", v.Values(), states(res), res.Parts[0].Outcome.Err())
		}
	}
	for _, b := range sent {
		jiratest.WantJSON(t, []byte(b), `{"fields":{"customfield_10050":{"id":"22"}}}`)
	}
	if len(sent) != 2 {
		t.Fatalf("%d edits sent, want 2", len(sent))
	}
	// A number has no IDs to pick.
	res := p.Edit(t.Context(), tracker.EditInput{Issue: "10001", Fields: []tracker.FieldEdit{{Field: "customfield_10016", Value: tracker.PickValues("5")}}})
	if states(res) != "edit:refused" || reason(res, 0) != tracker.ReasonInvalid {
		t.Fatalf("a picked number: %s, %v", states(res), res.Parts[0].Outcome.Err())
	}
}
