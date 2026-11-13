package forgefake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// GitLab's management surface: the merge request's people and labels in
// its REST answer, the merge request update, the project's users and
// labels, the resource state events, and the GraphQL Details read,
// re-review, bypass and default merge message.

// gitlabManageJSON is the merge request's reviewers, assignees (each with
// its user id) and label names. Callers hold mu.
func (e *Engine) gitlabManageJSON(r *Repo, p *Pull) map[string]any {
	users := func(logins []string) []map[string]any {
		out := []map[string]any{}
		for _, login := range logins {
			out = append(out, map[string]any{"id": e.gitlabUserID(r, login), "username": login, "name": e.gitlabName(r, login), "state": "active"})
		}
		return out
	}
	labels := []string{}
	labels = append(labels, p.Labels...)
	return map[string]any{"reviewers": users(p.Reviewers), "assignees": users(p.Assignees), "labels": labels}
}

func (e *Engine) gitlabName(r *Repo, login string) string {
	if m := r.member(login); m != nil && m.Name != "" {
		return m.Name
	}
	return login
}

// glabUpdateMR answers `PUT .../merge_requests/N` with any of `{title,
// description, state_event, reviewer_ids, assignee_ids, add_labels,
// remove_labels}`. A viewer who may not update the merge request is
// refused (403) and an empty title is GitLab's 400; as GitLab does, the
// reviewer, assignee and label fields of a viewer without
// adminMergeRequest are dropped, a user id that resolves to no member
// is dropped, an unknown label name is skipped, and a state_event the
// merge request cannot take changes nothing. The title's draft prefix,
// a close, a reopen and each added reviewer leave their note or state
// event.
func glabUpdateMR(e *Engine, c *call, m []string) response {
	var in struct {
		Title        *string  `json:"title"`
		Description  *string  `json:"description"`
		StateEvent   *string  `json:"state_event"`
		ReviewerIDs  *[]int64 `json:"reviewer_ids"`
		AssigneeIDs  *[]int64 `json:"assignee_ids"`
		AddLabels    *string  `json:"add_labels"`
		RemoveLabels *string  `json:"remove_labels"`
	}
	if err := decodeRESTBody(c, &in, false); err != nil {
		return unhandled("%v", err)
	}
	if in.StateEvent != nil && *in.StateEvent != "close" && *in.StateEvent != "reopen" {
		return unhandled("state_event %q is not close or reopen", *in.StateEvent)
	}
	r, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	switch {
	case in.Title != nil && strings.TrimSpace(*in.Title) == "":
		out, _ := json.Marshal(map[string]any{"message": map[string]any{"title": []string{"can't be blank"}}})
		return response{stdout: out, stderr: "400 title can't be blank", status: http.StatusBadRequest, header: http.Header{"Content-Type": {"application/json"}}}
	case !e.mayUpdate(r, p):
		return glabStatus(http.StatusForbidden, "403 Forbidden")
	}
	now := glabNow()
	if in.Title != nil {
		p.Title = *in.Title
		if draft := gitlabDraftTitle.MatchString(p.Title); draft != p.Draft {
			p.Draft = draft
			kind := "marked_ready"
			if draft {
				kind = "marked_draft"
			}
			p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: kind, Actor: e.viewer, At: now})
		}
	}
	if in.Description != nil {
		p.Body = *in.Description
	}
	if e.mayAdmin(r, p) {
		if in.ReviewerIDs != nil {
			next := e.gitlabUsers(r, p.Reviewers, *in.ReviewerIDs)
			var added []string
			for _, login := range next {
				if !containsFold(p.Reviewers, login) {
					added = append(added, login)
				}
			}
			for _, login := range p.Reviewers {
				if !containsFold(next, login) {
					p.ChangeRequests = removeFold(p.ChangeRequests, login)
				}
			}
			p.Reviewers = next
			if len(added) > 0 {
				p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "review_requested", Actor: e.viewer, At: now, Ref: strings.Join(added, ",")})
			}
		}
		if in.AssigneeIDs != nil {
			p.Assignees = e.gitlabUsers(r, p.Assignees, *in.AssigneeIDs)
		}
		if in.AddLabels != nil {
			for _, name := range strings.Split(*in.AddLabels, ",") {
				if label := r.label(strings.TrimSpace(name)); label != nil && !containsFold(p.Labels, label.Name) {
					p.Labels = append(p.Labels, label.Name)
				}
			}
		}
		if in.RemoveLabels != nil {
			for _, name := range strings.Split(*in.RemoveLabels, ",") {
				p.Labels = removeFold(p.Labels, strings.TrimSpace(name))
			}
		}
	}
	if in.StateEvent != nil {
		switch {
		case *in.StateEvent == "close" && p.State == "open":
			p.State, p.ClosedAt, p.ClosedBy = "closed", now, e.viewer
			p.Readiness.AutoMerge = nil
			p.StateEvents = append(p.StateEvents, StateEvent{ID: e.ids.next(), State: "closed", Actor: e.viewer, At: now})
		case *in.StateEvent == "reopen" && p.State == "closed":
			p.State, p.ClosedAt, p.ClosedBy = "open", "", ""
			p.StateEvents = append(p.StateEvents, StateEvent{ID: e.ids.next(), State: "reopened", Actor: e.viewer, At: now})
		}
	}
	return glabOK(jsonResponse(e.gitlabMRJSON(r, p)))
}

