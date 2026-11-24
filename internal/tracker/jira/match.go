package jira

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"agent-overflow/internal/tracker/trackerapi"
)

// The bounds of one jql/match request.
const (
	// MaxMatchIssues is the most issue IDs one MatchJQL takes.
	MaxMatchIssues = 50
	// MaxMatchQueries is the most JQL queries one MatchJQL takes.
	MaxMatchQueries = 10
)

// JQLMatch is one query's answer to MatchJQL: the issues of those asked
// that it matches, as decimal IDs in the order Jira gave them, and the
// errors Jira reported for the query. A query with errors says nothing
// of the issues it did not match.
type JQLMatch struct {
	Matched []string
	Errors  []string
}

// MatchJQL asks which of issues (numeric IDs, at most MaxMatchIssues)
// each of jqls (at most MaxMatchQueries) matches, in one request
// (POST /rest/api/3/jql/match), answered in the queries' order. Jira
// judges the issues as they are, not through its search index: measured,
// it matched an issue 0.3 s after its create while the search and a
// board's list by ID still missed it (docs/references/
// tracker-api-measurements.md). An ID that names no issue is matched by
// no query, with no error (measured). An answer that matches fewer than
// every issue asked and names no account is checked as an empty search
// is (prove), so an anonymous answer never passes for "not matched". A
// 400 to the match is a *QueryRefusedError; a failed sign-in check is
// not.
func (c *Client) MatchJQL(ctx context.Context, issues, jqls []string) ([]JQLMatch, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	switch {
	case len(issues) == 0 || len(issues) > MaxMatchIssues:
		return nil, notSent("a JQL match takes 1 to %d issues, not %d", MaxMatchIssues, len(issues))
	case len(jqls) == 0 || len(jqls) > MaxMatchQueries:
		return nil, notSent("a JQL match takes 1 to %d queries, not %d", MaxMatchQueries, len(jqls))
	}
	ids := make([]int64, len(issues))
	for i, ref := range issues {
		n, err := strconv.ParseInt(ref, 10, 64)
		if err != nil || n <= 0 || !isNumeric(ref) {
			return nil, notSent("%q is not an issue ID", ref)
		}
		ids[i] = n
	}
	for _, q := range jqls {
		if q == "" {
			return nil, notSent("a JQL match query is empty")
		}
	}
	var v struct {
		Matches []struct {
			Matched []int64  `json:"matchedIssues"`
			Errors  []string `json:"errors"`
		} `json:"matches"`
	}
	resp, err := c.call(ctx, trackerapi.Request{
		Method: http.MethodPost, Path: apiPath + "jql/match", ReadOnly: true,
		Body: map[string]any{"issueIds": ids, "jqls": jqls},
	}, &v)
	if err != nil {
		return nil, fmt.Errorf("jira: match %d issues against %d queries: %w", len(issues), len(jqls), refusedQuery(err))
	}
	if len(v.Matches) != len(jqls) {
		return nil, fmt.Errorf("jira: match against %d queries answered %d", len(jqls), len(v.Matches))
	}
	out := make([]JQLMatch, len(jqls))
	var left unnamed
	for i, m := range v.Matches {
		for _, id := range m.Matched {
			if !slices.Contains(ids, id) {
				return nil, fmt.Errorf("jira: match answered issue %d, which was not asked", id)
			}
			out[i].Matched = append(out[i].Matched, strconv.FormatInt(id, 10))
		}
		out[i].Errors = m.Errors
		if len(m.Errors) == 0 && len(m.Matched) < len(ids) {
			left.note(resp)
		}
	}
	if err := c.prove(ctx, "a JQL match", left); err != nil {
		return nil, err
	}
	return out, nil
}
