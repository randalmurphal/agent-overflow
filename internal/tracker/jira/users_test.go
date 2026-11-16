package jira

import (
	"errors"
	"net/http"
	"testing"

	"agent-overflow/internal/tracker/trackerapi"
)

func TestUserIsCachedAndForgotten(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
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

func TestUserCacheIsBounded(t *testing.T) {
	uc := newUserCache(2)
	for _, id := range []string{"a", "b", "c"} {
		uc.put(User{AccountID: id})
	}
	if _, ok := uc.get("a"); ok {
		t.Error("the oldest user was kept past the bound")
	}
	uc.get("b")
	uc.put(User{AccountID: "d"})
	if _, ok := uc.get("b"); !ok {
		t.Error("a recently used user was evicted")
	}
	if _, ok := uc.get("c"); ok {
		t.Error("the least recently used user was kept")
	}
}

func TestMyself(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("GET", myselfURL, reply(200, userOneJSON))
	u, err := c.Myself(t.Context())
	if err != nil || u.AccountID != "acc-user1" {
		t.Fatalf("got %+v %v", u, err)
	}
}

func TestFindUser(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("GET", "/rest/api/3/user/search", func(w http.ResponseWriter, r *http.Request, b []byte) {
		q := r.URL.Query()
		switch {
		case q.Get("accountId") == "acc-user2":
			reply(200, `[`+userTwoJSON+`]`)(w, r, b)
		case q.Get("accountId") != "":
			reply(200, `[]`)(w, r, b)
		case q.Get("query") == "User 1":
			reply(200, `[`+userOneJSON+`,{"accountId":"acc-user10","displayName":"User 10","active":true,"accountType":"atlassian"}]`)(w, r, b)
		case q.Get("query") == "Twin":
			reply(200, `[{"accountId":"acc-a","displayName":"Twin","active":true},{"accountId":"acc-b","displayName":"twin","active":true}]`)(w, r, b)
		case q.Get("query") == "user2@example.com":
			reply(200, `[`+userTwoJSON+`]`)(w, r, b)
		default:
			reply(200, `[{"accountId":"acc-gone","displayName":"Usera","active":false},{"accountId":"acc-bot","displayName":"Userbot","active":true,"accountType":"app"},`+userOneJSON+`]`)(w, r, b)
		}
	})
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

func TestAssignableUsers(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
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
	for _, q := range []AssignableQuery{{}, {Issue: "WEB-1", Project: "WEB"}, {Issue: "../x"}} {
		if _, err := c.AssignableUsers(t.Context(), q); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%+v: want a refusal before sending, got %v", q, err)
		}
	}
}
