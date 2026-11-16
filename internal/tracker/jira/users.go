package jira

import (
	"container/list"
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// userCacheSize bounds the users a Client keeps by account ID.
const userCacheSize = 2000

// maxUserSearch is the most users one search returns; Jira stops at the
// thousandth user however a search pages.
const maxUserSearch = 1000

// User is a Jira account. Email and TimeZone are empty when the user's
// privacy settings hide them.
type User struct {
	AccountID   string
	DisplayName string
	Email       string
	Active      bool
	// AccountType is atlassian, app or customer.
	AccountType string
	TimeZone    string
	AvatarURL   string // 48x48
}

// userJSON is a user as the API sends it.
type userJSON struct {
	AccountID   string            `json:"accountId"`
	DisplayName string            `json:"displayName"`
	Email       string            `json:"emailAddress"`
	Active      bool              `json:"active"`
	AccountType string            `json:"accountType"`
	TimeZone    string            `json:"timeZone"`
	Avatars     map[string]string `json:"avatarUrls"`
}

func (u userJSON) user() User {
	return User{
		AccountID: u.AccountID, DisplayName: u.DisplayName, Email: u.Email, Active: u.Active,
		AccountType: u.AccountType, TimeZone: u.TimeZone, AvatarURL: u.Avatars["48x48"],
	}
}

// userPtr is u, or nil when the JSON held no account.
func (u *userJSON) userPtr() *User {
	if u == nil || u.AccountID == "" {
		return nil
	}
	out := u.user()
	return &out
}

func users(in []userJSON) []User {
	out := make([]User, len(in))
	for i, u := range in {
		out[i] = u.user()
	}
	return out
}

// Myself returns the user the credential belongs to.
func (c *Client) Myself(ctx context.Context) (User, error) {
	var u userJSON
	if err := c.get(ctx, "myself", nil, &u); err != nil {
		return User{}, fmt.Errorf("jira: who the credential belongs to: %w", err)
	}
	c.users.put(u.user())
	return u.user(), nil
}

// User returns the user with accountID, from the client's cache when it
// holds it. A missing user is an error matching trackerapi.ErrNotFound.
func (c *Client) User(ctx context.Context, accountID string) (User, error) {
	if accountID == "" {
		return User{}, notSent("an account ID is required")
	}
	if u, ok := c.users.get(accountID); ok {
		return u, nil
	}
	var u userJSON
	if err := c.get(ctx, "user", url.Values{"accountId": {accountID}}, &u); err != nil {
		return User{}, fmt.Errorf("jira: user %s: %w", accountID, err)
	}
	c.users.put(u.user())
	return u.user(), nil
}

// ForgetUsers drops the cached users.
func (c *Client) ForgetUsers() { c.users.clear() }

// SearchUsers returns the active users whose display name or email
// starts with query, at most max (zero means 50, and Jira stops at 1000).
// Without the Browse users and groups permission Jira returns none.
func (c *Client) SearchUsers(ctx context.Context, query string, max int) ([]User, error) {
	if strings.TrimSpace(query) == "" {
		return nil, notSent("a user search needs a query")
	}
	found, err := c.userSearch(ctx, url.Values{"query": {query}}, max)
	if err != nil {
		return nil, fmt.Errorf("jira: search users for %q: %w", query, err)
	}
	return found, nil
}

func (c *Client) userSearch(ctx context.Context, q url.Values, max int) ([]User, error) {
	q.Set("maxResults", strconv.Itoa(pageSize(max, 50, maxUserSearch)))
	var found []userJSON
	if err := c.get(ctx, "user/search", q, &found); err != nil {
		return nil, err
	}
	out := users(found)
	for _, u := range out {
		c.users.put(u)
	}
	return out, nil
}

// AssignableQuery selects users who can be assigned an issue: an existing
// issue (Issue) or a new one in a project (Project). Exactly one is set.
type AssignableQuery struct {
	Issue   string
	Project string
	// Query matches the start of a display name or email; empty lists
	// everyone in the window.
	Query string
	// StartAt and MaxResults select the window of users Jira checks;
	// MaxResults zero means 50. Jira returns only the assignable users in
	// the window, so a short page does not mean there are no more.
	StartAt    int
	MaxResults int
}

// AssignableUsers returns the users in q's window who can be assigned.
func (c *Client) AssignableUsers(ctx context.Context, q AssignableQuery) ([]User, error) {
	v := url.Values{}
	switch {
	case (q.Issue == "") == (q.Project == ""):
		return nil, notSent("assignable users need exactly one of an issue or a project")
	case q.Issue != "":
		if err := checkIssue(q.Issue); err != nil {
			return nil, err
		}
		v.Set("issueKey", q.Issue)
	default:
		if err := checkProject(q.Project); err != nil {
			return nil, err
		}
		v.Set("project", q.Project)
	}
	if q.StartAt < 0 || q.MaxResults < 0 {
		return nil, notSent("assignable users: a negative window")
	}
	if q.Query != "" {
		v.Set("query", q.Query)
	}
	v.Set("startAt", strconv.Itoa(q.StartAt))
	v.Set("maxResults", strconv.Itoa(pageSize(q.MaxResults, 50, maxUserSearch)))
	var found []userJSON
	if err := c.get(ctx, "user/assignable/search", v, &found); err != nil {
		return nil, fmt.Errorf("jira: assignable users: %w", err)
	}
	out := users(found)
	for _, u := range out {
		c.users.put(u)
	}
	return out, nil
}

// FindUser resolves who, an account ID, an exact display name or an
// email (case ignored), to one active user. A name several users share
// is a *MatchError listing them; no exact match is a *MatchError listing
// the users whose name or email starts with who.
func (c *Client) FindUser(ctx context.Context, who string) (User, error) {
	return c.findUser(ctx, who, nil)
}

// findUser is FindUser searching with search when set (the users
// assignable to one issue), or the site's user search.
func (c *Client) findUser(ctx context.Context, who string, search func(context.Context, string) ([]User, error)) (User, error) {
	who = strings.TrimSpace(who)
	if who == "" {
		return User{}, notSent("a user needs an account ID, name or email")
	}
	if !strings.ContainsFunc(who, unicode.IsSpace) && !strings.Contains(who, "@") {
		if u, ok := c.users.get(who); ok {
			return u, nil
		}
		byID, err := c.userSearch(ctx, url.Values{"accountId": {who}}, 1)
		if err != nil {
			return User{}, fmt.Errorf("jira: find user %q: %w", who, err)
		}
		for _, u := range byID {
			if u.AccountID == who {
				return u, nil
			}
		}
	}
	if search == nil {
		search = func(ctx context.Context, q string) ([]User, error) {
			return c.userSearch(ctx, url.Values{"query": {q}}, 100)
		}
	}
	found, err := search(ctx, who)
	if err != nil {
		return User{}, fmt.Errorf("jira: find user %q: %w", who, err)
	}
	var exact, all []User
	for _, u := range found {
		if !u.Active || u.AccountType == "app" {
			continue
		}
		all = append(all, u)
		if strings.EqualFold(u.DisplayName, who) || (u.Email != "" && strings.EqualFold(u.Email, who)) {
			exact = append(exact, u)
		}
	}
	if len(exact) == 1 {
		return exact[0], nil
	}
	m := &MatchError{What: "user", Input: who, Ambiguous: len(exact) > 1, Candidates: userCandidates(all)}
	if m.Ambiguous {
		m.Candidates = userCandidates(exact)
	}
	return User{}, m
}

func userCandidates(us []User) []Candidate {
	out := make([]Candidate, len(us))
	for i, u := range us {
		name := u.DisplayName
		if u.Email != "" {
			name += " <" + u.Email + ">"
		}
		out[i] = Candidate{ID: u.AccountID, Name: name}
	}
	return out
}

// pageSize is n, or def when n is zero, capped at max.
func pageSize(n, def, max int) int {
	if n <= 0 {
		n = def
	}
	return min(n, max)
}

// userCache keeps the most recently used users by account ID, at most
// size of them.
type userCache struct {
	mu    sync.Mutex
	size  int
	order *list.List // of User, most recent first
	byID  map[string]*list.Element
}

func newUserCache(size int) *userCache {
	return &userCache{size: size, order: list.New(), byID: map[string]*list.Element{}}
}

func (uc *userCache) get(id string) (User, bool) {
	uc.mu.Lock()
	defer uc.mu.Unlock()
	e, ok := uc.byID[id]
	if !ok {
		return User{}, false
	}
	uc.order.MoveToFront(e)
	return e.Value.(User), true
}

func (uc *userCache) put(u User) {
	if u.AccountID == "" {
		return
	}
	uc.mu.Lock()
	defer uc.mu.Unlock()
	if e, ok := uc.byID[u.AccountID]; ok {
		e.Value = u
		uc.order.MoveToFront(e)
		return
	}
	uc.byID[u.AccountID] = uc.order.PushFront(u)
	if uc.order.Len() > uc.size {
		last := uc.order.Back()
		uc.order.Remove(last)
		delete(uc.byID, last.Value.(User).AccountID)
	}
}

func (uc *userCache) clear() {
	uc.mu.Lock()
	defer uc.mu.Unlock()
	uc.order.Init()
	clear(uc.byID)
}
