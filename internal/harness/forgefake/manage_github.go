package forgefake

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// GitHub's management surface: the Details half of PRTick's detail, the
// REST reviewer and assignee routes, and the GraphQL edits, label
// changes, state changes, searches and default merge message.

// githubManageDetail is the Details selection of a pull: the viewerCan*
// flags, the review requests, assignees and labels. Callers hold mu.
func (e *Engine) githubManageDetail(r *Repo, p *Pull) map[string]any {
	update := e.mayUpdate(r, p)
	requests := []map[string]any{}
	for _, login := range p.Reviewers {
		reviewer := githubActor(login, "")
		if m := r.member(login); m != nil && m.Name != "" {
			reviewer["name"] = m.Name
		}
		reviewer["__typename"] = "User"
		requests = append(requests, map[string]any{"requestedReviewer": reviewer})
	}
	for _, combined := range p.TeamReviewers {
		team := r.team(combined)
		requests = append(requests, map[string]any{"requestedReviewer": map[string]any{"__typename": "Team", "combinedSlug": combined, "name": team.Name}})
	}
	assignees := []map[string]any{}
	for _, login := range p.Assignees {
		name := ""
		if m := r.member(login); m != nil {
			name = m.Name
		}
		assignees = append(assignees, githubActor(login, name))
	}
	return map[string]any{
		"viewerCanUpdate": update,
		"viewerCanClose":  update,
		"viewerCanReopen": update,
		"viewerCanLabel":  e.mayLabel(r, p),
		"reviewRequests":  map[string]any{"nodes": requests},
		"assignees":       map[string]any{"nodes": assignees},
		"labels":          map[string]any{"nodes": githubLabelNodes(r, p.Labels)},
	}
}

func githubLabelNodes(r *Repo, names []string) []map[string]any {
	out := []map[string]any{}
	for _, name := range names {
		if label := r.label(name); label != nil {
			out = append(out, map[string]any{"id": label.ID, "name": label.Name, "color": strings.TrimPrefix(label.Color, "#")})
		}
	}
	return out
}

func githubRepoNodeID(r *Repo) string { return "R_ao" + strconv.FormatInt(r.ID, 10) }

// githubOwnerType is the repository owner's __typename.
func githubOwnerType(r *Repo) string {
	if r.Organization {
		return "Organization"
	}
	return "User"
}

// ghRESTPull resolves the pull a REST route names.
func (e *Engine) ghRESTPull(c *call, m []string) (*Repo, *Pull, response, bool) {
	r := e.repoOn("github", c.http.host, m[1])
	if r == nil {
		return nil, nil, ghHTTPNotFound(), false
	}
	number, _ := strconv.Atoi(m[2])
	p := r.pull(number)
	if p == nil {
		return nil, nil, ghHTTPNotFound(), false
	}
	return r, p, response{}, true
}

// ghRequestedReviewers answers POST and DELETE
// `repos/O/R/pulls/N/requested_reviewers` with `{reviewers,
// team_reviewers}`. A request needs triage access (403), names
// collaborators only and never the author (422, nothing changed); each
// added reviewer is pending again (a re-request) with its
// ReviewRequestedEvent.
func ghRequestedReviewers(e *Engine, c *call, m []string) response {
	var in struct {
		Reviewers     []string `json:"reviewers"`
		TeamReviewers []string `json:"team_reviewers"`
	}
	if err := decodeRESTBody(c, &in, false); err != nil {
		return unhandled("%v", err)
	}
	r, p, fail, ok := e.ghRESTPull(c, m)
	if !ok {
		return fail
	}
	owner, _, _ := strings.Cut(r.Project, "/")
	if !e.mayTriage(r, p) {
		return ghStatus(http.StatusForbidden, "Must have triage or write access to request reviews.")
	}
	adding := c.flag("method") == http.MethodPost
	if adding {
		for _, login := range in.Reviewers {
			switch {
			case strings.EqualFold(login, p.Author):
				return ghUnprocessable("Review cannot be requested from pull request author.")
			case r.member(login) == nil:
				return ghUnprocessable(fmt.Sprintf("Reviews may only be requested from collaborators. One or more of the users or teams you specified is not a collaborator of the %s repository.", r.Project))
			}
		}
		for _, slug := range in.TeamReviewers {
			if r.team(owner+"/"+slug) == nil {
				return ghUnprocessable(fmt.Sprintf("Reviews may only be requested from collaborators. One or more of the users or teams you specified is not a collaborator of the %s repository.", r.Project))
			}
		}
	}
	now := githubNow()
	for _, login := range in.Reviewers {
		if adding {
			if !containsFold(p.Reviewers, login) {
				p.Reviewers = append(p.Reviewers, login)
			}
			p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "review_requested", Actor: e.viewer, At: now, Ref: login})
		} else {
			p.Reviewers = removeFold(p.Reviewers, login)
		}
	}
	for _, slug := range in.TeamReviewers {
		combined := owner + "/" + slug
		if adding {
			if !containsFold(p.TeamReviewers, combined) {
				p.TeamReviewers = append(p.TeamReviewers, combined)
			}
		} else {
			p.TeamReviewers = removeFold(p.TeamReviewers, combined)
		}
	}
	resp := jsonResponse(map[string]any{"number": p.Number, "requested_reviewers": p.Reviewers})
	if adding {
		resp.status = http.StatusCreated
	}
	return resp
}

