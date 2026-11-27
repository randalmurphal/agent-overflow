package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// maxSearchPage is the most issues a search page asks for; Jira may
// return fewer when many fields are requested.
const maxSearchPage = 5000

// bulkFetchCapacity is the most issues one bulk fetch of fields may ask
// for, as Jira documents it (docs/references/tracker-api-measurements.md):
// 1000 when fields names 1 to 100 fields, none a wildcard (*all,
// *navigable), an exclusion (-id) or a field of many values (comment,
// worklog, attachment); 100 otherwise, and Jira refuses more with a 400.
// The rule's other condition, no heavy expand, holds: BulkFetch sends no
// expand. Measured: 101 issues with no fields or with comment refused,
// 250 with the card fields (labels and the sprint field among them)
// returned whole.
func bulkFetchCapacity(fields []string) int {
	if len(fields) == 0 || len(fields) > 100 {
		return 100
	}
	for _, f := range fields {
		switch {
		case strings.HasPrefix(f, "*"), strings.HasPrefix(f, "-"), f == "comment", f == "worklog", f == "attachment":
			return 100
		}
	}
	return 1000
}

// bulkFetchBatch is the most issues one bulk fetch asks for whatever
// Jira takes: 250 issues with the card fields measured 912 KB, about
// 3.6 KB an issue, so a batch of 1000 nears 4 MB and a site of heavier
// cards (long sprint histories) could pass the transport's 8 MiB answer
// bound, failing the read every time instead of costing one request
// more. 500, the size of a delta row page, keeps a batch near 1.8 MB.
const bulkFetchBatch = 500

// SearchQuery is one page of a JQL search.
type SearchQuery struct {
	// JQL must be bounded: it restricts the search (a project, an
	// assignee, a date), not only orders it. Build it with QuoteJQL.
	JQL string
	// Fields are the field IDs each issue carries (or *all, *navigable,
	// and -ID exclusions). None returns IDs and keys only.
	Fields []string
	// MaxResults bounds the page; zero means 100.
	MaxResults int
	// PageToken continues a search from the page that named it.
	PageToken string
}

// SearchWarning is Jira's note on a search, such as a clause cut at its
// argument limit or results truncated. Type is Jira's code
// (CLAUSE_LIMIT_EXCEEDED); Jira marks warnings experimental, so new
// types may appear.
type SearchWarning struct {
	Type    string
	Message string
}

// SearchPage is one page of results. NextPageToken is empty on the last
// page; a token expires seven days after Jira hands it out. Date is
// Jira's clock when it answered the page, zero when the answer named
// none.
type SearchPage struct {
	Issues        []*Issue
	NextPageToken string
	Warnings      []SearchWarning
	Date          time.Time
}