// gitlabUsers resolves ids to logins: one already in current, or a
// member (or the viewer) GitLab would accept; any other is dropped.
func (e *Engine) gitlabUsers(r *Repo, current []string, ids []int64) []string {
	out := []string{}
	for _, id := range ids {
		login, ok := e.gitlabLoginOf(r, id)
		if !ok || containsFold(out, login) {
			continue
		}
		if containsFold(current, login) || r.member(login) != nil || strings.EqualFold(login, e.viewer) {
			out = append(out, login)
		}
	}
	return out
}

// glabProjectUsers answers `GET projects/P/users?search=&per_page=` with
// the project's members matching search.
func glabProjectUsers(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return fail
	}
	query, perPage, page, err := glabPage(m[2], "search")
	if err != nil {
		return unhandled("%v", err)
	}
	var users []map[string]any
	for _, member := range r.Members {
		if matches(query.Get("search"), member.Login, member.Name) {
			users = append(users, map[string]any{"id": member.ID, "username": member.Login, "name": e.gitlabName(r, member.Login), "state": "active"})
		}
	}
	items, next := paginate(users, perPage, page)
	if items == nil {
		items = []map[string]any{}
	}
	return glabOK(jsonResponse(items), "X-Next-Page: "+next, "X-Page: "+strconv.Itoa(page))
}

// glabProjectLabels answers `GET projects/P/labels?search=&per_page=`.
func glabProjectLabels(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return fail
	}
	query, perPage, page, err := glabPage(m[2], "search")
	if err != nil {
		return unhandled("%v", err)
	}
	var labels []map[string]any
	for i, label := range r.Labels {
		if matches(query.Get("search"), label.Name) {
			labels = append(labels, map[string]any{"id": r.ID*1000 + int64(i), "name": label.Name, "color": label.Color})
		}
	}
	items, next := paginate(labels, perPage, page)
	if items == nil {
		items = []map[string]any{}
	}
	return glabOK(jsonResponse(items), "X-Next-Page: "+next, "X-Page: "+strconv.Itoa(page))
}

// glabStateEvents answers the merge request's resource state events.
func glabStateEvents(e *Engine, c *call, m []string) response {
	r, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	_, perPage, page, err := glabPage(m[3])
	if err != nil {
		return unhandled("%v", err)
	}
	events := []map[string]any{}
	for _, event := range p.StateEvents {
		events = append(events, map[string]any{
			"id": event.ID, "user": map[string]any{"id": e.gitlabUserID(r, event.Actor), "username": event.Actor, "name": e.gitlabName(r, event.Actor)},
			"created_at": event.At, "resource_type": "MergeRequest", "resource_id": r.ID*1000 + int64(p.Number), "state": event.State,
		})
	}
	items, next := paginate(events, perPage, page)
	return glabOK(jsonResponse(items), "X-Next-Page: "+next, "X-Page: "+strconv.Itoa(page))
}

