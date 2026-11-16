package jira

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"agent-overflow/internal/tracker/trackerapi"
)

// maxSearchPage is the most issues a search page asks for; Jira may
// return fewer when many fields are requested.
const maxSearchPage = 5000

// bulkFetchBatch is the most issues one bulk fetch asks for, the limit
// for any field list.
const bulkFetchBatch = 100

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

// SearchPage is one page of results. NextPageToken is empty on the last
// page; a token expires seven days after Jira hands it out.
type SearchPage struct {
	Issues        []*Issue
	NextPageToken string
	// Warnings are Jira's notes on the search, such as a clause cut at
	// its argument limit.
	Warnings []string
}

// Search runs one page of q on the enhanced search endpoint. Recent
// writes may not show yet.
func (c *Client) Search(ctx context.Context, q SearchQuery) (SearchPage, error) {
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
			Message string `json:"message"`
		} `json:"warnings"`
	}
	_, err := c.call(ctx, trackerapi.Request{
		Method: http.MethodPost, Path: apiPath + "search/jql", Body: body, ReadOnly: true,
	}, &v)
	if err != nil {
		return SearchPage{}, fmt.Errorf("jira: search: %w", err)
	}
	page := SearchPage{Issues: make([]*Issue, len(v.Issues))}
	for i, is := range v.Issues {
		page.Issues[i] = is.decode()
	}
	if !v.IsLast {
		page.NextPageToken = v.NextPageToken
	}
	for _, w := range v.Warnings {
		page.Warnings = append(page.Warnings, w.Message)
	}
	return page, nil
}

// SearchAll runs q from its first page (or q.PageToken) to the last, at
// most limit issues when limit is above zero. A failure after some pages
// returns their issues with the error; a token Jira hands out twice
// fails the search instead of looping.
func (c *Client) SearchAll(ctx context.Context, q SearchQuery, limit int) ([]*Issue, error) {
	var out []*Issue
	seen := map[string]bool{}
	for {
		if limit > 0 {
			q.MaxResults = min(pageSize(q.MaxResults, 100, maxSearchPage), limit-len(out))
		}
		page, err := c.Search(ctx, q)
		if err != nil {
			return out, err
		}
		out = append(out, page.Issues...)
		if limit > 0 && len(out) >= limit {
			return out[:limit], nil
		}
		if page.NextPageToken == "" {
			return out, nil
		}
		if seen[page.NextPageToken] {
			return out, fmt.Errorf("jira: search: the page token repeated after %d issues", len(out))
		}
		seen[page.NextPageToken] = true
		q.PageToken = page.NextPageToken
	}
}

// CountIssues returns Jira's approximate count of the issues jql matches.
// jql must be bounded.
func (c *Client) CountIssues(ctx context.Context, jql string) (int64, error) {
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

// BulkResult is what a bulk fetch returned: the issues, and the ones Jira
// could not return with its reason. An issue the user cannot see is in
// neither.
type BulkResult struct {
	Issues []*Issue
	Failed []BulkFailure
}

// BulkFailure is an issue a bulk fetch could not return.
type BulkFailure struct {
	ID      string
	Message string
}

// BulkFetch reads the issues named by key or ID with the fields named
// (none means Jira's navigable fields), 100 per request. A failure after
// some requests returns their issues with the error.
func (c *Client) BulkFetch(ctx context.Context, refs []string, fields ...string) (BulkResult, error) {
	for _, ref := range refs {
		if err := checkIssue(ref); err != nil {
			return BulkResult{}, err
		}
	}
	var out BulkResult
	for start := 0; start < len(refs); start += bulkFetchBatch {
		batch := refs[start:min(start+bulkFetchBatch, len(refs))]
		body := map[string]any{"issueIdsOrKeys": batch}
		if len(fields) > 0 {
			body["fields"] = fields
		}
		var v struct {
			Issues []issueJSON `json:"issues"`
			Errors []struct {
				ID      string `json:"id"`
				Message string `json:"errorMessage"`
			} `json:"issueErrors"`
		}
		_, err := c.call(ctx, trackerapi.Request{
			Method: http.MethodPost, Path: apiPath + "issue/bulkfetch", Body: body, ReadOnly: true,
		}, &v)
		if err != nil {
			return out, fmt.Errorf("jira: bulk fetch from %s: %w", batch[0], err)
		}
		for _, is := range v.Issues {
			out.Issues = append(out.Issues, is.decode())
		}
		for _, e := range v.Errors {
			out.Failed = append(out.Failed, BulkFailure(e))
		}
	}
	return out, nil
}

// QuoteJQL writes s as a JQL string literal, so text never reads as
// JQL: a backslash and a double quote are escaped, and a line break, a
// carriage return and a tab are written as \n, \r and \t. After the ~
// operator the literal is also a text search query, whose own syntax
// (wildcards, "-") is not escaped here.
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

// QuoteJQLList writes ss as a parenthesized list of JQL string literals,
// for the in and not in operators.
func QuoteJQLList(ss ...string) string {
	quoted := make([]string, len(ss))
	for i, s := range ss {
		quoted[i] = QuoteJQL(s)
	}
	return "(" + strings.Join(quoted, ", ") + ")"
}