// Search runs one page of q on the enhanced search endpoint. Recent
// writes may not show yet. Jira's 400 to the search is a
// *QueryRefusedError.
func (c *Client) Search(ctx context.Context, q SearchQuery) (SearchPage, error) {
	if err := c.admit(ctx); err != nil {
		return SearchPage{}, err
	}
	if strings.TrimSpace(q.JQL) == "" {
		return SearchPage{}, notSent("a search needs JQL")
	}
	if q.MaxResults < 0 {
		return SearchPage{}, notSent("a search page of %d issues", q.MaxResults)
	}
	body := map[string]any{"jql": q.JQL, "maxResults": pageSize(q.MaxResults, 100, maxSearchPage)}
	if len(q.Fields) > 0 {
		body["fields"] = q.Fields
	}
	if q.PageToken != "" {
		body["nextPageToken"] = q.PageToken
	}
	var v struct {
		Issues        []issueJSON `json:"issues"`
		NextPageToken string      `json:"nextPageToken"`
		IsLast        bool        `json:"isLast"`
		Warnings      []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"warnings"`
	}
	resp, err := c.call(ctx, trackerapi.Request{
		Method: http.MethodPost, Path: apiPath + "search/jql", Body: body, ReadOnly: true,
	}, &v)
	if err != nil {
		return SearchPage{}, fmt.Errorf("jira: search: %w", refusedQuery(err))
	}
	if len(v.Issues) == 0 {
		var u unnamed
		u.note(resp)
		if err := c.prove(ctx, "a search", u); err != nil {
			return SearchPage{}, err
		}
	}
	page := SearchPage{Issues: make([]*Issue, len(v.Issues)), Date: answerDate(resp)}
	for i, is := range v.Issues {
		page.Issues[i] = is.decode()
	}
	if page.NextPageToken, err = nextToken(v.IsLast, v.NextPageToken); err != nil {
		return SearchPage{}, fmt.Errorf("jira: search: %w", err)
	}
	for _, w := range v.Warnings {
		page.Warnings = append(page.Warnings, SearchWarning(w))
	}
	return page, nil
}

// nextToken is the token of the page after a token-paged one: none
// after the last, and an error for a page that is not the last but names
// no next one, which would otherwise read as the end.
func nextToken(isLast bool, token string) (string, error) {
	switch {
	case isLast:
		return "", nil
	case token == "":
		return "", &AnswerError{Message: "a page that is not the last names no next page"}
	}
	return token, nil
}

// SearchResult is what a search run to its end returned. Complete is
// false when the limit cut the search or Jira attached any warning, since
// a warning may say the results were cut; Warnings are every page's. Date
// is the first page's.
type SearchResult struct {
	Issues   []*Issue
	Warnings []SearchWarning
	Complete bool
	Date     time.Time
}

// SearchAll runs q from its first page (or q.PageToken) to the last, at
// most limit issues when limit is above zero. A failure after some pages
// returns their issues and warnings with the error; a token Jira hands
// out twice, or an empty page that names a next one, fails the search
// instead of looping. The pages are one walk (walk.go).
func (c *Client) SearchAll(ctx context.Context, q SearchQuery, limit int) (SearchResult, error) {
	size := pageSize(q.MaxResults, 100, maxSearchPage)
	first := q.PageToken
	return trackerapi.Walk(ctx, func(ctx context.Context) (SearchResult, error) {
		return tokenPages("search", first, limit, func(token string, room int) (SearchPage, error) {
			q.PageToken, q.MaxResults = token, size
			if room > 0 {
				q.MaxResults = min(size, room)
			}
			return c.Search(ctx, q)
		})
	})
}

// tokenPages reads the pages next returns, each continuing from the
// token of the one before and starting from first, until a page names no
// next one, or limit issues when limit is above zero; room is how many
// issues the limit leaves, zero for none. It is the one walk of every
// token-paged issue list.
func tokenPages(what, first string, limit int, next func(token string, room int) (SearchPage, error)) (SearchResult, error) {
	var out SearchResult
	seen := map[string]bool{}
	for token := first; ; {
		room := 0
		if limit > 0 {
			room = limit - len(out.Issues)
		}
		page, err := next(token, room)
		if err != nil {
			return out, err
		}
		if token == first {
			out.Date = page.Date
		}
		out.Issues = append(out.Issues, page.Issues...)
		out.Warnings = append(out.Warnings, page.Warnings...)
		cut := limit > 0 && len(out.Issues) > limit
		if cut {
			out.Issues = out.Issues[:limit]
		}
		switch {
		case page.NextPageToken == "":
			out.Complete = !cut && len(out.Warnings) == 0
			return out, nil
		case limit > 0 && len(out.Issues) >= limit:
			return out, nil
		case len(page.Issues) == 0:
			return out, fmt.Errorf("jira: %s: an empty page names a next page after %d issues", what, len(out.Issues))
		case seen[page.NextPageToken]:
			return out, fmt.Errorf("jira: %s: the page token repeated after %d issues", what, len(out.Issues))
		}
		seen[page.NextPageToken] = true
		token = page.NextPageToken
	}
}

// CountIssues returns Jira's approximate count of the issues jql matches.
// jql must be bounded.
func (c *Client) CountIssues(ctx context.Context, jql string) (int64, error) {
	if err := c.admit(ctx); err != nil {
		return 0, err
	}
	if strings.TrimSpace(jql) == "" {
		return 0, notSent("a count needs JQL")
	}
	var v struct {
		Count int64 `json:"count"`
	}
	_, err := c.call(ctx, trackerapi.Request{
		Method: http.MethodPost, Path: apiPath + "search/approximate-count",
		Body: map[string]string{"jql": jql}, ReadOnly: true,
	}, &v)
	if err != nil {
		return 0, fmt.Errorf("jira: count issues: %w", err)
	}
	return v.Count, nil
}

// BulkResult is what a bulk fetch returned: the issues, each once, and
// the ones Jira could not return with its reason. An issue the user
// cannot see, or that does not exist, is in neither. Complete reports
// that nothing failed and each distinct reference brought back an issue
// of its own, so it is false when one is missing or failed, and when two
// references (an ID and a key) name the same issue.
type BulkResult struct {
	Issues   []*Issue
	Failed   []BulkFailure
	Complete bool
}

// BulkFailure is an issue a bulk fetch could not return.
type BulkFailure struct {
	ID      string
	Message string
}

// BulkFetch reads the issues named by key or ID with the fields named
// (none means Jira's navigable fields), per request the fewer of
// bulkFetchBatch and as many as Jira takes with those fields
// (bulkFetchCapacity). A reference named again, a key in any case, is
// sent once. A failure after some requests returns their issues with the
// error.
func (c *Client) BulkFetch(ctx context.Context, refs []string, fields ...string) (BulkResult, error) {
	if err := c.admit(ctx); err != nil {
		return BulkResult{}, err
	}
	return trackerapi.Walk(ctx, func(ctx context.Context) (BulkResult, error) { return c.bulkFetch(ctx, refs, fields, false) })
}

// BulkFetchByID is BulkFetch of issue IDs alone, each answer checked as
// it is decoded, before anything is collapsed: an issue with no ID, with
// an ID its request did not ask, or answered twice is a malformed answer
// and fails the call, so an issue asked never reads as absent because
// the answer held another in its place.
func (c *Client) BulkFetchByID(ctx context.Context, ids []string, fields ...string) (BulkResult, error) {
	if err := c.admit(ctx); err != nil {
		return BulkResult{}, err
	}
	for _, id := range ids {
		if err := checkID("issue", id); err != nil {
			return BulkResult{}, err
		}
	}
	return trackerapi.Walk(ctx, func(ctx context.Context) (BulkResult, error) { return c.bulkFetch(ctx, ids, fields, true) })
}

// bulkFetch is one walk of BulkFetch, of BulkFetchByID when byID.
func (c *Client) bulkFetch(ctx context.Context, refs, fields []string, byID bool) (BulkResult, error) {
	returned := map[string]bool{}
	var out BulkResult
	asked, failed, left, err := bulkFetchEach(ctx, c, refs, fields, byID, func(issues []issueJSON, resp *trackerapi.Response, epoch uint64) {
		for _, is := range issues {
			got := is.decode()
			if got.Project != nil {
				c.keepProjects(epoch, resp.CredentialMark(), *got.Project)
			}
			if got.ID == "" || !returned[got.ID] {
				returned[got.ID] = true
				out.Issues = append(out.Issues, got)
			}
		}
	})
	out.Failed = failed
	if err != nil {
		return out, err
	}
	if err := c.prove(ctx, "a bulk fetch", left); err != nil {
		return BulkResult{}, err
	}
	out.Complete = len(out.Failed) == 0 && len(out.Issues) == asked
	return out, nil
}

// bulkIssue is an issue of a bulk fetch's answer as one reader decodes
// it: whatever it keeps, it names the issue's ID.
type bulkIssue interface{ issueID() string }

func (v issueJSON) issueID() string { return v.ID }

// bulkFetchEach is one walk of a bulk fetch of refs with fields, each
// answer's issues decoded as T, the one walk every bulk fetch makes: refs
// checked and each sent once (a key in any case), in batches of the fewer
// of bulkFetchBatch and as many as Jira takes with fields
// (bulkFetchCapacity), each answer checked (bulkAnswer.check, and with
// byID checkAnswered) before each is given its issues, with the answer and
// the projects cache's epoch from before the request. It returns how many
// distinct references it asked, the issues Jira could not return, and the
// answers that left out issues they were asked for, which the caller
// proves the user's (Client.prove) before it reads an absence as one; a
// failure after some requests returns what was given each so far with
// the error.
func bulkFetchEach[T bulkIssue](ctx context.Context, c *Client, refs, fields []string, byID bool,
	each func(issues []T, resp *trackerapi.Response, epoch uint64)) (asked int, failed []BulkFailure, left unnamed, err error) {
	named := make(map[string]bool, len(refs))
	distinct := make([]string, 0, len(refs))
	for _, ref := range refs {
		if err := checkIssue(ref); err != nil {
			return 0, nil, nil, err
		}
		if k := strings.ToUpper(ref); !named[k] {
			named[k] = true
			distinct = append(distinct, ref)
		}
	}
	refs = distinct
	size := min(bulkFetchCapacity(fields), bulkFetchBatch)
	for start := 0; start < len(refs); start += size {
		batch := refs[start:min(start+size, len(refs))]
		body := map[string]any{"issueIdsOrKeys": batch}
		if len(fields) > 0 {
			body["fields"] = fields
		}
		var v bulkAnswer[T]
		epoch := c.projects.epoch()
		resp, err := c.call(ctx, trackerapi.Request{
			Method: http.MethodPost, Path: apiPath + "issue/bulkfetch", Body: body, ReadOnly: true,
		}, &v)
		if err == nil {
			err = v.check()
		}
		if err != nil {
			return len(refs), failed, left, fmt.Errorf("jira: bulk fetch from %s: %w", batch[0], err)
		}
		issues, errs := *v.Issues, *v.Errors
		if len(issues)+len(errs) < len(batch) {
			left.note(resp)
		}
		if byID {
			if err := checkAnswered(batch, issues); err != nil {
				return len(refs), failed, left, fmt.Errorf("jira: bulk fetch from %s: %w", batch[0], err)
			}
		}
		each(issues, resp, epoch)
		for _, e := range errs {
			failed = append(failed, BulkFailure(e))
		}
	}
	return len(refs), failed, left, nil
}

// bulkAnswer is a bulk fetch's answer: the issues returned and those Jira
// could not return. An issue in neither does not exist or is hidden from
// the user, so both lists are what an absence is read from.
type bulkAnswer[T any] struct {
	Issues *[]T `json:"issues"`
	Errors *[]struct {
		ID      string `json:"id"`
		Message string `json:"errorMessage"`
	} `json:"issueErrors"`
}

// check refuses an answer missing either list, or holding null for it, as
// a malformed answer: measured, Jira answers both, as arrays, even when
// no issue asked exists, so an answer without them (an empty object, an
// error envelope) says nothing of the issues asked and must not read as
// every one absent.
func (a bulkAnswer[T]) check() error {
	switch {
	case a.Issues == nil:
		return &AnswerError{Message: "malformed answer: a bulk fetch answered with no issues list"}
	case a.Errors == nil:
		return &AnswerError{Message: "malformed answer: a bulk fetch answered with no issueErrors list"}
	}
	return nil
}

// checkAnswered refuses issues, the answer to a bulk fetch of the IDs
// asked, when one has no ID, an ID not asked, or an ID answered before.
func checkAnswered[T bulkIssue](asked []string, issues []T) error {
	seen := make(map[string]bool, len(issues))
	for _, is := range issues {
		id := is.issueID()
		switch {
		case id == "":
			return errors.New("malformed answer: an issue with no ID")
		case !slices.Contains(asked, id):
			return fmt.Errorf("malformed answer: issue %s, which was not asked", id)
		case seen[id]:
			return fmt.Errorf("malformed answer: issue %s twice", id)
		}
		seen[id] = true
	}
	return nil
}

// QuoteJQL writes s as a JQL string literal, so text never reads as
// JQL: a backslash and a double quote are escaped, and a line break, a
// carriage return and a tab are written as \n, \r and \t. After the ~
// operator the literal is also a text search query, whose own syntax
// (wildcards, "-") is not escaped here; QuoteJQLText escapes it.
func QuoteJQL(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// textSyntax are the characters Jira's text search reads as syntax after
// the ~ operator: the operators + - && || !, grouping ( ), phrase quotes,
// the wildcards * and ?, fuzzy ~ and boost ^ (ignored by Jira Cloud but
// still parsed), and the range { } [ ], field : and regular expression /
// delimiters of the Lucene syntax it descends from, with the backslash
// that escapes them. Source: Atlassian, "Search for work items using the
// text field" and "What is advanced search in Jira Cloud?", Restricted
// words and characters (support.atlassian.com, read 2026-11-19).
const textSyntax = `+-&|!(){}[]^"~*?:\/`

// Jira's text search reads the words AND, OR and NOT as operators only
// in capitals, and its terms ignore case (lowerTextOperators).

// QuoteJQLText writes s as the JQL string literal of a text search, the
// value after the ~ operator, so typed text is searched for as words and
// never acts as a wildcard or an operator: each character textSyntax holds
// is escaped with a backslash, the words AND, OR and NOT are written in
// lower case, which finds the same words, and the result is quoted with
// QuoteJQL, which escapes those backslashes again for JQL.
func QuoteJQLText(s string) string {
	s = lowerTextOperators(s)
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		if strings.ContainsRune(textSyntax, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return QuoteJQL(b.String())
}

// QuoteJQLList writes ss as a parenthesized list of JQL string literals,
// for the in and not in operators.
func QuoteJQLList(ss ...string) string {
	quoted := make([]string, len(ss))
	for i, s := range ss {
		quoted[i] = QuoteJQL(s)
	}
	return "(" + strings.Join(quoted, ", ") + ")"
}