// ghAssignees answers POST and DELETE `repos/O/R/issues/N/assignees`
// with `{assignees}`. As GitHub does, a login that cannot be assigned
// (not a collaborator), or any change by a viewer without triage access,
// is silently ignored; the answer is the issue as it stands.
func ghAssignees(e *Engine, c *call, m []string) response {
	var in struct {
		Assignees []string `json:"assignees"`
	}
	if err := decodeRESTBody(c, &in, false); err != nil {
		return unhandled("%v", err)
	}
	r, p, fail, ok := e.ghRESTPull(c, m)
	if !ok {
		return fail
	}
	adding := c.flag("method") == http.MethodPost
	if e.mayTriage(r, p) {
		for _, login := range in.Assignees {
			switch {
			case adding && r.member(login) != nil && !containsFold(p.Assignees, login):
				p.Assignees = append(p.Assignees, login)
			case !adding:
				p.Assignees = removeFold(p.Assignees, login)
			}
		}
	}
	resp := jsonResponse(map[string]any{"number": p.Number, "assignees": p.Assignees})
	if adding {
		resp.status = http.StatusCreated
	}
	return resp
}

// ghPREditTarget answers the pull's node id and body.
func ghPREditTarget(e *Engine, c *call, v graphQLVars) response {
	project, number, err := v.pullVars()
	if err != nil {
		return unhandled("PREditTarget: %v", err)
	}
	_, p, fail, ok := e.ghGraphQLPull(c, project, number, map[string]any{})
	if !ok {
		return fail
	}
	return graphQLData(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"id": p.NodeID, "body": p.Body}}})
}

// ghEditPR sets the title and/or body the request supplies; a viewer
// who may not update the pull is refused, as is an empty title.
func ghEditPR(e *Engine, c *call, v graphQLVars) response {
	const field = "updatePullRequest"
	r, p, fail, ok := e.ghMutationPull(c, v, field)
	if !ok {
		return fail
	}
	title, hasTitle := v["title"]
	body, hasBody := v["body"]
	switch {
	case !hasTitle && !hasBody:
		return unhandled("EditPR changes neither title nor body")
	case !e.mayUpdate(r, p):
		return graphQLMutationError(field, "FORBIDDEN", fmt.Sprintf("%s does not have the correct permissions to execute `UpdatePullRequest`", e.viewer))
	}
	if hasTitle {
		s, ok := title.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return graphQLMutationError(field, "UNPROCESSABLE", "Title can't be blank")
		}
	}
	if hasBody {
		if _, ok := body.(string); !ok {
			return unhandled("EditPR body is not a string")
		}
	}
	if hasTitle {
		p.Title = title.(string)
	}
	if hasBody {
		p.Body = body.(string)
	}
	return graphQLData(map[string]any{field: map[string]any{"pullRequest": map[string]any{"id": p.NodeID}}})
}

// ghPRLabelTarget answers the repository's and the pull's node ids, the
// pull's labels, and each id of $ids as a node: a label (with its
// repository) or null beside a NOT_FOUND error at its index.
func ghPRLabelTarget(e *Engine, c *call, v graphQLVars) response {
	project, number, err := v.pullVars()
	ids, err1 := v.strs("ids")
	if err := firstErr(err, err1); err != nil {
		return unhandled("PRLabelTarget: %v", err)
	}
	r, p, fail, ok := e.ghGraphQLPull(c, project, number, map[string]any{"nodes": nil})
	if !ok {
		return fail
	}
	labels := []map[string]any{}
	for _, node := range githubLabelNodes(r, p.Labels) {
		labels = append(labels, map[string]any{"id": node["id"]})
	}
	nodes := make([]any, len(ids))
	var problems []map[string]any
	for i, id := range ids {
		if owner, label := e.githubLabel(c, id); label != nil {
			nodes[i] = map[string]any{"__typename": "Label", "id": label.ID, "repository": map[string]any{"id": githubRepoNodeID(owner)}}
			continue
		}
		problems = append(problems, map[string]any{"type": "NOT_FOUND", "path": []any{"nodes", i}, "message": fmt.Sprintf("Could not resolve to a node with the global id of '%s'.", id)})
	}
	data := map[string]any{
		"repository": map[string]any{"id": githubRepoNodeID(r), "pullRequest": map[string]any{"id": p.NodeID, "labels": map[string]any{"nodes": labels}}},
		"nodes":      nodes,
	}
	if len(problems) > 0 {
		return jsonResponse(map[string]any{"data": data, "errors": problems})
	}
	return graphQLData(data)
}

