package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	c.keepUsers(epoch, resp.CredentialMark(), me)
	c.me.put(meEpoch, resp.CredentialMark(), struct{}{}, me)
	return me, nil
}

// Me is the user the credential belongs to, read once per credential:
// Myself's answer while the transport holds the credential read it was
// drawn under (see cache), and a Myself otherwise. The transport reading
// the credential again for any request, a sign-in check or a download
// included, retires it, as do ForgetCredential and a refused credential.
func (c *Client) Me(ctx context.Context) (User, error) {
	if err := c.admit(ctx); err != nil {
		return User{}, err
	}
	if u, ok := c.me.get(ctx, struct{}{}); ok {
		return u, nil
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
	if u, ok := c.users.get(ctx, accountID); ok {
		return u, nil
	}
	epoch := c.users.epoch()
	var u userJSON
	resp, err := c.call(ctx, trackerapi.Request{Path: apiPath + "user", Query: url.Values{"accountId": {accountID}}}, &u)
	if err != nil {
		return User{}, fmt.Errorf("jira: user %s: %w", accountID, err)
	}
	c.keepUsers(epoch, resp.CredentialMark(), u.user())
	return u.user(), nil
}

// ErrUserWithheld is matched by the error of a poker voter's lookup Jira
// withheld (PokerVoter): the user is missing (also trackerapi.ErrNotFound),
// or the credential may not see users (403, without the Browse users and
// groups permission).
var ErrUserWithheld = errors.New("jira: Jira does not show this user")

// PokerVoter is User for a planning poker voter, whose votes are read
// every few seconds: a lookup Jira withheld matches ErrUserWithheld and
// is kept as long as a user is, so the poll asks again only then. Every
// other caller uses User, which asks each time.
func (c *Client) PokerVoter(ctx context.Context, accountID string) (User, error) {
	if err, ok := c.voterMisses.get(ctx, accountID); ok {
		return User{}, err
	}
	// Taken before the lookup: put drops the answer when the credential
	// changed meanwhile.
	epoch, mark := c.voterMisses.epoch(), c.api.CredentialMark()
	u, err := c.User(ctx, accountID)
	if withheld(err) {
		err = fmt.Errorf("%w: %w", ErrUserWithheld, err)
		c.voterMisses.put(epoch, mark, accountID, err)
	}
	return u, err
}

// withheld reports a user lookup's error that is Jira withholding the
// user: a confirmed 404, or a 403.
func withheld(err error) bool {
	se, ok := errors.AsType[*trackerapi.StatusError](err)
	return errors.Is(err, trackerapi.ErrNotFound) || ok && se.Status == http.StatusForbidden
}

// keepUsers caches us under epoch, drawn under the credential read mark.
func (c *Client) keepUsers(epoch uint64, mark trackerapi.CredentialMark, us ...User) {
	for _, u := range us {
		if u.AccountID != "" {
			c.users.put(epoch, mark, u.AccountID, u)
		}
	}
}

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
	resp, err := c.call(ctx, trackerapi.Request{Path: apiPath + "user/search", Query: q}, &found)
	if err != nil {
		return nil, err
	}
	out := users(found)
	c.keepUsers(epoch, resp.CredentialMark(), out...)
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
	resp, err := c.call(ctx, trackerapi.Request{Path: apiPath + "user/assignable/search", Query: v}, &found)
	if err != nil {
		return nil, fmt.Errorf("jira: assignable users: %w", err)
	}
	out := users(found)
	c.keepUsers(epoch, resp.CredentialMark(), out...)
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
	return c.findUser(ctx, who, false, nil)
}

// findUser is FindUser, by account ID alone when picked, and with scope
// set also checks that the user can be assigned there, asking about the
// user by account ID.
func (c *Client) findUser(ctx context.Context, who string, picked bool, scope *AssignableQuery) (User, error) {
	who = strings.TrimSpace(who)
	if who == "" {
		return User{}, notSent("a user needs an account ID, name or email")
	}
	var u User
	var err error
	if picked {
		u, err = c.userByAccountID(ctx, who)
	} else {
		u, err = c.resolveUser(ctx, who)
	}
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

// userByAccountID finds the user whose account ID is exactly id, as
// picked: a miss is a *MatchError with Picked set.
func (c *Client) userByAccountID(ctx context.Context, id string) (User, error) {
	u, ok, err := c.userWithID(ctx, id)
	switch {
	case err != nil:
		return User{}, err
	case !ok:
		return User{}, &MatchError{What: "user", Input: id, Picked: true}
	}
	return u, nil
}

// userWithID finds the user whose account ID is exactly id, from the
// cache or by Jira's search by account ID.
func (c *Client) userWithID(ctx context.Context, id string) (User, bool, error) {
	if u, ok := c.users.get(ctx, id); ok {
		return u, true, nil
	}
	byID, err := c.userSearch(ctx, url.Values{"accountId": {id}}, 0, 1)
	if err != nil {
		return User{}, false, fmt.Errorf("jira: find user %q: %w", id, err)
	}
	for _, u := range byID {
		if u.AccountID == id {
			return u, true, nil
		}
	}
	return User{}, false, nil
}

// resolveUser finds who by account ID, then by exact name or email.
func (c *Client) resolveUser(ctx context.Context, who string) (User, error) {
	if !strings.ContainsFunc(who, unicode.IsSpace) && !strings.Contains(who, "@") {
		u, ok, err := c.userWithID(ctx, who)
		if err != nil || ok {
			return u, err
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
func (c *Client) usersMatching(ctx context.Context, query string) ([]User, bool, error) {
	r, err := trackerapi.Walk(ctx, func(ctx context.Context) (matched, error) {
		found, complete, err := c.usersMatchingWalk(ctx, query)
		return matched{found, complete}, err
	})
	return r.found, r.complete, err
}

// matched is what usersMatching returns, as one value for walk.
type matched struct {
	found    []User
	complete bool
}

// usersMatchingWalk is one walk of usersMatching.
func (c *Client) usersMatchingWalk(ctx context.Context, query string) (found []User, complete bool, err error) {
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
