package jira

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"agent-overflow/internal/tracker/trackerapi"
)

// userCacheSize bounds the users a Client keeps by account ID.
const userCacheSize = 2000

// maxUserSearch is the window of a user search: Jira takes the users that
// match from the first to the thousandth, however a search pages.
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

// Myself asks Jira who the credential belongs to; every call is a
// request, so it verifies the credential. Me answers from what it read.
func (c *Client) Myself(ctx context.Context) (User, error) {
	if err := c.admit(ctx); err != nil {
		return User{}, err
	}
	epoch, meEpoch := c.users.epoch(), c.me.epoch()
	var u userJSON
	resp, err := c.call(ctx, trackerapi.Request{Path: myselfPath}, &u)
	if err != nil {
		return User{}, fmt.Errorf("jira: who the credential belongs to: %w", err)
	}
	me := u.user()
	c.keepUsers(epoch, me)
	if mark := resp.CredentialMark(); mark != (trackerapi.CredentialMark{}) {
		c.me.put(meEpoch, struct{}{}, signedIn{user: me, mark: mark})
	}
	return me, nil
}

// signedIn is the user Myself read and the credential read its answer
// was drawn under.
type signedIn struct {
	user User
	mark trackerapi.CredentialMark
}

// Me is the user the credential belongs to, read once per credential:
// Myself's last answer while every answer since came under the same read
// of the credential, and a Myself otherwise. ForgetCredential and a
// refused credential drop it, and an answer drawn under another read of
// the credential (the transport read it again after a 401) retires it.
func (c *Client) Me(ctx context.Context) (User, error) {
	if err := c.admit(ctx); err != nil {
		return User{}, err
	}
	if s, ok := c.me.get(struct{}{}); ok && s.mark == c.lastMark() {
		return s.user, nil
	}
	return c.Myself(ctx)
}

// User returns the user with accountID, from the client's cache when it
// holds it. A missing user is an error matching trackerapi.ErrNotFound.
func (c *Client) User(ctx context.Context, accountID string) (User, error) {
	if err := c.admit(ctx); err != nil {
		return User{}, err
	}
	if accountID == "" {
		return User{}, notSent("an account ID is required")
	}
	if u, ok := c.users.get(accountID); ok {
		return u, nil
	}
	epoch := c.users.epoch()
	var u userJSON
	if err := c.get(ctx, "user", url.Values{"accountId": {accountID}}, &u); err != nil {
		return User{}, fmt.Errorf("jira: user %s: %w", accountID, err)
	}
	c.keepUsers(epoch, u.user())
	return u.user(), nil
}

// keepUsers caches us under epoch.
func (c *Client) keepUsers(epoch uint64, us ...User) {
	for _, u := range us {
		if u.AccountID != "" {
			c.users.put(epoch, u.AccountID, u)
		}
	}
}

// ForgetUsers drops the cached users; a read in flight stores nothing.
func (c *Client) ForgetUsers() { c.users.clear() }

// SearchUsers returns the users whose display name or email starts with
// query, at most max (zero means 50, and Jira stops at 1000). Without the
// Browse users and groups permission Jira returns none.
func (c *Client) SearchUsers(ctx context.Context, query string, max int) ([]User, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(query) == "" {
		return nil, notSent("a user search needs a query")
	}
	found, err := c.userSearch(ctx, url.Values{"query": {query}}, 0, pageSize(max, 50, maxUserSearch))
	if err != nil {
		return nil, fmt.Errorf("jira: search users for %q: %w", query, err)
	}
	return found, nil
}

// userSearch is one page of user/search from startAt.
func (c *Client) userSearch(ctx context.Context, q url.Values, startAt, max int) ([]User, error) {
	q.Set("startAt", strconv.Itoa(startAt))
	q.Set("maxResults", strconv.Itoa(max))
	epoch := c.users.epoch()
	var found []userJSON
	if err := c.get(ctx, "user/search", q, &found); err != nil {
		return nil, err
	}
	out := users(found)
	c.keepUsers(epoch, out...)
	return out, nil
}

// AssignableQuery selects users who can be assigned an issue: an existing
// issue (Issue, a key or ID) or a new one in a project (Project). Exactly
// one is set.
type AssignableQuery struct {
	Issue   string
	Project string
	// Query matches the start of a display name or email. AccountID asks
	// about one user instead, who is returned only when assignable.
	// Exactly one is set: Jira refuses a request with neither or both.
	Query     string
	AccountID string
	// StartAt and MaxResults select the window of users Jira checks;
	// MaxResults zero means 50. Jira returns only the assignable users in
	// the window, so a short page does not mean there are no more.
	StartAt    int
	MaxResults int
}

