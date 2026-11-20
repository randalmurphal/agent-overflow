package jiraprovider

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// usersJSON is n invented users, acc-u1 onward.
func usersJSON(n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(`{"accountId":"acc-u%d","displayName":"User %d","active":true,"accountType":"atlassian"}`, i+1, i+1)
	}
	return "[" + strings.Join(out, ",") + "]"
}

// choicesSite is newSite with issue 10001's edit screen: Severity with
// options options, the assignee, Reviewers and the parent.
func choicesSite(t *testing.T, options int) *jiratest.Server {
	f, _ := newSite(t)
	opts := make([]string, options)
	for i := range opts {
		opts[i] = fmt.Sprintf(`{"id":"%d","value":"Minor %d"}`, 100+i, i)
	}
	f.On("GET", editMetaPath, jiratest.Reply(200, `{"fields":{
		"customfield_10050":{"key":"customfield_10050","name":"Severity","schema":{"type":"option","custom":"com.atlassian.jira.plugin.system.customfieldtypes:select","customId":10050},"operations":["set"],"allowedValues":[`+strings.Join(opts, ",")+`]},
		"priority":{"key":"priority","name":"Priority","schema":{"type":"priority","system":"priority"},"operations":["set"]},
		"assignee":{"key":"assignee","name":"Assignee","schema":{"type":"user","system":"assignee"},"operations":["set"]},
		"customfield_10060":{"key":"customfield_10060","name":"Reviewers","schema":{"type":"array","items":"user","custom":"com.atlassian.jira.plugin.system.customfieldtypes:multiuserpicker","customId":10060},"operations":["set"]},
		"parent":`+parentMeta+`}}`))
	return f
}

// TestChoicesMore proves each pick list returns at most pickLimit rows and
// sets More exactly when it may leave a match out: allowed values and the
// site's priorities past pickLimit, a parent search with a further page,
// a full page or a warning, a user search with a match past pickLimit,
// and an assignable search returning more than pickLimit.
func TestChoicesMore(t *testing.T) {
	issue := tracker.OnIssue("10001")
	for _, tc := range []struct {
		name  string
		field tracker.FieldID
		route func(t *testing.T, f *jiratest.Server)
		rows  int
		more  bool
	}{
		{name: "allowed values, whole", field: "customfield_10050", rows: pickLimit},
		{name: "allowed values past the limit", field: "customfield_10050", rows: pickLimit, more: true},
		{name: "priorities, whole", field: "priority", rows: 2, route: priorities(2)},
		{name: "priorities past the limit", field: "priority", rows: pickLimit, more: true, route: priorities(pickLimit + 1)},
		{name: "parents, short page", field: tracker.FieldParent, rows: 3, route: parents(3, "", "")},
		{name: "parents, a further page", field: tracker.FieldParent, rows: 3, more: true, route: parents(3, "next", "")},
		{name: "parents, a full page", field: tracker.FieldParent, rows: pickLimit, more: true, route: parents(pickLimit, "", "")},
		{name: "parents, a warning", field: tracker.FieldParent, rows: 3, more: true, route: parents(3, "", "partial")},
		{name: "users, whole", field: "customfield_10060", rows: pickLimit, route: userSearch(pickLimit)},
		{name: "users past the limit", field: "customfield_10060", rows: pickLimit, more: true, route: userSearch(pickLimit + 1)},
		{name: "assignable, whole", field: "assignee", rows: pickLimit, route: assignable(pickLimit)},
		{name: "assignable past the limit", field: "assignee", rows: pickLimit, more: true, route: assignable(pickLimit + 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := pickLimit
			if tc.more {
				options = pickLimit + 1
			}
			f := choicesSite(t, options)
			if tc.route != nil {
				tc.route(t, f)
			}
			got, err := newProvider(t, f, nil).Choices(t.Context(), issue, tc.field, "m")
			if err != nil || len(got.Choices) != tc.rows || got.More != tc.more {
				t.Errorf("got %d rows, more %v, %v; want %d, %v", len(got.Choices), got.More, err, tc.rows, tc.more)
			}
		})
	}
}