// glabGraphQLMR resolves the merge request a GitLab GraphQL operation
// names by fullPath (or projectPath) and iid. A miss answers null, as
// GitLab does.
func (e *Engine) glabGraphQLMR(c *call, v graphQLVars, pathVar string) (*Repo, *Pull, response, bool) {
	project, err := v.str(pathVar)
	iid, err1 := v.str("iid")
	if err := firstErr(err, err1); err != nil {
		return nil, nil, unhandled("%v", err), false
	}
	r := e.repoOn("gitlab", c.http.host, project)
	if r == nil {
		return nil, nil, graphQLData(map[string]any{"project": nil}), false
	}
	number, _ := strconv.Atoi(iid)
	p := r.pull(number)
	if p == nil {
		return nil, nil, graphQLData(map[string]any{"project": map[string]any{"mergeRequest": nil}}), false
	}
	return r, p, response{}, true
}

// glabManage answers PRManage (checks false) and PRManageChecks: the
// viewer's permissions, the reviewers with their reviewState, the
// assignees and labels, and the schema probes, which answer what the
// repository's ServerLacks takes away. PRManageChecks on a server
// without the override is refused as such a server refuses the field.
func glabManage(checks bool) func(e *Engine, c *call, v graphQLVars) response {
	return func(e *Engine, c *call, v graphQLVars) response {
		r, p, fail, ok := e.glabGraphQLMR(c, v, "fullPath")
		if !ok {
			return fail
		}
		if checks && r.lacks(lacksOverride) {
			return jsonResponse(map[string]any{"errors": []map[string]any{{"message": "Field 'mergeabilityChecks' doesn't exist on type 'MergeRequest'"}}})
		}
		gid := func(login string) string {
			return "gid://gitlab/User/" + strconv.FormatInt(e.gitlabUserID(r, login), 10)
		}
		reviewers := []map[string]any{}
		for _, login := range p.Reviewers {
			reviewers = append(reviewers, map[string]any{"id": gid(login), "username": login, "name": e.gitlabName(r, login),
				"mergeRequestInteraction": map[string]any{"reviewState": strings.ToUpper(gitlabReviewState(p, login))}})
		}
		assignees := []map[string]any{}
		for _, login := range p.Assignees {
			assignees = append(assignees, map[string]any{"id": gid(login), "username": login, "name": e.gitlabName(r, login)})
		}
		labels := []map[string]any{}
		for _, name := range p.Labels {
			if label := r.label(name); label != nil {
				labels = append(labels, map[string]any{"title": label.Name, "color": label.Color})
			}
		}
		mr := map[string]any{
			"userPermissions": map[string]any{"updateMergeRequest": e.mayUpdate(r, p), "adminMergeRequest": e.mayAdmin(r, p)},
			"reviewers":       map[string]any{"nodes": reviewers},
			"assignees":       map[string]any{"nodes": assignees},
			"labels":          map[string]any{"nodes": labels},
		}
		if checks {
			status := "INACTIVE"
			if len(p.ChangeRequests) > 0 {
				status = "SUCCESS"
				if !p.ChangesOverridden {
					status = "FAILED"
				}
			}
			mr["mergeabilityChecks"] = []map[string]any{{"identifier": "REQUESTED_CHANGES", "status": status}}
		}
		var rereview any = map[string]any{"name": "MergeRequestReviewerRereviewInput"}
		if r.lacks(lacksRereview) {
			rereview = nil
		}
		fields := []map[string]any{}
		for _, name := range []string{"projectPath", "iid", "title", "description", "state", "targetBranch", "overrideRequestedChanges"} {
			if name != "overrideRequestedChanges" || !r.lacks(lacksOverride) {
				fields = append(fields, map[string]any{"name": name})
			}
		}
		return graphQLData(map[string]any{
			"project":     map[string]any{"mergeRequest": mr},
			"rereview":    rereview,
			"updateInput": map[string]any{"inputFields": fields},
		})
	}
}

