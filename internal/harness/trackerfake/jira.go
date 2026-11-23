package trackerfake

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// jiraService serves one Jira site: the platform REST API under
// rest/api/3, and Jira Software's under rest/agile/1.0 and
// rest/software/1.0.
type jiraService struct{ site string }

// jiraRoute is a route of a Jira site.
type jiraRoute = route[*jiraCall]

// jiraCall is one request a route handles.
type jiraCall struct {
	e      *Engine
	s      *jiraSite
	x      *exchange
	params map[string]string
	query  url.Values
	// viewer is the account ID the credential belongs to.
	viewer string
}

// The not-found and refusal texts Jira answers with.
const (
	issueNotFound = "Issue does not exist or you do not have permission to see it."
	jiraAuthText  = "Client must be authenticated to access this resource."
)

func (j jiraService) tenant(e *Engine) *jiraSite { return e.jira[j.site] }

// authenticate admits Basic with the account's email and the site's
// token. Any other credential is anonymous: Jira answers an issue read
// as not found for it, a search with no issues, and anything else 401
// (measured with no credential; a wrong token is unverified and answered
// the same).
func (j jiraService) authenticate(e *Engine, x *exchange) *answer {
	s := j.tenant(e)
	if s == nil {
		return nil // route() reports the unseeded site as unhandled
	}
	user, token, presented := basicAuth(x.r.Header.Get("Authorization"))
	me := s.user(s.Account)
	if presented && strings.EqualFold(user, me.Email) && sameSecret(s.token, token) {
		x.tenant = s.Site
		return nil
	}
	r, _, routed := findRoute(jiraRoutes, x)
	switch {
	case routed && r.anonymous404:
		a := jiraErrors(http.StatusNotFound, issueNotFound)
		return &a
	case routed && r.name() == "POST "+api+"search/jql":
		a := jsonAnswer(http.StatusOK, map[string]any{"issues": []any{}, "isLast": true})
		return &a
	}
	a := answer{status: http.StatusUnauthorized, header: http.Header{"Content-Type": {"text/plain;charset=UTF-8"}}, body: []byte(jiraAuthText)}
	if presented {
		a.header.Set("X-Seraph-LoginReason", "AUTHENTICATED_FAILED")
	}
	return &a
}

// basicAuth reads a Basic credential; presented is false without one.
func basicAuth(header string) (user, token string, presented bool) {
	enc, ok := strings.CutPrefix(header, "Basic ")
	if !ok {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", "", true
	}
	user, token, _ = strings.Cut(string(raw), ":")
	return user, token, true
}

func (j jiraService) route(e *Engine, x *exchange) match {
	s := j.tenant(e)
	if s == nil {
		return match{unhandled: "no Jira site " + j.site + " is seeded"}
	}
	r, params, ok := findRoute(jiraRoutes, x)
	if !ok {
		return match{unhandled: "no route matches"}
	}
	q := x.r.URL.Query()
	if why := r.checkQuery(q); why != "" {
		return match{unhandled: why}
	}
	c := &jiraCall{e: e, s: s, x: x, params: params, query: q, viewer: s.Account}
	return match{name: r.name(), reads: r.reads, run: func() answer { return forAccount(dated(r.handle(c), e.now()), c.viewer) }}
}

// forAccount is a naming the account it was made for. Measured: every
// answer to a request with a valid token carries X-AAccountId, equal to
// myself's accountId (a search, a board configuration, a board issue
// list, a 404 for a missing issue), and no answer to a request with no
// credential does. route runs only for an authenticated request, so the
// answers authenticate makes carry none.
func forAccount(a answer, account string) answer {
	if a.header == nil {
		a.header = http.Header{}
	}
	a.header.Set("X-AAccountId", account)
	return a
}

// dated is a with the Date header every HTTP answer carries (RFC 9110),
// from the engine clock, so a client that takes the tracker's time from
// it follows the fake's clock and not the computer's. It is taken after
// the handler ran, so it is never before a time the answer holds.
func dated(a answer, now time.Time) answer {
	if a.header == nil {
		a.header = http.Header{}
	}
	a.header.Set("Date", now.UTC().Format(http.TimeFormat))
	return a
}