// AssignableUsers returns the users in q's window who can be assigned.
func (c *Client) AssignableUsers(ctx context.Context, q AssignableQuery) ([]User, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	v := url.Values{}
	switch {
	case (q.Issue == "") == (q.Project == ""):
		return nil, notSent("assignable users need exactly one of an issue or a project")
	case q.Issue != "":
		if err := issueParam(v, q.Issue); err != nil {
			return nil, err
		}
	default:
		if err := checkProject(q.Project); err != nil {
			return nil, err
		}
		v.Set("project", q.Project)
	}
	switch {
	case q.StartAt < 0 || q.MaxResults < 0:
		return nil, notSent("assignable users: a negative window")
	case (q.Query == "") == (q.AccountID == ""):
		return nil, notSent("assignable users need exactly one of a query or an account ID")
	case q.Query != "":
		v.Set("query", q.Query)
	default:
		v.Set("accountId", q.AccountID)
	}
	v.Set("startAt", strconv.Itoa(q.StartAt))
	v.Set("maxResults", strconv.Itoa(pageSize(q.MaxResults, 50, maxUserSearch)))
	epoch := c.users.epoch()
	var found []userJSON
	if err := c.get(ctx, "user/assignable/search", v, &found); err != nil {
		return nil, fmt.Errorf("jira: assignable users: %w", err)
	}
	out := users(found)
	c.keepUsers(epoch, out...)
	return out, nil
}

// FindUser resolves who, an account ID, an exact display name or an
// email (case ignored), to one active user who is not an app. A name
// several users share is a *MatchError listing them, and no exact match
// is a *MatchError listing the users whose name or email starts with
// who. A name more users start with than Jira's search window holds is
// an *UnprovenMatchError, since a second exact match may lie beyond it.
func (c *Client) FindUser(ctx context.Context, who string) (User, error) {
	if err := c.admit(ctx); err != nil {
		return User{}, err
	}
	return c.findUser(ctx, who, nil)
}

// findUser is FindUser, and with scope set also checks that the user can
// be assigned there, asking about the user by account ID.
func (c *Client) findUser(ctx context.Context, who string, scope *AssignableQuery) (User, error) {
	who = strings.TrimSpace(who)
	if who == "" {
		return User{}, notSent("a user needs an account ID, name or email")
	}
	u, err := c.resolveUser(ctx, who)
	if err != nil {
		return User{}, err
	}
	if !eligible(u) {
		return User{}, &MatchError{What: "active user", Input: who}
	}
	if scope == nil {
		return u, nil
	}
	q := *scope
	q.AccountID = u.AccountID
	found, err := c.AssignableUsers(ctx, q)
	if err != nil {
		return User{}, fmt.Errorf("jira: find user %q: %w", who, err)
	}
	for _, a := range found {
		if a.AccountID == u.AccountID {
			return u, nil
		}
	}
	return User{}, &MatchError{What: "user who can be assigned " + firstNonEmpty(q.Issue, "in "+q.Project), Input: who}
}

// eligible reports a user a value may name: active and not an app.
func eligible(u User) bool { return u.Active && u.AccountType != "app" }

// resolveUser finds who by account ID, then by exact name or email.
func (c *Client) resolveUser(ctx context.Context, who string) (User, error) {
	if !strings.ContainsFunc(who, unicode.IsSpace) && !strings.Contains(who, "@") {
		if u, ok := c.users.get(who); ok {
			return u, nil
		}
		byID, err := c.userSearch(ctx, url.Values{"accountId": {who}}, 0, 1)
		if err != nil {
			return User{}, fmt.Errorf("jira: find user %q: %w", who, err)
		}
		for _, u := range byID {
			if u.AccountID == who {
				return u, nil
			}
		}
	}
	found, complete, err := c.usersMatching(ctx, who)
	if err != nil {
		return User{}, fmt.Errorf("jira: find user %q: %w", who, err)
	}
	var exact, all []User
	for _, u := range found {
		if !eligible(u) {
			continue
		}
		all = append(all, u)
		if strings.EqualFold(u.DisplayName, who) || (u.Email != "" && strings.EqualFold(u.Email, who)) {
			exact = append(exact, u)
		}
	}
	switch {
	case len(exact) > 1:
		return User{}, &MatchError{What: "user", Input: who, Ambiguous: true, Candidates: userCandidates(exact)}
	case !complete:
		return User{}, &UnprovenMatchError{What: "user", Input: who, Limit: maxUserSearch}
	case len(exact) == 1:
		return exact[0], nil
	}
	return User{}, &MatchError{What: "user", Input: who, Candidates: userCandidates(all)}
}

// usersMatching walks the user search for query to its end: a page that
// comes back empty, or the end of Jira's window, when complete is false
// because more users may match than it lists. A page that holds no user
// not seen already fails the walk rather than repeat it.
func (c *Client) usersMatching(ctx context.Context, query string) (found []User, complete bool, err error) {
	seen := map[string]bool{}
	for start := 0; start < maxUserSearch; {
		page, err := c.userSearch(ctx, url.Values{"query": {query}}, start, maxUserSearch-start)
		if err != nil {
			return nil, false, err
		}
		if len(page) == 0 {
			return found, true, nil
		}
		fresh := false
		for _, u := range page {
			if !seen[u.AccountID] {
				seen[u.AccountID], fresh = true, true
				found = append(found, u)
			}
		}
		if !fresh {
			return nil, false, fmt.Errorf("the user search repeated its page at %d", start)
		}
		start += len(page)
	}
	return found, false, nil
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
