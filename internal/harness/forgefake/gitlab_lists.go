package forgefake

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The GitLab half of the project list's detail: the GraphQL reads of the
// open merge requests the viewer reviews or authored, and the viewer's
// events.

// gitlabGraphQL is GitLab's GraphQL operation table, dispatched as
// githubGraphQL is.
var gitlabGraphQL = []graphQLOp{
	{kind: "query", name: "PullsReviewing", required: []string{"fullPath", "viewer"}, optional: []string{"after"}, run: glabPullsRead("reviewer", gitlabReviewNode)},
	{kind: "query", name: "PullsAuthored", required: []string{"fullPath", "viewer"}, optional: []string{"after"}, run: glabPullsRead("author", gitlabReviewNode)},
	{kind: "query", name: "PullsReviewingActivity", required: []string{"fullPath", "viewer"}, optional: []string{"after"}, run: glabPullsRead("reviewer", gitlabActivityNode)},
	{kind: "query", name: "PullsAuthoredActivity", required: []string{"fullPath", "viewer"}, optional: []string{"after"}, run: glabPullsRead("author", gitlabActivityNode)},
	{kind: "query", name: "PullsReviewingApprovals", required: []string{"fullPath", "viewer"}, optional: []string{"after"}, run: glabPullsRead("reviewer", gitlabApprovalsNode)},
	{kind: "query", name: "PullsAuthoredApprovals", required: []string{"fullPath", "viewer"}, optional: []string{"after"}, run: glabPullsRead("author", gitlabApprovalsNode)},
}

// glabPullsRead answers project(fullPath) { mergeRequests(<filter>Username:
// $viewer, state: opened, sort: UPDATED_DESC, first: 100, after) } with
// the nodes node builds: the review, the discussion and commit, or the
// approval rule fields. A project not on the host answers a null
// project, as GitLab does.
func glabPullsRead(filter string, node func(*Pull) map[string]any) func(e *Engine, c *call, v graphQLVars) response {
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
			nodes = append(nodes, node(p))
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
	case len(gitlabComments(p, login)) > 0:
		return "reviewed"
	}
	return "unreviewed"
}

// gitlabComment is one note login wrote on a merge request: its time and
// the event target type GitLab files it under (Note for a conversation
// comment, DiffNote for a comment in a thread on the diff, every fixture
// thread having a path).
type gitlabComment struct {
	at, targetType string
}

// gitlabComments are the notes login wrote on the merge request, in its
// conversation and its threads.
func gitlabComments(p *Pull, login string) []gitlabComment {
	var out []gitlabComment
	for _, comment := range p.Comments {
		if strings.EqualFold(comment.Author, login) {
			out = append(out, gitlabComment{at: comment.CreatedAt, targetType: "Note"})
		}
	}
	for _, thread := range p.Threads {
		for _, comment := range thread.Comments {
			if strings.EqualFold(comment.Author, login) {
				out = append(out, gitlabComment{at: comment.CreatedAt, targetType: "DiffNote"})
			}
		}
	}
	return out
}

// gitlabApprovalsNode is a merge request's approval rules: one
// any-approver rule requiring the pull's readiness.requiredApprovals,
// the rule a project with no other rules has.
func gitlabApprovalsNode(p *Pull) map[string]any {
	return map[string]any{
		"iid": strconv.Itoa(p.Number),
		"approvalState": map[string]any{"rules": []map[string]any{
			{"type": "ANY_APPROVER", "approvalsRequired": p.Readiness.RequiredApprovals},
		}},
	}
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

// glabUserEvents answers GET
// users/ID/events?per_page=N[&page=N][&after=D][&action=A] for the viewer:
// its comments on open and closed merge requests of every project on the
// host ("commented on" with the note's noteable) and its approvals
// ("approved"), newest first, created after the date; action commented or
// approved keeps those alone, as GitLab filters the feed. Another user is
// GitLab's 404; another action is unhandled.
func glabUserEvents(e *Engine, c *call, m []string) response {
	if m[1] != strconv.Itoa(gitlabViewerID) {
		return glabNotFound("User")
	}
	query, perPage, page, err := glabPage(m[2], "after", "action")
	if err != nil {
		return unhandled("%v", err)
	}
	actions := map[string]string{"commented": "commented on", "approved": "approved"}
	keep, filtered := "", query.Has("action")
	if filtered {
		var ok bool
		if keep, ok = actions[query.Get("action")]; !ok {
			return unhandled("events action %q is not implemented", query.Get("action"))
		}
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
			for _, comment := range gitlabComments(p, e.viewer) {
				note := map[string]any{"noteable_type": "MergeRequest", "noteable_iid": p.Number, "type": nil}
				if comment.targetType == "DiffNote" {
					note["type"] = "DiffNote"
				}
				events = append(events, event{at: comment.at, body: map[string]any{
					"project_id": r.ID, "action_name": "commented on", "target_type": comment.targetType, "created_at": comment.at,
					"author_username": e.viewer, "note": note,
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
		return (!after.IsZero() && !at.After(after)) || (filtered && ev.body["action_name"] != keep)
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
