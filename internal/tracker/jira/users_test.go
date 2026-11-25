package jira

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

// P3: a user lookup Jira refused or lacked is asked again on the next
// call under the same credential, so a permission granted, or a user
// made, shows at once to every caller of User (a worklog's author, among
// them).
func TestUserAsksAgainAfterAWithheldAnswer(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := jiratest.NewServer(t)
			c := newTestClient(t, f)
			withheld := false
			f.On("GET", "/rest/api/3/user", func(w http.ResponseWriter, r *http.Request, b []byte) {
				if withheld && r.URL.Query().Get("accountId") == "acc-user2" {
					w.Header().Set("X-AAccountId", "acc-user1")
					jiratest.Reply(status, `{"errorMessages":["no"],"errors":{}}`)(w, r, b)
					return
				}
				jiratest.Reply(200, strings.ReplaceAll(userOneJSON, "acc-user1", r.URL.Query().Get("accountId")))(w, r, b)
			})
			// A credential already in use: its first answer was a user.
			if _, err := c.User(t.Context(), "acc-user1"); err != nil {
				t.Fatal(err)
			}
			withheld = true
			if _, err := c.User(t.Context(), "acc-user2"); err == nil {
				t.Fatal("the withheld lookup succeeded")
			}
			withheld = false
			if u, err := c.User(t.Context(), "acc-user2"); err != nil || u.AccountID != "acc-user2" {
				t.Fatalf("after the grant: %+v, %v", u, err)
			}
			if n := f.Count("GET", "/rest/api/3/user"); n != 3 {
				t.Fatalf("%d lookups, want 3", n)
			}
		})
	}
}

// A user is read once and handed out until it is siteAge old, so a
// changed name, time zone or active state shows by then.
func TestUserIsCachedUntilItAges(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	read := time.Now()
	at := read
	c.users.clock = func() time.Time { return at }
	f.On("GET", "/rest/api/3/user", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "accountId", "acc-user1")
		jiratest.Reply(200, userOneJSON)(w, r, b)
	})
	for range 2 {
		u, err := c.User(t.Context(), "acc-user1")
		if err != nil || u.DisplayName != "User 1" || u.Email != "user1@example.com" || !u.Active {
			t.Fatalf("got %+v, %v", u, err)
		}
	}
	if n := f.Count("GET", "/rest/api/3/user"); n != 1 {
		t.Errorf("%d reads, want 1 then the cache", n)
	}
	for i, age := range []time.Duration{siteAge - time.Nanosecond, siteAge} {
		at = read.Add(age)
		if _, err := c.User(t.Context(), "acc-user1"); err != nil {
			t.Fatal(err)
		}
		if n := f.Count("GET", "/rest/api/3/user"); n != 1+i {
			t.Errorf("%d reads with the user %s old, want %d", n, age, 1+i)
		}
	}
}

func TestCacheIsBoundedAndFenced(t *testing.T) {
	now := signedIn(t, nil).api.CredentialMark
	mark := now()
	uc := newCache[string, User](2, 0, nil, now)
	for _, id := range []string{"a", "b", "c"} {
		uc.put(uc.epoch(), mark, id, User{AccountID: id})
	}
	if _, ok := uc.get(t.Context(), "a"); ok {
		t.Error("the oldest user was kept past the bound")
	}
	uc.get(t.Context(), "b")
	uc.put(uc.epoch(), mark, "d", User{AccountID: "d"})
	if _, ok := uc.get(t.Context(), "b"); !ok {
		t.Error("a recently used user was evicted")
	}
	if _, ok := uc.get(t.Context(), "c"); ok {
		t.Error("the least recently used user was kept")
	}
	old := uc.epoch()
	uc.clear()
	uc.put(old, mark, "e", User{AccountID: "e"})
	if _, ok := uc.get(t.Context(), "e"); ok {
		t.Error("a store from before the clear was kept")
	}
	// A value drawn under no credential, or under a read the transport no
	// longer holds, is not handed out.
	uc.put(uc.epoch(), trackerapi.CredentialMark{}, "f", User{AccountID: "f"})
	if _, ok := uc.get(t.Context(), "f"); ok {
		t.Error("a value of no credential read was kept")
	}
	other := newCache[string, User](2, 0, nil, signedIn(t, nil).api.CredentialMark)
	other.put(other.epoch(), other.now(), "h", User{AccountID: "h"})
	other.put(other.epoch(), other.now(), "i", User{AccountID: "i"})
	other.put(other.epoch(), mark, "g", User{AccountID: "g"})
	if _, ok := other.get(t.Context(), "g"); ok {
		t.Error("a value of another client's credential read was kept")
	}
	// Nor is it stored, so it evicts nothing live.
	if _, ok := other.get(t.Context(), "h"); !ok {
		t.Error("a value of another credential read evicted a live one")
	}
}

