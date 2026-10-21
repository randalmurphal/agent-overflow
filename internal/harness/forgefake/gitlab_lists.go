package forgefake

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The GitLab half of the project list's detail: the GraphQL reads of the
// open merge requests the viewer reviews or authored, the viewer's
// events, and a project's approval rules.

// gitlabGraphQL is GitLab's GraphQL operation table, dispatched as
// githubGraphQL is.
var gitlabGraphQL = []graphQLOp{
	{kind: "query", name: "PullsReviewing", required: []string{"fullPath", "viewer"}, optional: []string{"after"}, run: glabPullsRead("reviewer", false)},
	{kind: "query", name: "PullsAuthored", required: []string{"fullPath", "viewer"}, optional: []string{"after"}, run: glabPullsRead("author", false)},
	{kind: "query", name: "PullsReviewingActivity", required: []string{"fullPath", "viewer"}, optional: []string{"after"}, run: glabPullsRead("reviewer", true)},
	{kind: "query", name: "PullsAuthoredActivity", required: []string{"fullPath", "viewer"}, optional: []string{"after"}, run: glabPullsRead("author", true)},
}

// glabPullsRead answers project(fullPath) { mergeRequests(<filter>Username:
// $viewer, state: opened, sort: UPDATED_DESC, first: 100, after) } with the
// approval fields or, for activity, the discussion and commit fields. A
// project not on the host answers a null project, as GitLab does.
func glabPullsRead(filter string, activity bool) func(e *Engine, c *call, v graphQLVars) response {
	return func(e *Engine, c *call, v graphQLVars) response {
		fullPath, err1 := v.str("fullPath")
		viewer, err2 := v.str("viewer")
		offset, err3 := v.cursor("after")
		if err := firstErr(err1, err2, err3); err != nil {
			return unhandled("GitLab merge requests read: %v", err)
		}
		r := e.repoOn("gitlab", c.http.host, fullPath)
		if r == nil {
			return graphQLData(map[string]any{"project": nil})
		}
		var pulls []*Pull
		for i := range r.Pulls {
			p := &r.Pulls[i]
			if p.State != "open" {
				continue
			}
			match := strings.EqualFold(p.Author, viewer)
			if filter == "reviewer" {
				match = slices.ContainsFunc(p.Reviewers, func(login string) bool { return strings.EqualFold(login, viewer) })
			}
			if match {
				pulls = append(pulls, p)
			}
		}
		// UPDATED_DESC; fixture order (creation) breaks ties, newest first.
		slices.Reverse(pulls)
		slices.SortStableFunc(pulls, func(a, b *Pull) int { return cmp.Compare(rfc3339(b.UpdatedAt), rfc3339(a.UpdatedAt)) })
		nodes := make([]map[string]any, 0, len(pulls))
		for _, p := range pulls {
			if activity {
				nodes = append(nodes, gitlabActivityNode(p))
			} else {
				nodes = append(nodes, gitlabReviewNode(p))
			}
		}
		return graphQLData(map[string]any{"project": map[string]any{"mergeRequests": graphQLConnection(nodes, offset, githubGraphQLPageSize)}})
	}
}

// rfc3339 is a fixture time as unix nanoseconds (normalize validated it).
func rfc3339(value string) int64 {
	at, _ := time.Parse(time.RFC3339, value)
	return at.UnixNano()
}

// gitlabReviewNode is a merge request's approval fields: each reviewer's
// state (gitlabReviewState), who approved, the head pipeline.
func gitlabReviewNode(p *Pull) map[string]any {
	reviewers := []map[string]any{}
	for _, login := range p.Reviewers {
		reviewers = append(reviewers, map[string]any{
			"username":                login,
			"mergeRequestInteraction": map[string]any{"reviewState": strings.ToUpper(gitlabReviewState(p, login))},
		})
	}
	approved := []map[string]any{}
	for _, review := range p.Reviews {
		approved = append(approved, map[string]any{"username": review.Author})
	}
	var pipeline any
	if p.CI != nil {
		pipeline = map[string]any{"status": strings.ToUpper(p.CI.Status)}
	}
	return map[string]any{
		"iid":          strconv.Itoa(p.Number),
		"reviewers":    map[string]any{"nodes": reviewers},
		"approvedBy":   map[string]any{"nodes": approved},
		"headPipeline": pipeline,
	}
}

// gitlabReviewState is a reviewer's reviewState: approved while their
// approval stands, requested_changes for one in changeRequests, reviewed
// for one who commented on the merge request, else unreviewed.
func gitlabReviewState(p *Pull, login string) string {
	switch {
	case gitlabViewerApproved(p, login):
		return "approved"
	case slices.ContainsFunc(p.ChangeRequests, func(other string) bool { return strings.EqualFold(other, login) }):
		return "requested_changes"
	case len(gitlabCommentTimes(p, login)) > 0:
		return "reviewed"
	}
	return "unreviewed"
}