// TestUserChoicesNoText proves a user field asked with no text searches
// nothing and says the list leaves users out.
func TestUserChoicesNoText(t *testing.T) {
	f := choicesSite(t, 1)
	got, err := newProvider(t, f, nil).Choices(t.Context(), tracker.OnIssue("10001"), "customfield_10060", " ")
	if err != nil || len(got.Choices) != 0 || !got.More {
		t.Errorf("got %+v %v", got, err)
	}
}

// TestAssignableUsersMore proves AssignableUsers asks for the whole window
// Jira allows, returns at most pickLimit users, and sets More only when
// more came back.
func TestAssignableUsersMore(t *testing.T) {
	for _, n := range []int{pickLimit, pickLimit + 1} {
		f, p := newSite(t)
		assignable(n)(t, f)
		got, err := p.AssignableUsers(t.Context(), tracker.OnCreate("10000", "10002"), "user")
		if err != nil || len(got.Users) != pickLimit || got.More != (n > pickLimit) {
			t.Errorf("%d found: %d users, more %v, %v", n, len(got.Users), got.More, err)
		}
	}
}

// priorities answers the site's priorities: n named "Medium 0" onward.
func priorities(n int) func(*testing.T, *jiratest.Server) {
	return func(_ *testing.T, f *jiratest.Server) {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf(`{"id":"%d","name":"Medium %d"}`, i+1, i)
		}
		f.On("GET", "/rest/api/3/priority/search", jiratest.Reply(200, `{"isLast":true,"values":[`+strings.Join(out, ",")+`]}`))
	}
}

// parents answers the parent search with n issues, a next page token
// when next is set and a warning when warn is.
func parents(n int, next, warn string) func(*testing.T, *jiratest.Server) {
	return func(_ *testing.T, f *jiratest.Server) {
		f.On("POST", searchPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
			out := make([]string, n)
			for i := range out {
				out[i] = fmt.Sprintf(`{"id":"%d","key":"WEB-%d","fields":{"summary":"Epic"}}`, 10100+i, 100+i)
			}
			body := `{"isLast":` + strconv.FormatBool(next == "") + `,"issues":[` + strings.Join(out, ",") + `]`
			if next != "" {
				body += `,"nextPageToken":"` + next + `"`
			}
			if warn != "" {
				body += `,"warnings":[{"type":"x","message":"` + warn + `"}]`
			}
			jiratest.Reply(200, body+"}")(w, r, b)
		})
	}
}

// userSearch answers the user search with n users, checking that it asks
// for one more than pickLimit.
func userSearch(n int) func(*testing.T, *jiratest.Server) {
	return func(t *testing.T, f *jiratest.Server) {
		f.On("GET", "/rest/api/3/user/search", func(w http.ResponseWriter, r *http.Request, b []byte) {
			if q := r.URL.Query(); q.Get("maxResults") != strconv.Itoa(pickLimit+1) || q.Get("startAt") != "0" {
				t.Errorf("user search %s", r.URL.RawQuery)
			}
			jiratest.Reply(200, usersJSON(n))(w, r, b)
		})
	}
}

// assignable answers the assignable search with n users, checking that it
// asks for the whole window.
func assignable(n int) func(*testing.T, *jiratest.Server) {
	return func(t *testing.T, f *jiratest.Server) {
		f.On("GET", "/rest/api/3/user/assignable/search", func(w http.ResponseWriter, r *http.Request, b []byte) {
			if q := r.URL.Query(); q.Get("maxResults") != strconv.Itoa(assignableWindow) || q.Get("startAt") != "0" {
				t.Errorf("assignable search %s", r.URL.RawQuery)
			}
			jiratest.Reply(200, usersJSON(n))(w, r, b)
		})
	}
}