func TestMyself(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", myselfURL, jiratest.Reply(200, userOneJSON))
	u, err := c.Myself(t.Context())
	if err != nil || u.AccountID != "acc-user1" {
		t.Fatalf("got %+v %v", u, err)
	}
}

// userSearch answers user/search: by account ID from byID, and by query
// from the list byQuery names, windowed by startAt and maxResults.
func userSearch(t *testing.T, byID map[string]string, byQuery map[string][]string) jiratest.Handler {
	return func(w http.ResponseWriter, r *http.Request, b []byte) {
		q := r.URL.Query()
		if id := q.Get("accountId"); id != "" {
			if q.Get("query") != "" {
				t.Error("a user search by both query and account ID")
			}
			if u, ok := byID[id]; ok {
				jiratest.Reply(200, `[`+u+`]`)(w, r, b)
				return
			}
			jiratest.Reply(200, `[]`)(w, r, b)
			return
		}
		all := byQuery[q.Get("query")]
		start, _ := strconv.Atoi(q.Get("startAt"))
		max, _ := strconv.Atoi(q.Get("maxResults"))
		end := min(start+max, len(all), maxUserSearch)
		if start >= end {
			jiratest.Reply(200, `[]`)(w, r, b)
			return
		}
		jiratest.Reply(200, `[`+strings.Join(all[start:end], ",")+`]`)(w, r, b)
	}
}

func person(id, name string, active bool, kind string) string {
	return fmt.Sprintf(`{"accountId":%q,"displayName":%q,"active":%v,"accountType":%q}`, id, name, active, kind)
}

func TestFindUser(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/user/search", userSearch(t, map[string]string{"acc-user2": userTwoJSON}, map[string][]string{
		"User 1":            {userOneJSON, person("acc-user10", "User 10", true, "atlassian")},
		"Twin":              {person("acc-a", "Twin", true, "atlassian"), person("acc-b", "twin", true, "atlassian")},
		"user2@example.com": {userTwoJSON},
		"Use":               {person("acc-gone", "Usera", false, "atlassian"), person("acc-bot", "Userbot", true, "app"), userOneJSON},
	}))
	for who, want := range map[string]string{"acc-user2": "acc-user2", "User 1": "acc-user1", "user2@example.com": "acc-user2"} {
		u, err := c.FindUser(t.Context(), who)
		if err != nil || u.AccountID != want {
			t.Errorf("%s: got %+v %v", who, u, err)
		}
	}
	_, err := c.FindUser(t.Context(), "Twin")
	if me, ok := errors.AsType[*MatchError](err); !ok || !me.Ambiguous || len(me.Candidates) != 2 {
		t.Errorf("ambiguous: %v", err)
	}
	_, err = c.FindUser(t.Context(), "Use")
	me, ok := errors.AsType[*MatchError](err)
	if !ok || me.Ambiguous || len(me.Candidates) != 1 || me.Candidates[0].ID != "acc-user1" {
		t.Errorf("no exact match must list the active people found: %v", err)
	}
	if !trackerapi.WriteNotApplied(notSentErr(err)) {
		t.Error("a match error found before sending is not sent")
	}
}

// TestFindUserWalksTheWholeWindow proves a name is unique only when the
// search was read to its end: a second exact match past the first page
// makes it ambiguous, and a window Jira cuts makes it unprovable.
func TestFindUserWalksTheWholeWindow(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	many := func(n int, twinAt int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = person(fmt.Sprintf("acc-%d", i), fmt.Sprintf("Sam %d", i), true, "atlassian")
		}
		out[0] = person("acc-first", "Sam", true, "atlassian")
		if twinAt > 0 {
			out[twinAt] = person("acc-twin", "Sam", true, "atlassian")
		}
		return out
	}
	byQuery := map[string][]string{"Sam": many(101, 100)}
	f.On("GET", "/rest/api/3/user/search", userSearch(t, nil, byQuery))
	_, err := c.FindUser(t.Context(), "Sam")
	if me, ok := errors.AsType[*MatchError](err); !ok || !me.Ambiguous || len(me.Candidates) != 2 {
		t.Errorf("a twin at 101: %v", err)
	}
	byQuery["Sam"] = many(1500, 0)
	_, err = c.FindUser(t.Context(), "Sam")
	if _, ok := errors.AsType[*UnprovenMatchError](err); !ok {
		t.Errorf("a window Jira cuts: %v", err)
	}
	byQuery["Sam"] = many(999, 0)
	if u, err := c.FindUser(t.Context(), "Sam"); err != nil || u.AccountID != "acc-first" {
		t.Errorf("a window read to its end: %+v %v", u, err)
	}
	// A search that ignores startAt hands out its first page again: the
	// walk fails at the repeat rather than read the window through.
	before := f.Count("GET", "/rest/api/3/user/search")
	f.On("GET", "/rest/api/3/user/search", jiratest.Reply(200, `[`+person("acc-first", "Sam Smith", true, "atlassian")+`]`))
	_, err = c.FindUser(t.Context(), "Sam Smith")
	if err == nil || !strings.Contains(err.Error(), "repeated") || f.Count("GET", "/rest/api/3/user/search")-before != 2 {
		t.Errorf("a repeated page: %v after %d pages", err, f.Count("GET", "/rest/api/3/user/search")-before)
	}
}

