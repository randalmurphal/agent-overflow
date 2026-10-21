package forgefake

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The project list: GitHub's search over a repository's open pull
// requests (PullsSearch), and the list fields a pull carries for it and
// for GitLab's merge request list.

// githubOpenPullsQuery is the one search the app runs; any other query
// text is unhandled.
var githubOpenPullsQuery = regexp.MustCompile(`^is:pr is:open repo:([^\s/]+/[^\s/]+)$`)

// githubSearchNotVisible is GitHub's refusal of a search naming a
// repository the login cannot see.
const githubSearchNotVisible = "The listed users and repositories cannot be searched either because the resources do not exist or you do not have permission to view them."

// ghPullsSearch answers search(type: ISSUE, query: "is:pr is:open
// repo:O/R", first: 100, after:) with the repository's open pulls, newest
// first, a page of 100 from after, beside the viewer.
func ghPullsSearch(e *Engine, c *call, v graphQLVars) response {
	query, err1 := v.str("query")
	offset, err2 := v.cursor("after")
	if err := firstErr(err1, err2); err != nil {
		return unhandled("PullsSearch: %v", err)
	}
	m := githubOpenPullsQuery.FindStringSubmatch(query)
	if m == nil {
		return unhandled("PullsSearch query %q is not \"is:pr is:open repo:O/R\"", query)
	}
	viewer := map[string]any{"login": e.viewer}
	r := e.repoOn("github", c.http.host, m[1])
	if r == nil {
		return jsonResponse(map[string]any{
			"data":   map[string]any{"viewer": viewer, "search": nil},
			"errors": []map[string]any{{"path": []string{"search"}, "message": githubSearchNotVisible}},
		})
	}
	nodes := []map[string]any{}
	for i := len(r.Pulls) - 1; i >= 0; i-- {
		if p := &r.Pulls[i]; p.State == "open" {
			nodes = append(nodes, e.githubSearchPull(r, p))
		}
	}
	return graphQLData(map[string]any{"viewer": viewer, "search": graphQLConnection(nodes, offset, githubGraphQLPageSize)})
}

// githubSearchPull is one PullsSearch node. Callers hold mu.
func (e *Engine) githubSearchPull(r *Repo, p *Pull) map[string]any {
	var decision any
	if d := githubReviewDecision(r, p, e.viewer); d != "" {
		decision = d
	}
	requests := []map[string]any{}
	for _, login := range p.Reviewers {
		requests = append(requests, map[string]any{"requestedReviewer": map[string]any{"__typename": "User", "login": login}})
	}
	opinions := []map[string]any{}
	for _, review := range githubOpinions(p) {
		opinions = append(opinions, map[string]any{"viewerDidAuthor": strings.EqualFold(review.Author, e.viewer)})
	}
	var latest any
	for _, review := range p.Reviews {
		if strings.EqualFold(review.Author, e.viewer) && review.State != "PENDING" {
			latest = map[string]any{"state": review.State, "submittedAt": review.SubmittedAt}
		}
	}
	var rollup any
	if state := githubRollupState(p); state != "" {
		rollup = map[string]any{"state": state}
	}
	return map[string]any{
		"id":                       p.NodeID,
		"number":                   p.Number,
		"url":                      githubPullURL(r, p),
		"title":                    p.Title,
		"isDraft":                  p.Draft,
		"createdAt":                p.CreatedAt,
		"updatedAt":                p.UpdatedAt,
		"headRefOid":               p.HeadSHA,
		"headRefName":              p.HeadRef,
		"baseRefName":              p.BaseRef,
		"isCrossRepository":        p.HeadRepo != "",
		"author":                   githubActor(p.Author, p.AuthorName),
		"totalCommentsCount":       userNoteCount(p),
		"reviewDecision":           decision,
		"mergeStateStatus":         githubMergeStateStatus(r, p, e.viewer),
		"mergeable":                githubMergeable[p.Mergeable],
		"viewerDidAuthor":          strings.EqualFold(p.Author, e.viewer),
		"reviewRequests":           map[string]any{"nodes": requests},
		"latestOpinionatedReviews": map[string]any{"nodes": opinions},
		"viewerLatestReview":       latest,
		"commits": map[string]any{"totalCount": p.Commits, "nodes": []map[string]any{{"commit": map[string]any{
			"committedDate": p.CommittedAt, "statusCheckRollup": rollup,
		}}}},
	}
}

// githubRollupState is the head commit's StatusState over its checks: a
// failed or cancelled job is FAILURE, a live one PENDING, a check the
// base requires with no job EXPECTED, else SUCCESS; no checks is "".
func githubRollupState(p *Pull) string {
	if p.CI == nil || len(p.CI.Jobs) == 0 {
		if len(p.Readiness.RequiredChecks) > 0 {
			return "EXPECTED"
		}
		return ""
	}
	state := "SUCCESS"
	for _, job := range p.CI.Jobs {
		switch job.Status {
		case "failed", "canceled":
			return "FAILURE"
		case "running", "pending":
			state = "PENDING"
		}
	}
	for _, name := range p.Readiness.RequiredChecks {
		if p.ciJob(name) == nil && state == "SUCCESS" {
			state = "EXPECTED"
		}
	}
	return state
}

// normalizeList validates and fills the fields the project lists read.
func (p *Pull) normalizeList() error {
	for i, login := range p.Reviewers {
		if strings.TrimSpace(login) == "" || slices.ContainsFunc(p.Reviewers[:i], func(other string) bool { return strings.EqualFold(other, login) }) {
			return fmt.Errorf("reviewers[%d]: %q is empty or listed twice", i, login)
		}
	}
	p.CreatedAt = defaultString(p.CreatedAt, "2026-01-01T00:00:00Z")
	p.UpdatedAt = defaultString(p.UpdatedAt, p.CreatedAt)
	p.CommittedAt = defaultString(p.CommittedAt, p.CreatedAt)
	for name, value := range map[string]string{"createdAt": p.CreatedAt, "updatedAt": p.UpdatedAt, "committedAt": p.CommittedAt} {
		if _, err := time.Parse(time.RFC3339, value); err != nil {
			return fmt.Errorf("%s %q is not RFC 3339", name, value)
		}
	}
	switch {
	case p.Commits < 0:
		return errors.New("commits is negative")
	case p.Commits == 0:
		p.Commits = 1
	}
	return nil
}
