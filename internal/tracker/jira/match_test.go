package jira

import (
	"errors"
	"net/http"
	"slices"
	"testing"

	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

const matchURL = "/rest/api/3/jql/match"

// TestMatchJQL: the request carries the IDs as numbers and the queries in
// order, each query's answer keeps its matched IDs and its errors, and
// an answer that does not hold one entry per query or names an issue not
// asked is an error.
func TestMatchJQL(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("POST", matchURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantJSON(t, b, `{"issueIds":[10001,10002],"jqls":["filter = 1","filter = 2"]}`)
		jiratest.Reply(200, `{"matches":[{"matchedIssues":[10002],"errors":[]},{"matchedIssues":[],"errors":["The value '2' does not exist for the field 'filter'."]}]}`)(w, r, b)
	})
	got, err := c.MatchJQL(t.Context(), []string{"10001", "10002"}, []string{"filter = 1", "filter = 2"})
	if err != nil || len(got) != 2 || !slices.Equal(got[0].Matched, []string{"10002"}) || len(got[0].Errors) != 0 ||
		len(got[1].Matched) != 0 || len(got[1].Errors) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	f.On("POST", matchURL, jiratest.Reply(200, `{"matches":[{"matchedIssues":[10001],"errors":[]}]}`))
	if _, err := c.MatchJQL(t.Context(), []string{"10001"}, []string{"a = 1", "b = 2"}); err == nil {
		t.Error("an answer short of a query was accepted")
	}
	f.On("POST", matchURL, jiratest.Reply(200, `{"matches":[{"matchedIssues":[10009],"errors":[]}]}`))
	if _, err := c.MatchJQL(t.Context(), []string{"10001"}, []string{"a = 1"}); err == nil {
		t.Error("an issue not asked was accepted")
	}
}

// TestMatchJQLRefusesBeforeSending: bounds, IDs that are not numeric and
// empty queries are refused as the caller's mistake, with no request.
func TestMatchJQLRefusesBeforeSending(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	many := make([]string, MaxMatchIssues+1)
	for i := range many {
		many[i] = "1"
	}
	queries := make([]string, MaxMatchQueries+1)
	for i := range queries {
		queries[i] = "a = 1"
	}
	for name, call := range map[string]func() error{
		"no issue":     func() error { _, err := c.MatchJQL(t.Context(), nil, []string{"a = 1"}); return err },
		"too many":     func() error { _, err := c.MatchJQL(t.Context(), many, []string{"a = 1"}); return err },
		"no query":     func() error { _, err := c.MatchJQL(t.Context(), []string{"1"}, nil); return err },
		"many queries": func() error { _, err := c.MatchJQL(t.Context(), []string{"1"}, queries); return err },
		"a key":        func() error { _, err := c.MatchJQL(t.Context(), []string{"WEB-1"}, []string{"a = 1"}); return err },
		"an empty JQL": func() error { _, err := c.MatchJQL(t.Context(), []string{"1"}, []string{""}); return err },
		"a signed ID":  func() error { _, err := c.MatchJQL(t.Context(), []string{"+1"}, []string{"a = 1"}); return err },
		"a zero ID":    func() error { _, err := c.MatchJQL(t.Context(), []string{"0"}, []string{"a = 1"}); return err },
		"an ID too big": func() error {
			_, err := c.MatchJQL(t.Context(), []string{"99999999999999999999"}, []string{"a = 1"})
			return err
		},
	} {
		if err := call(); !errors.Is(err, trackerapi.ErrInvalidInput) {
			t.Errorf("%s: %v, want ErrInvalidInput", name, err)
		}
	}
	if n := f.Count("POST", matchURL); n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

// TestMatchJQLProvesAnEmptyAnswer: an answer that matched less than was
// asked and names no account is checked with a sign-in read, and a
// refused credential fails the match; one that names the account is
// taken as it is.
func TestMatchJQLProvesAnEmptyAnswer(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("POST", matchURL, jiratest.Reply(200, `{"matches":[{"matchedIssues":[],"errors":[]}]}`))
	if got, err := c.MatchJQL(t.Context(), []string{"10001"}, []string{"a = 1"}); err != nil || len(got[0].Matched) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if n := f.Count("GET", "/rest/api/3/myself"); n != 0 {
		t.Fatalf("%d sign-in checks for an answer that named its account", n)
	}
	f.SetAccount("")
	f.On("GET", "/rest/api/3/myself", jiratest.Reply(401, `{}`))
	_, err := c.MatchJQL(t.Context(), []string{"10001"}, []string{"a = 1"})
	if _, ok := errors.AsType[*trackerapi.AuthError](err); !ok {
		t.Fatalf("an anonymous empty match: %v, want an AuthError", err)
	}
}

// TestMatchJQLRefusedIsTheMatchsOwn400: a 400 to the match is a
// *QueryRefusedError holding that answer; an anonymous empty match whose
// sign-in check answers 400 is an *UnprovenError and no refusal of the
// query, though the check's 400 is in its chain.
func TestMatchJQLRefusedIsTheMatchsOwn400(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("POST", matchURL, jiratest.Reply(400, `{"errorMessages":["Bad request"],"errors":{}}`))
	_, err := c.MatchJQL(t.Context(), []string{"10001"}, []string{"a = 1"})
	if refused, ok := errors.AsType[*QueryRefusedError](err); !ok || refused.Err.Status != http.StatusBadRequest {
		t.Fatalf("a refused match: %v, want a QueryRefusedError of the 400", err)
	}
	f.On("POST", matchURL, jiratest.Reply(200, `{"matches":[{"matchedIssues":[],"errors":[]}]}`))
	f.SetAccount("")
	f.On("GET", "/rest/api/3/myself", jiratest.Reply(400, `{"errorMessages":["Bad request"],"errors":{}}`))
	_, err = c.MatchJQL(t.Context(), []string{"10001"}, []string{"a = 1"})
	_, unproven := errors.AsType[*UnprovenError](err)
	_, refused := errors.AsType[*QueryRefusedError](err)
	status, ok := errors.AsType[*trackerapi.StatusError](err)
	if !unproven || refused || !ok || status.Status != http.StatusBadRequest {
		t.Fatalf("a match whose sign-in check answered 400: %v; want an UnprovenError holding the check's 400 and no QueryRefusedError", err)
	}
	if n := f.Count("GET", "/rest/api/3/myself"); n != 1 {
		t.Fatalf("%d sign-in checks; want 1", n)
	}
}