// glabGraphQLRefusal is GitLab's top-level GraphQL error.
func glabGraphQLRefusal(message string) response {
	return jsonResponse(map[string]any{"data": nil, "errors": []map[string]any{{"message": message}}})
}

const glabNoPermission = "The resource that you are attempting to access does not exist or you don't have permission to perform this action"

// glabRereview asks a reviewer for a new review: their state reads
// unreviewed and their change request is cleared, with a
// review_requested note. A server that lacks the mutation, a viewer who
// may not update the merge request, and a user who is not a reviewer are
// refused, changing nothing.
func glabRereview(e *Engine, c *call, v graphQLVars) response {
	userID, err := v.str("userId")
	if err != nil {
		return unhandled("PRRereview: %v", err)
	}
	r, p, fail, ok := e.glabGraphQLMR(c, v, "projectPath")
	if !ok {
		return fail
	}
	if r.lacks(lacksRereview) {
		return glabGraphQLRefusal("Field 'mergeRequestReviewerRereview' doesn't exist on type 'Mutation'")
	}
	if !e.mayUpdate(r, p) {
		return glabGraphQLRefusal(glabNoPermission)
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(userID, "gid://gitlab/User/"), 10, 64)
	if err != nil || !strings.HasPrefix(userID, "gid://gitlab/User/") {
		return glabGraphQLRefusal(fmt.Sprintf("%q is not a valid ID for UserID.", userID))
	}
	login, known := e.gitlabLoginOf(r, id)
	if !known || !containsFold(p.Reviewers, login) {
		return graphQLData(map[string]any{"mergeRequestReviewerRereview": map[string]any{"errors": []string{"Reviewer not found"}}})
	}
	p.ChangeRequests = removeFold(p.ChangeRequests, login)
	if !containsFold(p.rerequested, login) {
		p.rerequested = append(p.rerequested, login)
	}
	p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "review_requested", Actor: e.viewer, At: glabNow(), Ref: login})
	return graphQLData(map[string]any{"mergeRequestReviewerRereview": map[string]any{"errors": []string{}}})
}

// glabBypassChanges overrides the requested changes: the merge request's
// requested_changes check passes. A server that lacks the override and a
// viewer who cannot merge are refused, changing nothing.
func glabBypassChanges(e *Engine, c *call, v graphQLVars) response {
	r, p, fail, ok := e.glabGraphQLMR(c, v, "projectPath")
	if !ok {
		return fail
	}
	switch {
	case r.lacks(lacksOverride):
		return glabGraphQLRefusal("InputObject 'MergeRequestUpdateInput' doesn't accept argument 'overrideRequestedChanges'")
	case gitlabRoles[r.Merge.Permission] < gitlabRoles["developer"]:
		return graphQLData(map[string]any{"mergeRequestUpdate": map[string]any{"errors": []string{"Only users with permission to merge can override requested changes"}}})
	}
	p.ChangesOverridden = true
	return graphQLData(map[string]any{"mergeRequestUpdate": map[string]any{"errors": []string{}}})
}

// glabMergeMessage answers the default merge and squash commit messages
// of GitLab's default templates.
func glabMergeMessage(e *Engine, c *call, v graphQLVars) response {
	r, p, fail, ok := e.glabGraphQLMR(c, v, "fullPath")
	if !ok {
		return fail
	}
	merge := fmt.Sprintf("Merge branch '%s' into '%s'\n\n%s\n\nSee merge request %s!%d", p.HeadRef, p.BaseRef, p.Title, r.Project, p.Number)
	return graphQLData(map[string]any{"project": map[string]any{"mergeRequest": map[string]any{
		"defaultMergeCommitMessage": merge, "defaultSquashCommitMessage": p.Title,
	}}})
}
