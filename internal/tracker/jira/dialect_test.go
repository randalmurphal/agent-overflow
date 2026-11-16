package jira

import (
	"errors"
	"maps"
	"net/http"
	"testing"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

func TestDialectProblemShapes(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		msg    string
		fields map[string]string
	}{
		"messages and field errors": {
			body:   `{"errorMessages":["Issue does not exist"],"errors":{"summary":"Summary is required","customfield_10001":"Bad number"}}`,
			msg:    "Issue does not exist",
			fields: map[string]string{"summary": "Summary is required", "customfield_10001": "Bad number"},
		},
		"field errors only":       {body: `{"errorMessages":[],"errors":{"priority":"Priority is invalid"}}`, fields: map[string]string{"priority": "Priority is invalid"}},
		"several messages":        {body: `{"errorMessages":["one","two"],"errors":{}}`, msg: "one; two"},
		"errors as a list":        {body: `{"errors":[{"message":"first"},{"title":"second"},"third"]}`, msg: "first; second; third"},
		"message key":             {body: `{"message":"Rate limited"}`, msg: "Rate limited"},
		"errorMessage key":        {body: `{"errorMessage":"Something broke"}`, msg: "Something broke"},
		"a non-string field note": {body: `{"errors":{"labels":{"code":7}}}`, fields: map[string]string{"labels": `{"code":7}`}},
		"not JSON":                {body: `<html>oops</html>`},
		"empty":                   {body: ``},
	} {
		t.Run(name, func(t *testing.T) {
			p := dialect{}.Problem(400, []byte(tc.body))
			if p.Message != tc.msg || !maps.Equal(p.Fields, tc.fields) {
				t.Errorf("got %q %v, want %q %v", p.Message, p.Fields, tc.msg, tc.fields)
			}
		})
	}
}

func TestDialectRejectedLogin(t *testing.T) {
	for value, want := range map[string]bool{
		"AUTHENTICATED_FAILED":      true,
		"AUTHENTICATION_DENIED":     true,
		"OK, AUTHENTICATION_DENIED": true,
		"OK":                        false,
		"AUTHORISATION_FAILED":      false,
		"":                          false,
	} {
		h := http.Header{}
		if value != "" {
			h.Set("X-Seraph-LoginReason", value)
		}
		if got := (dialect{}).RejectedLogin(h); got != want {
			t.Errorf("%q: got %v, want %v", value, got, want)
		}
	}
}

func TestDialectRateLimited(t *testing.T) {
	reset := "2026-11-16T12:00:05Z"
	h := http.Header{"X-Ratelimit-Reset": {reset}}
	until, limited := dialect{}.RateLimited(429, h, nil)
	if want, _ := time.Parse(time.RFC3339, reset); !limited || !until.Equal(want) {
		t.Errorf("reset header: %v %v", until, limited)
	}
	h.Set("Retry-After", "3")
	if until, limited := (dialect{}).RateLimited(429, h, nil); !limited || !until.IsZero() {
		t.Errorf("Retry-After must win, through the transport: %v %v", until, limited)
	}
	if _, limited := (dialect{}).RateLimited(503, h, nil); limited {
		t.Error("a 503 is not a rate limit")
	}
}

// TestDialectThroughTransport proves the dialect reaches callers: field
// errors on a StatusError, and a refused login marked on a 200.
func TestDialectThroughTransport(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("PUT", "/rest/api/3/issue/WEB-1/assignee", reply(400, `{"errorMessages":[],"errors":{"assignee":"User cannot be assigned"}}`))
	err := c.Assign(t.Context(), "WEB-1", "acc-user1")
	se, ok := errors.AsType[*trackerapi.StatusError](err)
	if !ok || se.Fields["assignee"] != "User cannot be assigned" {
		t.Fatalf("got %v", err)
	}
	if !trackerapi.WriteNotApplied(err) {
		t.Error("a 400 is a refusal")
	}
	f.on("GET", "/rest/api/3/myself", func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("X-Seraph-LoginReason", "AUTHENTICATED_FAILED")
		_, _ = w.Write([]byte(`{}`))
	})
	if _, err := c.Myself(t.Context()); !isAuth(err) {
		t.Errorf("a refused login on a 200: %v", err)
	}
}

func isAuth(err error) bool {
	_, ok := errors.AsType[*trackerapi.AuthError](err)
	return ok
}