// TestFindUserByIDIsEligibleOnly proves an account ID, from the cache
// or a search, names only an active user who is not an app.
func TestFindUserByIDIsEligibleOnly(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/user/search", userSearch(t, map[string]string{"acc-bot": person("acc-bot", "Bot", true, "app")}, nil))
	f.On("GET", "/rest/api/3/user", jiratest.Reply(200, person("acc-gone", "Gone", false, "atlassian")))
	if _, err := c.User(t.Context(), "acc-gone"); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"acc-gone", "acc-bot"} {
		if _, err := c.FindUser(t.Context(), who); err == nil {
			t.Errorf("%s was found", who)
		}
	}
}

// assignableSearch answers user/assignable/search as the spec says:
// 400 without one of issueKey, issueId or project, and 400 unless exactly
// one of query and accountId is sent. assignable holds the users who can
// be assigned, by account ID; a query matches the start of their names.
func assignableSearch(t *testing.T, assignable map[string]string, names map[string]string) jiratest.Handler {
	return func(w http.ResponseWriter, r *http.Request, b []byte) {
		q := r.URL.Query()
		scopes := 0
		for _, k := range []string{"issueKey", "issueId", "project"} {
			if q.Has(k) {
				scopes++
			}
		}
		if scopes == 0 || q.Has("query") == q.Has("accountId") {
			t.Errorf("a request the spec refuses: %s", r.URL.RawQuery)
			jiratest.Reply(400, `{"errorMessages":["query or accountId is missing"]}`)(w, r, b)
			return
		}
		var out []string
		for id, js := range assignable {
			if id == q.Get("accountId") || (q.Has("query") && strings.HasPrefix(names[id], q.Get("query"))) {
				out = append(out, js)
			}
		}
		slices.Sort(out)
		jiratest.Reply(200, "["+strings.Join(out, ",")+"]")(w, r, b)
	}
}

func TestAssignableUsers(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	search := assignableSearch(t, map[string]string{"acc-user1": userOneJSON}, map[string]string{"acc-user1": "User 1"})
	f.On("GET", "/rest/api/3/user/assignable/search", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "project", "WEB")
		jiratest.WantQuery(t, r, "startAt", "50")
		jiratest.WantQuery(t, r, "maxResults", "50")
		search(w, r, b)
	})
	for _, q := range []AssignableQuery{{Project: "WEB", Query: "User", StartAt: 50}, {Project: "WEB", AccountID: "acc-user1", StartAt: 50}} {
		us, err := c.AssignableUsers(t.Context(), q)
		if err != nil || len(us) != 1 || us[0].AccountID != "acc-user1" {
			t.Fatalf("%+v: got %v %v", q, us, err)
		}
	}
	for _, q := range []AssignableQuery{
		{}, {Issue: "WEB-1", Project: "WEB", Query: "a"}, {Issue: "../x", Query: "a"},
		{Issue: "WEB-1", Query: "a", AccountID: "b"}, {Issue: "WEB-1"}, {Project: "WEB"},
	} {
		if _, err := c.AssignableUsers(t.Context(), q); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%+v: want a refusal before sending, got %v", q, err)
		}
	}
	if n := f.Count("GET", "/rest/api/3/user/assignable/search"); n != 2 {
		t.Errorf("%d requests, want 2", n)
	}
}

// TestAssignableUsersNamesTheIssueRightly proves an issue ID is sent as
// issueId and a key as issueKey.
func TestAssignableUsersNamesTheIssueRightly(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	var got []string
	f.On("GET", "/rest/api/3/user/assignable/search", func(w http.ResponseWriter, r *http.Request, b []byte) {
		got = append(got, "id="+r.URL.Query().Get("issueId")+" key="+r.URL.Query().Get("issueKey"))
		jiratest.Reply(200, `[]`)(w, r, b)
	})
	for _, ref := range []string{"10001", "WEB-1"} {
		if _, err := c.AssignableUsers(t.Context(), AssignableQuery{Issue: ref, Query: "User"}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(got, "; ") != "id=10001 key=; id= key=WEB-1" {
		t.Errorf("sent %v", got)
	}
}
