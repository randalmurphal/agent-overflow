package jira

import (
	"net/http"
	"testing"

	"agent-overflow/internal/tracker/jira/jiratest"
)

// Each name's match, a person or none, is kept: a name costs one search
// however often it is asked.
func TestUserNamedKeepsEachNamesMatch(t *testing.T) {
	t.Parallel()
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	searches := 0
	f.On("GET", "/rest/api/3/user/search", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if r.URL.Query().Get("startAt") != "0" {
			jiratest.Reply(200, `[]`)(w, r, b)
			return
		}
		searches++
		jiratest.Reply(200, `[{"accountId":"acc-1","displayName":"Pat Lee","active":true,"accountType":"atlassian"}]`)(w, r, b)
	})
	for _, name := range []string{"Pat Lee", "pat lee", "Pat Lee", "Nobody", "nobody"} {
		u, ok, err := c.UserNamed(t.Context(), name)
		if err != nil || ok != (name != "Nobody" && name != "nobody") || ok && u.AccountID != "acc-1" {
			t.Fatalf("%s = %+v, %v, %v", name, u, ok, err)
		}
	}
	if searches != 2 {
		t.Fatalf("%d searches; want one for each name, case ignored", searches)
	}
}