// githubLabel resolves a label node id among the GitHub repositories on
// the request's host.
func (e *Engine) githubLabel(c *call, id string) (*Repo, *Label) {
	for _, key := range sortedKeys(e.repos) {
		r := e.repos[key]
		if r.Forge != "github" || !strings.EqualFold(r.Host, c.http.host) {
			continue
		}
		if label := r.labelByID(id); label != nil {
			return r, label
		}
	}
	return nil, nil
}

// ghChangeLabels adds (add) or removes the labels $labelIDs names on the
// pull $labelableID names. A viewer without viewerCanLabel, or a label of
// another repository, is refused and nothing changes.
func ghChangeLabels(add bool) func(e *Engine, c *call, v graphQLVars) response {
	field := "removeLabelsFromLabelable"
	if add {
		field = "addLabelsToLabelable"
	}
	return func(e *Engine, c *call, v graphQLVars) response {
		pullID, err := v.str("labelableID")
		ids, err1 := v.strs("labelIDs")
		if err := firstErr(err, err1); err != nil {
			return unhandled("%s: %v", field, err)
		}
		r, p, fail, ok := e.ghMutationPull(c, graphQLVars{"pullRequestID": pullID}, field)
		if !ok {
			return fail
		}
		if !e.mayLabel(r, p) {
			return graphQLMutationError(field, "FORBIDDEN", fmt.Sprintf("%s does not have the correct permissions to execute `%s`", e.viewer, strings.ToUpper(field[:1])+field[1:]))
		}
		names := make([]string, 0, len(ids))
		for _, id := range ids {
			label := r.labelByID(id)
			if label == nil {
				return graphQLMutationError(field, "NOT_FOUND", fmt.Sprintf("Could not resolve to a node with the global id of '%s'.", id))
			}
			names = append(names, label.Name)
		}
		for _, name := range names {
			switch {
			case add && !containsFold(p.Labels, name):
				p.Labels = append(p.Labels, name)
			case !add:
				p.Labels = removeFold(p.Labels, name)
			}
		}
		return graphQLData(map[string]any{field: map[string]any{"clientMutationId": nil}})
	}
}

// ghSetState answers ClosePR, ReopenPR and ConvertPRToDraft: each needs
// a viewer who may update the pull and a pull in the state it leaves;
// each records its timeline event.
func ghSetState(state string) func(e *Engine, c *call, v graphQLVars) response {
	field := map[string]string{"close": "closePullRequest", "reopen": "reopenPullRequest", "draft": "convertPullRequestToDraft"}[state]
	return func(e *Engine, c *call, v graphQLVars) response {
		r, p, fail, ok := e.ghMutationPull(c, v, field)
		if !ok {
			return fail
		}
		if !e.mayUpdate(r, p) {
			return graphQLMutationError(field, "FORBIDDEN", fmt.Sprintf("%s does not have the correct permissions to execute `%s`", e.viewer, strings.ToUpper(field[:1])+field[1:]))
		}
		now := githubNow()
		switch state {
		case "close":
			if p.State != "open" {
				return graphQLMutationError(field, "UNPROCESSABLE", "Pull request is not open")
			}
			p.State, p.ClosedAt, p.ClosedBy = "closed", now, e.viewer
			p.Readiness.AutoMerge = nil
			p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "closed", Actor: e.viewer, At: now})
		case "reopen":
			if p.State != "closed" {
				return graphQLMutationError(field, "UNPROCESSABLE", "Pull request is not closed")
			}
			p.State, p.ClosedAt, p.ClosedBy = "open", "", ""
			p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "reopened", Actor: e.viewer, At: now})
		case "draft":
			if p.State != "open" || p.Draft {
				return graphQLMutationError(field, "UNPROCESSABLE", "Pull request is not an open pull request ready for review")
			}
			p.Draft = true
			p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "marked_draft", Actor: e.viewer, At: now})
		}
		return graphQLData(map[string]any{field: map[string]any{"pullRequest": map[string]any{"state": strings.ToUpper(p.State), "isDraft": p.Draft}}})
	}
}

