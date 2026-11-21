package jira

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/tracker/jira/jiratest"
)

// TestFieldsNamingFindsAFieldAddedLater: a catalog read before a field
// was added is read again once when a read names the field, and costs
// nothing when a read names only fields it holds; an ID no field has
// costs that one read, and naming it again costs none.
func TestFieldsNamingFindsAFieldAddedLater(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	added := `,{"id":"customfield_10100","name":"Impact","custom":true,"schema":{"type":"number","custom":"com.atlassian.jira.plugin.system.customfieldtypes:float","customId":10100}}]`
	var later atomic.Bool
	f.On("GET", "/rest/api/3/field", func(w http.ResponseWriter, r *http.Request, b []byte) {
		body := catalogJSON
		if later.Load() {
			body = strings.TrimSuffix(strings.TrimSpace(catalogJSON), "]") + added
		}
		jiratest.Reply(200, body)(w, r, b)
	})
	reads := func() int { return f.Count("GET", "/rest/api/3/field") }
	if _, err := c.Fields(t.Context()); err != nil {
		t.Fatal(err)
	}
	later.Store(true)
	if _, err := c.FieldsNaming(t.Context(), "summary", "customfield_10001"); err != nil || reads() != 1 {
		t.Fatalf("naming held fields: %d reads, %v; want the cached catalog", reads(), err)
	}
	cat, err := c.FieldsNaming(t.Context(), "summary", "customfield_10100")
	if _, ok := cat.Field("customfield_10100"); err != nil || !ok || reads() != 2 {
		t.Fatalf("naming the added field: found %v, %d reads, %v; want it read once", ok, reads(), err)
	}
	if cat, _ := c.Fields(t.Context()); cat.Len() != 3 || reads() != 2 {
		t.Fatalf("the catalog read again is not the one cached: %d fields, %d reads", cat.Len(), reads())
	}
	for range 2 {
		if _, err := c.FieldsNaming(t.Context(), "customfield_10999"); err != nil {
			t.Fatal(err)
		}
	}
	if reads() != 3 {
		t.Fatalf("naming a field nobody has twice: %d reads, want 3", reads())
	}
	// The ID found missing stays known as missing across the next read.
	if _, err := c.FieldsNaming(t.Context(), "customfield_10888"); err != nil || reads() != 4 {
		t.Fatal(reads(), err)
	}
	if _, err := c.FieldsNaming(t.Context(), "customfield_10999", "customfield_10888"); err != nil || reads() != 4 {
		t.Fatalf("naming fields found missing: %d reads, %v; want none", reads(), err)
	}
}
