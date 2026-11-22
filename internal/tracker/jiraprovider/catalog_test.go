package jiraprovider

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// impactJSON is a custom field added on the site after the provider read
// its catalog.
const impactJSON = `{"id":"customfield_10100","name":"Impact","schema":{"type":"number","custom":"com.atlassian.jira.plugin.system.customfieldtypes:float","customId":10100}}`

// addedFieldSite is newSite whose catalog gains Impact once added is set,
// with issue 10001's edit screen offering it and a detail read holding a
// value of it.
func addedFieldSite(t *testing.T, added *atomic.Bool) (*jiratest.Server, *Provider) {
	t.Helper()
	f, p := newSite(t)
	f.On("GET", fieldPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		body := fieldsJSON
		if added.Load() {
			body = strings.TrimSuffix(strings.TrimSpace(fieldsJSON), "]") + "," + impactJSON + "]"
		}
		jiratest.Reply(200, body)(w, r, b)
	})
	meta := `"customfield_10100":{"key":"customfield_10100","name":"Impact","schema":{"type":"number","custom":"com.atlassian.jira.plugin.system.customfieldtypes:float","customId":10100},"operations":["set"]}`
	f.On("GET", issuePath, jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"summary":"S"},"editmeta":{"fields":{`+meta+`}}}`))
	return f, p
}

// TestAFieldAddedLaterIsEdited: an edit of a field added after the
// catalog was read reads the catalog again once and is sent, where it
// was refused as naming no field; a second edit of it reads nothing more.
func TestAFieldAddedLaterIsEdited(t *testing.T) {
	var added atomic.Bool
	f, p := addedFieldSite(t, &added)
	if _, err := p.jira.Fields(t.Context()); err != nil {
		t.Fatal(err)
	}
	added.Store(true)
	var sent atomic.Int32
	f.On("PUT", issuePath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"fields":{"customfield_10100":5}}`)
		sent.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	for range 2 {
		res := p.Edit(t.Context(), tracker.EditInput{Issue: "10001", Fields: []tracker.FieldEdit{{Field: "customfield_10100", Value: tracker.TypedValues("5")}}})
		if states(res) != "edit:applied" {
			t.Fatalf("edit of the added field: %s, %v", states(res), res.Parts[0].Outcome.Err())
		}
	}
	if n := f.Count("GET", fieldPath); n != 2 || sent.Load() != 2 {
		t.Fatalf("%d catalog reads and %d edits, want 2 and 2", n, sent.Load())
	}
}