// ghSearchAssignable answers the repository's members matching $query as
// assignableUsers, and with $withTeams on an organization's repository
// its teams; teamsRefused answers the teams as GitHub does a token
// without read:org (null beside an error at their path).
func ghSearchAssignable(e *Engine, c *call, v graphQLVars) response {
	owner, err := v.str("owner")
	name, err1 := v.str("name")
	query, err2 := v.str("query")
	withTeams, err3 := v.boolean("withTeams")
	if err := firstErr(err, err1, err2, err3); err != nil {
		return unhandled("SearchAssignable: %v", err)
	}
	r := e.repoOn("github", c.http.host, owner+"/"+name)
	if r == nil {
		return graphQLNotFound(map[string]any{"repository": nil}, []string{"repository"}, fmt.Sprintf("Could not resolve to a Repository with the name '%s/%s'.", owner, name))
	}
	users := []map[string]any{}
	for _, m := range r.Members {
		if matches(query, m.Login, m.Name) {
			users = append(users, githubActor(m.Login, m.Name))
		}
	}
	ownerNode := map[string]any{}
	var problems []map[string]any
	if withTeams && r.Organization {
		if r.TeamsRefused {
			ownerNode["teams"] = nil
			problems = append(problems, map[string]any{"type": "INSUFFICIENT_SCOPES", "path": []any{"repository", "owner", "teams"}, "message": "Your token has not been granted the required scopes to execute this query. The 'teams' field requires one of the following scopes: ['read:org']."})
		} else {
			teams := []map[string]any{}
			for _, team := range r.Teams {
				if matches(query, team.Slug, team.Name) {
					teams = append(teams, map[string]any{"combinedSlug": owner + "/" + team.Slug, "name": team.Name})
				}
			}
			ownerNode["teams"] = githubPage(teams, 20)
		}
	}
	data := map[string]any{"repository": map[string]any{"assignableUsers": githubPage(users, 20), "owner": ownerNode}}
	if len(problems) > 0 {
		return jsonResponse(map[string]any{"data": data, "errors": problems})
	}
	return graphQLData(data)
}

// githubPage is the first page of nodes with its hasNextPage.
func githubPage(nodes []map[string]any, first int) map[string]any {
	more := len(nodes) > first
	if more {
		nodes = nodes[:first]
	}
	return map[string]any{"pageInfo": map[string]any{"hasNextPage": more}, "nodes": nodes}
}

// ghSearchLabels answers the repository's labels matching $query.
func ghSearchLabels(e *Engine, c *call, v graphQLVars) response {
	owner, err := v.str("owner")
	name, err1 := v.str("name")
	query, err2 := v.str("query")
	if err := firstErr(err, err1, err2); err != nil {
		return unhandled("SearchLabels: %v", err)
	}
	r := e.repoOn("github", c.http.host, owner+"/"+name)
	if r == nil {
		return graphQLNotFound(map[string]any{"repository": nil}, []string{"repository"}, fmt.Sprintf("Could not resolve to a Repository with the name '%s/%s'.", owner, name))
	}
	var names []string
	for _, label := range r.Labels {
		if matches(query, label.Name) {
			names = append(names, label.Name)
		}
	}
	return graphQLData(map[string]any{"repository": map[string]any{"labels": githubPage(githubLabelNodes(r, names), 20)}})
}

// ghPRMergeMessage answers GitHub's default merge message of the method:
// a merge commit's "Merge pull request #N from owner/head" over the
// title, a squash's "Title (#N)" over the body.
func ghPRMergeMessage(e *Engine, c *call, v graphQLVars) response {
	project, number, err := v.pullVars()
	method, err1 := v.str("method")
	if err := firstErr(err, err1); err != nil {
		return unhandled("PRMergeMessage: %v", err)
	}
	if !slices.Contains([]string{"MERGE", "SQUASH", "REBASE"}, method) {
		return unhandled("PRMergeMessage: method %q is not a PullRequestMergeMethod", method)
	}
	r, p, fail, ok := e.ghGraphQLPull(c, project, number, map[string]any{})
	if !ok {
		return fail
	}
	owner, _, _ := strings.Cut(r.Project, "/")
	headline, body := fmt.Sprintf("Merge pull request #%d from %s/%s", p.Number, owner, p.HeadRef), p.Title
	switch method {
	case "SQUASH":
		headline, body = fmt.Sprintf("%s (#%d)", p.Title, p.Number), p.Body
	case "REBASE":
		headline, body = "", ""
	}
	return graphQLData(map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"viewerMergeHeadlineText": headline, "viewerMergeBodyText": body}}})
}

// ghWithMessage is a merge or auto-merge with an edited commit message:
// GitHub takes any headline and body, so the handler checks their types
// and merges as without one; the invocation records what was sent.
func ghWithMessage(run func(e *Engine, c *call, v graphQLVars) response) func(e *Engine, c *call, v graphQLVars) response {
	return func(e *Engine, c *call, v graphQLVars) response {
		_, err := v.str("commitHeadline")
		_, err1 := v.str("commitBody")
		if err := firstErr(err, err1); err != nil {
			return unhandled("%v", err)
		}
		return run(e, c, v)
	}
}
