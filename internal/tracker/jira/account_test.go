package jira

import (
	"errors"
	"testing"

	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

// accountSite answers a search with issues (or none), a bulk fetch and
// a bulk changelog that return WEB-1 only, and the sign-in check with
// myself's status.
func accountSite(t *testing.T, found bool, myself int) (*jiratest.Server, *Client) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	issues := `[]`
	if found {
		issues = `[{"id":"10001","key":"WEB-1"}]`
	}
	f.On("POST", "/rest/api/3/search/jql", jiratest.Reply(200, `{"issues":`+issues+`,"isLast":true}`))
	f.On("POST", "/rest/api/3/issue/bulkfetch", jiratest.Reply(200, `{"issues":[{"id":"10001","key":"WEB-1"}],"issueErrors":[]}`))
	f.On("POST", bulkChangelogURL, jiratest.Reply(200, `{"issueChangeLogs":[{"issueId":"10001","changeHistories":[`+changeJSON(1)+`]}]}`))
	body := userOneJSON
	if myself != 200 {
		body = `{"errorMessages":["Client must be authenticated to access this resource."]}`
	}
	f.On("GET", myselfURL, jiratest.Reply(myself, body))
	return f, c
}

// TestAnswersThatLeaveOutAreProven proves an answer that leaves out what
// was asked (an empty search, a bulk fetch or changelog missing issues)
// is taken as the user's with no request when it names the account it
// was made for, and only after a sign-in check when it names none, since
// Jira answers a search with no credential 200 with no issues (measured).
// An answer that left nothing out needs no check either way.
func TestAnswersThatLeaveOutAreProven(t *testing.T) {
	reads := map[string]func(c *Client) error{
		"an empty search": func(c *Client) error {
			_, err := c.Search(t.Context(), SearchQuery{JQL: `key in ("WEB-9")`})
			return err
		},
		"a bulk fetch missing an issue": func(c *Client) error {
			_, err := c.BulkFetch(t.Context(), []string{"10001", "10009"})
			return err
		},
		"a bulk changelog missing an issue": func(c *Client) error {
			_, err := c.BulkChangelogs(t.Context(), ChangelogQuery{Issues: []string{"10001", "10009"}})
			return err
		},
	}
	for what, read := range reads {
		for _, named := range []bool{true, false} {
			f, c := accountSite(t, false, 200)
			if !named {
				f.SetAccount("")
			}
			if err := read(c); err != nil {
				t.Fatalf("%s, account named %v: %v", what, named, err)
			}
			if n, want := f.Count("GET", myselfURL), map[bool]int{true: 0, false: 1}[named]; n != want {
				t.Errorf("%s, account named %v: %d sign-in checks, want %d", what, named, n, want)
			}
		}
	}
	// Nothing left out: no check, account named or not.
	f, c := accountSite(t, true, 200)
	f.SetAccount("")
	if _, err := c.Search(t.Context(), SearchQuery{JQL: `key in ("WEB-1")`}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.BulkFetch(t.Context(), []string{"10001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.BulkChangelogs(t.Context(), ChangelogQuery{Issues: []string{"10001"}}); err != nil {
		t.Fatal(err)
	}
	if n := f.Count("GET", myselfURL); n != 0 {
		t.Errorf("%d sign-in checks for answers that left nothing out", n)
	}
}

// TestUnprovenAnswersFail proves an answer that names no account and left
// out what was asked is not taken as missing when the sign-in check
// refuses the credential (an error holding the AuthError) or fails (an
// *UnprovenError, which nothing reads as not found).
func TestUnprovenAnswersFail(t *testing.T) {
	for _, tc := range []struct {
		myself  int
		refused bool
	}{{401, true}, {403, false}} {
		f, c := accountSite(t, false, tc.myself)
		f.SetAccount("")
		_, err := c.Search(t.Context(), SearchQuery{JQL: `key in ("WEB-9")`})
		_, unproven := errors.AsType[*UnprovenError](err)
		_, auth := errors.AsType[*trackerapi.AuthError](err)
		if err == nil || auth != tc.refused || unproven == tc.refused || errors.Is(err, trackerapi.ErrNotFound) {
			t.Errorf("sign-in check %d: %v", tc.myself, err)
		}
		if _, err := c.BulkFetch(t.Context(), []string{"10001", "10009"}); err == nil {
			t.Errorf("sign-in check %d: a bulk fetch missing an issue read as complete", tc.myself)
		}
	}
}

// TestNamedNotFoundIsNotChecked proves a 404 that names the account it
// was made for is the user's answer with nothing more sent, for a read
// and for a write, and that one naming no account gets its sign-in
// check (measured: a valid token's 404 names the account, a 404 to no
// credential does not).
func TestNamedNotFoundIsNotChecked(t *testing.T) {
	for _, named := range []bool{true, false} {
		f := jiratest.NewServer(t)
		c := newTestClient(t, f)
		if !named {
			f.SetAccount("")
		}
		f.On("GET", issuePath404, jiratest.Reply(404, `{"errorMessages":["Issue does not exist or you do not have permission to see it."]}`))
		f.On("DELETE", "/rest/api/3/issueLink/77", jiratest.Reply(404, `{}`))
		f.On("GET", myselfURL, jiratest.Reply(200, userOneJSON))
		_, err := c.GetIssue(t.Context(), "WEB-404")
		if !errors.Is(err, trackerapi.ErrNotFound) || isAuth(err) {
			t.Fatalf("account named %v: read: %v, want not found", named, err)
		}
		err = c.UnlinkIssues(t.Context(), "77")
		if !errors.Is(err, trackerapi.ErrNotFound) || !trackerapi.WriteNotApplied(err) {
			t.Fatalf("account named %v: write: %v, want not found and not applied", named, err)
		}
		if n, want := f.Count("GET", myselfURL), map[bool]int{true: 0, false: 2}[named]; n != want {
			t.Errorf("account named %v: %d sign-in checks for two 404s, want %d", named, n, want)
		}
		if n := f.Count("GET", issuePath404); n != 1 {
			t.Errorf("account named %v: the issue was read %d times, want 1", named, n)
		}
	}
}
