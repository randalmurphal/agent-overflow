package forgefake

import (
	"cmp"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// The reads of a repository's pull request history (internal/git
// pr_history*.go): GitHub's PullHistory and PullHistoryMore, and GitLab's
// project events, beside the merge request list's update-time filters
// (glabMRList).

// githubHistoryActor is an act's or a pull's author as GitHub answers
// `author { __typename login ... on User { name } }`: a seeded login with
// no display name is a bot, as the fixture's comments and reviews read.
func githubHistoryActor(login, name string) map[string]any {
	actor := githubActor(login, name)
	actor["__typename"] = "User"
	if name == "" {
		actor["__typename"] = "Bot"
	}
	return actor
}

// githubHistoryReviews is a pull's APPROVED reviews, or its COMMENTED and
// CHANGES_REQUESTED ones, oldest first, as the `states` argument selects
// them. An approval carries the count of its line comments, the thread
// comments that name it (Comment.Review), as `comments(first: 1) {
// totalCount }` answers it.
func githubHistoryReviews(p *Pull, approvals bool) []map[string]any {
	out := []map[string]any{}
	for _, review := range p.Reviews {
		if approvals && review.State != "APPROVED" || !approvals && review.State != "COMMENTED" && review.State != "CHANGES_REQUESTED" {
			continue
		}
		node := map[string]any{"submittedAt": review.SubmittedAt, "author": githubHistoryActor(review.Author, review.AuthorName)}
		if approvals {
			count := 0
			for _, thread := range p.Threads {
				for _, comment := range thread.Comments {
					if comment.Review == review.ID {
						count++
					}
				}
			}
			node["comments"] = map[string]any{"totalCount": count}
		}
		out = append(out, node)
	}
	return out
}

// githubHistoryComments is a pull's conversation comments, oldest first.
func githubHistoryComments(p *Pull) []map[string]any {
	out := make([]map[string]any, 0, len(p.Comments))
	for _, comment := range p.Comments {
		out = append(out, map[string]any{"createdAt": comment.CreatedAt, "author": githubHistoryActor(comment.Author, comment.AuthorName)})
	}
	return out
}

// historyOrder is r's pulls by update time, newest first, a tie newest
// in fixture order first.
func historyOrder(r *Repo) []*Pull {
	pulls := make([]*Pull, 0, len(r.Pulls))
	for i := len(r.Pulls) - 1; i >= 0; i-- {
		pulls = append(pulls, &r.Pulls[i])
	}
	slices.SortStableFunc(pulls, func(a, b *Pull) int { return cmp.Compare(rfc3339(b.UpdatedAt), rfc3339(a.UpdatedAt)) })
	return pulls
}

// githubHistoryPull is a pull's own fields as PullHistory selects them.
func (e *Engine) githubHistoryPull(r *Repo, p *Pull) map[string]any {
	var mergedAt, closedAt any
	if p.State == "merged" {
		mergedAt = p.MergedAt
	}
	if defaultString(p.State, "open") != "open" {
		closedAt = p.ClosedAt
	}
	return map[string]any{
		"number": p.Number, "url": githubPullURL(r, p), "title": p.Title, "state": strings.ToUpper(defaultString(p.State, "open")),
		"isDraft": p.Draft, "headRefName": p.HeadRef, "headRepository": e.githubHeadRepository(r, p),
		"createdAt": p.CreatedAt, "updatedAt": p.UpdatedAt, "mergedAt": mergedAt, "closedAt": closedAt,
		"author": githubHistoryActor(p.Author, p.AuthorName),
	}
}

// ghPullHistory answers a page of 50 of the repository's pulls, most
// recently updated first, each with its newest 50 COMMENTED and
// CHANGES_REQUESTED reviews, its first 3 approvals and its newest 50
// conversation comments, beside the repository's node id and parent and
// the viewer's login.
func ghPullHistory(e *Engine, c *call, v graphQLVars) response {
	owner, err1 := v.str("owner")
	name, err2 := v.str("name")
	offset, err3 := v.cursor("after")
	if err := firstErr(err1, err2, err3); err != nil {
		return unhandled("PullHistory: %v", err)
	}
	r := e.repoOn("github", c.http.host, owner+"/"+name)
	if r == nil {
		return graphQLNotFound(map[string]any{"viewer": map[string]any{"login": e.viewer}, "repository": nil}, []string{"repository"},
			fmt.Sprintf("Could not resolve to a Repository with the name '%s/%s'.", owner, name))
	}
	nodes := []map[string]any{}
	for _, p := range historyOrder(r) {
		node := e.githubHistoryPull(r, p)
		reviews, comments := githubHistoryReviews(p, false), githubHistoryComments(p)
		node["reviews"] = graphQLLast(reviews, len(reviews), 50)
		node["approvals"] = graphQLConnection(githubHistoryReviews(p, true), 0, 3)
		node["comments"] = graphQLLast(comments, len(comments), 50)
		nodes = append(nodes, node)
	}
	repository := e.githubRepoParent(r)
	repository["pullRequests"] = graphQLConnection(nodes, offset, 50)
	return graphQLData(map[string]any{"viewer": map[string]any{"login": e.viewer}, "repository": repository})
}

// ghPullHistoryMore answers, of one pull, each kind of act its want
// variable names, 100 a page: its reviews and conversation comments
// before their cursors (from the newest without one), its approvals after
// theirs.
func ghPullHistoryMore(e *Engine, c *call, v graphQLVars) response {
	project, number, err := v.pullVars()
	wantReviews, err1 := v.boolean("wantReviews")
	wantApprovals, err2 := v.boolean("wantApprovals")
	wantComments, err3 := v.boolean("wantComments")
	approvalsAfter, err4 := v.cursor("approvalsAfter")
	if err := firstErr(err, err1, err2, err3, err4); err != nil {
		return unhandled("PullHistoryMore: %v", err)
	}
	_, p, fail, ok := e.ghGraphQLPull(c, project, number, nil)
	if !ok {
		return fail
	}
	pull := map[string]any{}
	if wantReviews {
		reviews := githubHistoryReviews(p, false)
		before, err := v.before("reviewsBefore", len(reviews))
		if err != nil {
			return unhandled("PullHistoryMore: %v", err)
		}
		pull["reviews"] = graphQLLast(reviews, before, 100)
	}
	if wantApprovals {
		pull["approvals"] = graphQLConnection(githubHistoryReviews(p, true), approvalsAfter, 100)
	}
	if wantComments {
		comments := githubHistoryComments(p)
		before, err := v.before("commentsBefore", len(comments))
		if err != nil {
			return unhandled("PullHistoryMore: %v", err)
		}
		pull["comments"] = graphQLLast(comments, before, 100)
	}
	return graphQLData(map[string]any{"repository": map[string]any{"pullRequest": pull}})
}

// glabProjectEvents answers a project's approved or commented events,
// newest first or oldest first (sort), created after or before a date
// (after the date's last instant, before its first: GitLab's dates), in
// pages with X-Next-Page. An approval is each APPROVED review; a comment
// each conversation and thread comment, with its note on the merge
// request. Each event's id is its review's or comment's. A project whose
// EventsHidden is set drops each event from the page it selected, as
// GitLab does with events the reader may not see.
func glabProjectEvents(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return fail
	}
	query, perPage, page, err := glabPage(m[2], "action", "after", "before", "sort")
	if err != nil {
		return unhandled("%v", err)
	}
	action := query.Get("action")
	if action != "approved" && action != "commented" {
		return unhandled("project events action %q is not implemented", action)
	}
	sortOrder := query.Get("sort")
	if sortOrder != "" && sortOrder != "asc" && sortOrder != "desc" {
		return unhandled("sort=%q is not asc or desc", sortOrder)
	}
	dates := map[string]time.Time{}
	for _, key := range []string{"after", "before"} {
		if raw := query.Get(key); raw != "" {
			day, err := time.Parse(time.DateOnly, raw)
			if err != nil {
				return unhandled("%s %q is not YYYY-MM-DD", key, raw)
			}
			dates[key] = day
		}
	}
	type event struct {
		id   int64
		at   time.Time
		body map[string]any
	}
	var events []event
	add := func(id int64, at, login, name string, body map[string]any) {
		t, _ := time.Parse(time.RFC3339, at)
		if after, ok := dates["after"]; ok && t.Before(after.AddDate(0, 0, 1)) {
			return
		}
		if before, ok := dates["before"]; ok && !t.Before(before) {
			return
		}
		body["id"], body["created_at"], body["project_id"] = id, at, r.ID
		body["author"], body["author_username"] = gitlabUser(login, name), login
		events = append(events, event{id: id, at: t, body: body})
	}
	for i := range r.Pulls {
		p := &r.Pulls[i]
		if action == "approved" {
			for _, review := range p.Reviews {
				if review.State == "APPROVED" {
					add(review.ID, review.SubmittedAt, review.Author, review.AuthorName, map[string]any{
						"action_name": "approved", "target_type": "MergeRequest", "target_iid": p.Number, "target_id": p.Number})
				}
			}
			continue
		}
		note := func(comment Comment, targetType string) {
			add(comment.ID, comment.CreatedAt, comment.Author, comment.AuthorName, map[string]any{
				"action_name": "commented on", "target_type": targetType, "target_id": comment.ID,
				"note": map[string]any{"noteable_type": "MergeRequest", "noteable_iid": p.Number, "system": false, "type": nil}})
		}
		for _, comment := range p.Comments {
			note(comment, "Note")
			for _, reply := range comment.Replies {
				note(reply, "DiscussionNote")
			}
		}
		for _, thread := range p.Threads {
			for _, comment := range thread.Comments {
				note(comment, "DiffNote")
			}
		}
	}
	// GitLab sorts a project's events by id, not by creation time.
	slices.SortStableFunc(events, func(a, b event) int { return cmp.Compare(a.id, b.id) })
	if sortOrder != "asc" {
		slices.Reverse(events)
	}
	selected, next := paginate(events, perPage, page)
	items := make([]map[string]any, 0, len(selected))
	for _, ev := range selected {
		if !r.EventsHidden {
			items = append(items, ev.body)
		}
	}
	return glabOK(jsonResponse(items), "X-Next-Page: "+next)
}

// glabUpdatedFilter keeps the merge requests updated at or after
// updated_after and at or before updated_before (ISO 8601 times), as
// GitLab's list filters them.
func glabUpdatedFilter(query url.Values) (func(p *Pull) bool, error) {
	bounds := map[string]time.Time{}
	for _, key := range []string{"updated_after", "updated_before"} {
		if raw := query.Get(key); raw != "" {
			t, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return nil, fmt.Errorf("%s %q is not an ISO 8601 time", key, raw)
			}
			bounds[key] = t
		}
	}
	return func(p *Pull) bool {
		at, _ := time.Parse(time.RFC3339Nano, p.UpdatedAt)
		if after, ok := bounds["updated_after"]; ok && at.Before(after) {
			return false
		}
		if before, ok := bounds["updated_before"]; ok && at.After(before) {
			return false
		}
		return true
	}, nil
}