// gitlabCommentTimes are the times login commented on the merge request,
// in its conversation or its threads.
func gitlabCommentTimes(p *Pull, login string) []string {
	var times []string
	for _, comment := range p.Comments {
		if strings.EqualFold(comment.Author, login) {
			times = append(times, comment.CreatedAt)
		}
	}
	for _, thread := range p.Threads {
		for _, comment := range thread.Comments {
			if strings.EqualFold(comment.Author, login) {
				times = append(times, comment.CreatedAt)
			}
		}
	}
	return times
}

// gitlabActivityNode is a merge request's discussion and commit fields:
// its threads are its resolvable discussions.
func gitlabActivityNode(p *Pull) map[string]any {
	resolved := 0
	for _, thread := range p.Threads {
		if thread.Resolved {
			resolved++
		}
	}
	return map[string]any{
		"iid":                        strconv.Itoa(p.Number),
		"resolvableDiscussionsCount": len(p.Threads),
		"resolvedDiscussionsCount":   resolved,
		"commits":                    map[string]any{"nodes": []map[string]any{{"committedDate": p.CommittedAt}}},
	}
}

// gitlabViewerID is the viewer's account id (GET user answers it).
const gitlabViewerID = 1

// glabUserEvents answers GET users/ID/events?per_page=N[&page=N][&after=D]
// for the viewer: its comments on open and closed merge requests of every
// project on the host ("commented on" with the note's noteable) and its
// approvals ("approved"), newest first, created after the date. Another
// user is GitLab's 404.
func glabUserEvents(e *Engine, c *call, m []string) response {
	if m[1] != strconv.Itoa(gitlabViewerID) {
		return glabNotFound("User")
	}
	query, perPage, page, err := glabPage(m[2], "after")
	if err != nil {
		return unhandled("%v", err)
	}
	var after time.Time
	if raw := query.Get("after"); raw != "" {
		if after, err = time.Parse(time.DateOnly, raw); err != nil {
			return unhandled("after %q is not YYYY-MM-DD", raw)
		}
	}
	type event struct {
		at   string
		body map[string]any
	}
	var events []event
	for _, r := range e.repos {
		if r.Forge != "gitlab" || !strings.EqualFold(r.Host, c.http.host) {
			continue
		}
		for i := range r.Pulls {
			p := &r.Pulls[i]
			for _, at := range gitlabCommentTimes(p, e.viewer) {
				events = append(events, event{at: at, body: map[string]any{
					"project_id": r.ID, "action_name": "commented on", "target_type": "Note", "created_at": at,
					"author_username": e.viewer,
					"note":            map[string]any{"noteable_type": "MergeRequest", "noteable_iid": p.Number},
				}})
			}
			for _, review := range p.Reviews {
				if strings.EqualFold(review.Author, e.viewer) {
					events = append(events, event{at: review.SubmittedAt, body: map[string]any{
						"project_id": r.ID, "action_name": "approved", "target_type": "MergeRequest", "target_iid": p.Number,
						"created_at": review.SubmittedAt, "author_username": e.viewer,
					}})
				}
			}
		}
	}
	// GitLab's after is a date: events created after its first instant.
	events = slices.DeleteFunc(events, func(ev event) bool {
		at, _ := time.Parse(time.RFC3339, ev.at)
		return !after.IsZero() && !at.After(after)
	})
	slices.SortStableFunc(events, func(a, b event) int {
		if c := cmp.Compare(rfc3339(b.at), rfc3339(a.at)); c != 0 {
			return c
		}
		return cmp.Compare(a.body["project_id"].(int64), b.body["project_id"].(int64))
	})
	bodies := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		bodies = append(bodies, ev.body)
	}
	items, next := paginate(bodies, perPage, page)
	if items == nil {
		items = []map[string]any{}
	}
	return glabOK(jsonResponse(items), "X-Next-Page: "+next)
}

// glabApprovalRules answers GET projects/P/approval_rules[?per_page=N]
// with the project's rules (RepoMerge.ApprovalRules), one page.
func glabApprovalRules(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return fail
	}
	_, perPage, page, err := glabPage(m[2])
	if err != nil {
		return unhandled("%v", err)
	}
	rules := []map[string]any{}
	for i, required := range r.Merge.ApprovalRules {
		rules = append(rules, map[string]any{
			"id": r.ID*100 + int64(i) + 1, "name": "Rule " + strconv.Itoa(i+1), "rule_type": "regular", "approvals_required": required,
		})
	}
	items, next := paginate(rules, perPage, page)
	if items == nil {
		items = []map[string]any{}
	}
	return glabOK(jsonResponse(items), "X-Next-Page: "+next)
}
