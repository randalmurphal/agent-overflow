package jira

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/trackerapi"
)

func TestUserIsCachedAndForgotten(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/user", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "accountId", "acc-user1")
		reply(200, userOneJSON)(w, r, b)
	})
	for range 2 {
		u, err := c.User(t.Context(), "acc-user1")
		if err != nil || u.DisplayName != "User 1" || u.Email != "user1@example.com" || !u.Active {
			t.Fatalf("got %+v, %v", u, err)
		}
	}
	if n := f.count("GET", "/rest/api/3/user"); n != 1 {
		t.Errorf("%d reads, want 1 then the cache", n)
	}
	c.ForgetUsers()
	if _, err := c.User(t.Context(), "acc-user1"); err != nil {
		t.Fatal(err)
	}
	if n := f.count("GET", "/rest/api/3/user"); n != 2 {
		t.Errorf("ForgetUsers kept the user: %d reads", n)
	}
}

// TestForgetUsersBeatsAReadInFlight proves a read that started before
// ForgetUsers stores nothing when it ends after it.
func TestForgetUsersBeatsAReadInFlight(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	entered, release := make(chan struct{}), make(chan struct{})
	f.on("GET", "/rest/api/3/user", func(w http.ResponseWriter, r *http.Request, b []byte) {
		if c.users.epoch() == 0 {
			close(entered)
			<-release
		}
		reply(200, userOneJSON)(w, r, b)
	})
	done := make(chan error)
	go func() {
		_, err := c.User(t.Context(), "acc-user1")
		done <- err
	}()
	<-entered
	c.ForgetUsers()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := c.users.get("acc-user1"); ok {
		t.Error("a read older than ForgetUsers stored its user")
	}
}

func TestCacheIsBoundedAndFenced(t *testing.T) {
	uc := newCache[string, User](2, nil)
	for _, id := range []string{"a", "b", "c"} {
		uc.put(uc.epoch(), id, User{AccountID: id})
	}
	if _, ok := uc.get("a"); ok {
		t.Error("the oldest user was kept past the bound")
	}
	uc.get("b")
	uc.put(uc.epoch(), "d", User{AccountID: "d"})
	if _, ok := uc.get("b"); !ok {
		t.Error("a recently used user was evicted")
	}
	if _, ok := uc.get("c"); ok {
		t.Error("the least recently used user was kept")
	}
	old := uc.epoch()
	uc.clear()
	uc.put(old, "e", User{AccountID: "e"})
	if _, ok := uc.get("e"); ok {
		t.Error("a store from before the clear was kept")
	}
}

func TestMyself(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", myselfURL, reply(200, userOneJSON))
	u, err := c.Myself(t.Context())
	if err != nil || u.AccountID != "acc-user1" {
		t.Fatalf("got %+v %v", u, err)
	}
}

// userSearch answers user/search: by account ID from byID, and by query
// from the list byQuery names, windowed by startAt and maxResults.
func userSearch(t *testing.T, byID map[string]string, byQuery map[string][]string) handler {
	return func(w http.ResponseWriter, r *http.Request, b []byte) {
		q := r.URL.Query()
		if id := q.Get("accountId"); id != "" {
			if q.Get("query") != "" {
				t.Error("a user search by both query and account ID")
			}
			if u, ok := byID[id]; ok {
				reply(200, `[`+u+`]`)(w, r, b)
				return
			}
			reply(200, `[]`)(w, r, b)
			return
		}
		all := byQuery[q.Get("query")]
		start, _ := strconv.Atoi(q.Get("startAt"))
		max, _ := strconv.Atoi(q.Get("maxResults"))
		end := min(start+max, len(all), maxUserSearch)
		if start >= end {
			reply(200, `[]`)(w, r, b)
			return
		}
		reply(200, `[`+strings.Join(all[start:end], ",")+`]`)(w, r, b)
	}
}

func person(id, name string, active bool, kind string) string {
	return fmt.Sprintf(`{"accountId":%q,"displayName":%q,"active":%v,"accountType":%q}`, id, name, active, kind)
}

func TestFindUser(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/user/search", userSearch(t, map[string]string{"acc-user2": userTwoJSON}, map[string][]string{
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
	f := newFake(t)
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
	f.on("GET", "/rest/api/3/user/search", userSearch(t, nil, byQuery))
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
	before := f.count("GET", "/rest/api/3/user/search")
	f.on("GET", "/rest/api/3/user/search", reply(200, `[`+person("acc-first", "Sam Smith", true, "atlassian")+`]`))
	_, err = c.FindUser(t.Context(), "Sam Smith")
	if err == nil || !strings.Contains(err.Error(), "repeated") || f.count("GET", "/rest/api/3/user/search")-before != 2 {
		t.Errorf("a repeated page: %v after %d pages", err, f.count("GET", "/rest/api/3/user/search")-before)
	}
}

// TestFindUserByIDIsEligibleOnly proves an account ID, from the cache
// or a search, names only an active user who is not an app.
func TestFindUserByIDIsEligibleOnly(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/user/search", userSearch(t, map[string]string{"acc-bot": person("acc-bot", "Bot", true, "app")}, nil))
	f.on("GET", "/rest/api/3/user", reply(200, person("acc-gone", "Gone", false, "atlassian")))
	if _, err := c.User(t.Context(), "acc-gone"); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"acc-gone", "acc-bot"} {
		if _, err := c.FindUser(t.Context(), who); err == nil {
			t.Errorf("%s was found", who)
		}
	}
}

func TestAssignableUsers(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/user/assignable/search", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "project", "WEB")
		wantQuery(t, r, "query", "User")
		wantQuery(t, r, "startAt", "50")
		wantQuery(t, r, "maxResults", "50")
		reply(200, `[`+userOneJSON+`]`)(w, r, b)
	})
	us, err := c.AssignableUsers(t.Context(), AssignableQuery{Project: "WEB", Query: "User", StartAt: 50})
	if err != nil || len(us) != 1 {
		t.Fatalf("got %v %v", us, err)
	}
	for _, q := range []AssignableQuery{{}, {Issue: "WEB-1", Project: "WEB"}, {Issue: "../x"}, {Issue: "WEB-1", Query: "a", AccountID: "b"}} {
		if _, err := c.AssignableUsers(t.Context(), q); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%+v: want a refusal before sending, got %v", q, err)
		}
	}
}

// TestAssignableUsersNamesTheIssueRightly proves an issue ID is sent as
// issueId and a key as issueKey.
func TestAssignableUsersNamesTheIssueRightly(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	var got []string
	f.on("GET", "/rest/api/3/user/assignable/search", func(w http.ResponseWriter, r *http.Request, b []byte) {
		got = append(got, "id="+r.URL.Query().Get("issueId")+" key="+r.URL.Query().Get("issueKey"))
		reply(200, `[]`)(w, r, b)
	})
	for _, ref := range []string{"10001", "WEB-1"} {
		if _, err := c.AssignableUsers(t.Context(), AssignableQuery{Issue: ref}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(got, "; ") != "id=10001 key=; id= key=WEB-1" {
		t.Errorf("sent %v", got)
	}
}