func (jiraService) rateLimited(e *Engine, x *exchange, retryAfter int) answer {
	reset := e.now().Add(time.Duration(retryAfter) * time.Second)
	return answer{status: http.StatusTooManyRequests, header: http.Header{
		"Content-Type":          {"application/json"},
		"Retry-After":           {strconv.Itoa(retryAfter)},
		"X-Ratelimit-Limit":     {"100"},
		"X-Ratelimit-Remaining": {"0"},
		"X-Ratelimit-Reset":     {reset.Format(time.RFC3339)},
		"Ratelimit-Reason":      {"jira-burst-based"},
	}}
}

func (jiraService) unhandledAnswer(detail string) answer {
	return jiraErrors(http.StatusNotImplemented, "trackerfake: unhandled: "+detail)
}

func (jiraService) routeNames() []string { return routeNames(jiraRoutes) }

// jiraErrors is Jira's error body with messages.
func jiraErrors(status int, messages ...string) answer {
	return jsonAnswer(status, map[string]any{"errorMessages": orEmpty(messages), "errors": map[string]string{}})
}

// jiraFieldErrors is Jira's error body with a message per field.
func jiraFieldErrors(status int, fields map[string]string) answer {
	return jsonAnswer(status, map[string]any{"errorMessages": []string{}, "errors": fields})
}

// decode reads the request's JSON body into out strictly: a member out
// does not declare is unhandled, as is a body that is not one JSON value.
// An empty body is unhandled unless optional.
func (c *jiraCall) decode(out any, optional bool) error {
	return decodeStrict(c.x.body, out, optional)
}

func decodeStrict(body []byte, out any, optional bool) error {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		if optional {
			return nil
		}
		return errors.New("the request has no JSON body")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("the request body: %w", err)
	}
	if dec.More() {
		return errors.New("the request body holds more than one JSON value")
	}
	return nil
}

// noBody refuses a body on a route that takes none.
func (c *jiraCall) noBody() error {
	if len(bytes.TrimSpace(c.x.body)) > 0 {
		return errors.New("the route takes no request body")
	}
	return nil
}

// intParam reads a non-negative integer query parameter, def when absent.
func (c *jiraCall) intParam(name string, def int) (int, error) {
	v := c.query.Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("query parameter %s=%q is not a count", name, v)
	}
	return n, nil
}

// issue is the issue the {issue} parameter names.
func (c *jiraCall) issue() *JiraIssue { return c.s.issue(c.params["issue"]) }

// touch stamps is as updated now and returns the time.
func (c *jiraCall) touch(is *JiraIssue) time.Time {
	now := c.e.now()
	is.Updated = now
	return now
}

// Jira pages two ways: by offset (startAt and maxResults, pageWindow),
// windowed by offsetWindow alone, and by token (nextPageToken),
// windowed by tokenWindow alone. Each takes every value the read
// matched, never a page of them.

// offsetWindow is the page of values from startAt, at most maxResults
// long, and whether it is the last.
func offsetWindow[T any](values []T, startAt, maxResults int) ([]T, bool) {
	end := min(startAt+maxResults, len(values))
	if startAt >= len(values) {
		return []T{}, true
	}
	return values[startAt:end], end >= len(values)
}

// offsetPage is Jira's offset-paged envelope over values, as the
// Agile and catalog lists write it.
func offsetPage[T any](values []T, startAt, maxResults int) map[string]any {
	page, last := offsetWindow(values, startAt, maxResults)
	return map[string]any{
		"startAt": startAt, "maxResults": maxResults, "total": len(values),
		"isLast": last, "values": page,
	}
}

// listPage is the envelope of a list Jira writes under its own key
// (comments, worklogs, issue types, fields), with no isLast.
func listPage[T any](key string, values []T, startAt, maxResults int) map[string]any {
	page, _ := offsetWindow(values, startAt, maxResults)
	return map[string]any{"startAt": startAt, "maxResults": maxResults, "total": len(values), key: page}
}

// tokenWindow is the page of values from offset, at most size long, and
// the token of the next page, empty for the last.
func tokenWindow[T any](values []T, scope string, offset, size int) ([]T, string) {
	page, last := offsetWindow(values, offset, size)
	if last {
		return page, ""
	}
	return page, pageToken(scope, offset+len(page))
}

// pageWindow reads startAt and maxResults, maxResults def when absent and
// at most most.
func (c *jiraCall) pageWindow(def, most int) (int, int, error) {
	start, err := c.intParam("startAt", 0)
	if err != nil {
		return 0, 0, err
	}
	size, err := c.intParam("maxResults", def)
	if err != nil {
		return 0, 0, err
	}
	return start, min(max(size, 1), most), nil
}
