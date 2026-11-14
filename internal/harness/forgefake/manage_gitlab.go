package forgefake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// GitLab's management surface: the merge request's people and labels in
// its REST answer, the merge request update, the project's users, the
// resource state events, and the GraphQL schema probe, Details read, set
// mutations, draft, label search, re-review, bypass and default merge
// message.

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
// description, state_event}`. A body that sets none of them is GitLab's
// 400; a viewer who may not update the merge request is refused (403)
// and an empty title is GitLab's 400; a state_event the merge request
// cannot take changes nothing. The title's draft prefix, a close and a
// reopen leave their note or state event (a reopen as GitLab records
// it, "opened").
func glabUpdateMR(e *Engine, c *call, m []string) response {
	var in struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
		StateEvent  *string `json:"state_event"`
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
	badRequest := func(message any) response {
		out, _ := json.Marshal(map[string]any{"message": message})
		return response{stdout: out, stderr: fmt.Sprintf("400 %v", message), status: http.StatusBadRequest, header: http.Header{"Content-Type": {"application/json"}}}
	}
	switch {
	case in.Title == nil && in.Description == nil && in.StateEvent == nil:
		out, _ := json.Marshal(map[string]any{"error": "assignee_id, assignee_ids, reviewer_ids, description, state_event, target_branch, title, discussion_locked, squash, labels, add_labels, remove_labels, milestone_id, remove_source_branch, allow_collaboration, allow_maintainer_to_push are missing, at least one parameter must be provided"})
		return response{stdout: out, stderr: "400 at least one parameter must be provided", status: http.StatusBadRequest, header: http.Header{"Content-Type": {"application/json"}}}
	case in.Title != nil && strings.TrimSpace(*in.Title) == "":
		return badRequest(map[string]any{"title": []string{"can't be blank"}})
	case !e.mayUpdate(r, p):
		return glabStatus(http.StatusForbidden, "403 Forbidden")
	}
	now := glabNow()
	if in.Title != nil {
		p.Title = *in.Title
		e.glabFollowDraftTitle(p, now)
	}
	if in.Description != nil {
		p.Body = *in.Description
	}
	if in.StateEvent != nil {
		switch {
		case *in.StateEvent == "close" && p.State == "open":
			p.State, p.ClosedAt, p.ClosedBy = "closed", now, e.viewer
			p.Readiness.AutoMerge = nil
			p.StateEvents = append(p.StateEvents, StateEvent{ID: e.ids.next(), State: "closed", Actor: e.viewer, At: now})
		case *in.StateEvent == "reopen" && p.State == "closed":
			p.State, p.ClosedAt, p.ClosedBy = "open", "", ""
			p.StateEvents = append(p.StateEvents, StateEvent{ID: e.ids.next(), State: "opened", Actor: e.viewer, At: now})
		}
	}
	return glabOK(jsonResponse(e.gitlabMRJSON(r, p)))
}

// glabFollowDraftTitle makes the merge request's draft state its title's
// prefix, with the note GitLab leaves when it changes.
func (e *Engine) glabFollowDraftTitle(p *Pull, now string) {
	draft := gitlabDraftTitle.MatchString(p.Title)
	if draft == p.Draft {
		return
	}
	p.Draft = draft
	kind := "marked_ready"
	if draft {
		kind = "marked_draft"
	}
	p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: kind, Actor: e.viewer, At: now})
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

// glabManageSchema answers PRManageSchema: the input types a server has,
// less what the repository's ServerLacks takes away. A server without
// a type answers null for it, as __type does. It reads no project, so
// it answers from the first GitLab repository on the host.
func glabManageSchema(e *Engine, c *call, _ graphQLVars) response {
	var r *Repo
	for _, key := range sortedKeys(e.repos) {
		if candidate := e.repos[key]; candidate.Forge == "gitlab" && strings.EqualFold(candidate.Host, c.http.host) {
			r = candidate
			break
		}
	}
	lacks := func(feature string) bool { return r != nil && r.lacks(feature) }
	fields := func(names ...string) map[string]any {
		out := []map[string]any{}
		for _, name := range names {
			out = append(out, map[string]any{"name": name})
		}
		return map[string]any{"inputFields": out}
	}
	set := func(list string) any {
		if lacks(lacksOperationMode) {
			return fields("projectPath", "iid", list)
		}
		return fields("projectPath", "iid", list, "operationMode")
	}
	var rereview any = map[string]any{"name": "MergeRequestReviewerRereviewInput"}
	if lacks(lacksRereview) {
		rereview = nil
	}
	update := fields("projectPath", "iid", "title", "description", "state", "targetBranch", "overrideRequestedChanges")
	if lacks(lacksOverride) {
		update = fields("projectPath", "iid", "title", "description", "state", "targetBranch")
	}
	return graphQLData(map[string]any{
		"rereview":     rereview,
		"updateInput":  update,
		"setReviewers": set("reviewerUsernames"),
		"setAssignees": set("assigneeUsernames"),
		"setLabels":    set("labelIds"),
	})
}

// glabManage answers PRManage (checks false) and PRManageChecks: the
// viewer's permissions, the reviewers with their reviewState, the
// assignees and labels (with their ids). PRManageChecks on a server
// without the override is refused as such a server refuses the field.
func glabManage(checks bool) func(e *Engine, c *call, v graphQLVars) response {
	return func(e *Engine, c *call, v graphQLVars) response {
		r, p, fail, ok := e.glabGraphQLMR(c, v, "fullPath")
		if !ok {
			return fail
		}
		if checks && r.lacks(lacksOverride) {
			return glabGraphQLInvalid("Field 'mergeabilityChecks' doesn't exist on type 'MergeRequest'")
		}
		reviewers := []map[string]any{}
		for _, login := range p.Reviewers {
			reviewers = append(reviewers, map[string]any{"id": e.gitlabGID(r, login), "username": login, "name": e.gitlabName(r, login),
				"mergeRequestInteraction": map[string]any{"reviewState": strings.ToUpper(gitlabReviewState(p, login))}})
		}
		assignees := []map[string]any{}
		for _, login := range p.Assignees {
			assignees = append(assignees, map[string]any{"id": e.gitlabGID(r, login), "username": login, "name": e.gitlabName(r, login)})
		}
		mr := map[string]any{
			"userPermissions": map[string]any{"updateMergeRequest": e.mayUpdate(r, p), "adminMergeRequest": e.mayAdmin(r, p)},
			"reviewers":       map[string]any{"nodes": reviewers},
			"assignees":       map[string]any{"nodes": assignees},
			"labels":          map[string]any{"nodes": gitlabLabelNodes(r, p.Labels)},
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
		return graphQLData(map[string]any{"project": map[string]any{"mergeRequest": mr}})
	}
}

func (e *Engine) gitlabGID(r *Repo, login string) string {
	return "gid://gitlab/User/" + strconv.FormatInt(e.gitlabUserID(r, login), 10)
}

// gitlabLabelNodes are the named labels as GraphQL label nodes.
func gitlabLabelNodes(r *Repo, names []string) []map[string]any {
	out := []map[string]any{}
	for _, name := range names {
		if label := r.label(name); label != nil {
			out = append(out, map[string]any{"id": label.ID, "title": label.Name, "color": label.Color})
		}
	}
	return out
}

// glabGraphQLInvalid is GitLab's answer to a document or variables that
// fail validation: errors with no data entry, before anything runs.
func glabGraphQLInvalid(message string) response {
	return jsonResponse(map[string]any{"errors": []map[string]any{{"message": message}}})
}

// glabGraphQLDenied is GitLab's answer to a mutation the viewer may not
// run: its field null beside the permission error.
func glabGraphQLDenied(field string) response {
	return jsonResponse(map[string]any{"data": map[string]any{field: nil}, "errors": []map[string]any{{"message": glabNoPermission, "path": []any{field}}}})
}

const glabNoPermission = "The resource that you are attempting to access does not exist or you don't have permission to perform this action"

// glabMutationMR resolves a set mutation's merge request and mode, and
// refuses a server without operationMode and a viewer without
// adminMergeRequest.
func (e *Engine) glabMutationMR(c *call, v graphQLVars, field string) (*Repo, *Pull, string, response, bool) {
	mode, err := v.str("mode")
	if err != nil || (mode != "APPEND" && mode != "REMOVE") {
		return nil, nil, "", unhandled("%s: mode %v is not APPEND or REMOVE", field, v["mode"]), false
	}
	r, p, fail, ok := e.glabGraphQLMR(c, v, "projectPath")
	if !ok {
		return nil, nil, "", fail, false
	}
	switch {
	case r.lacks(lacksOperationMode):
		return nil, nil, "", glabGraphQLInvalid(fmt.Sprintf("InputObject '%s' doesn't accept argument 'operationMode'", map[string]string{
			"mergeRequestSetReviewers": "MergeRequestSetReviewersInput", "mergeRequestSetAssignees": "MergeRequestSetAssigneesInput", "mergeRequestSetLabels": "MergeRequestSetLabelsInput",
		}[field])), false
	case !e.mayAdmin(r, p):
		return nil, nil, "", glabGraphQLDenied(field), false
	}
	return r, p, mode, response{}, true
}

// glabSetPeople answers PRSetReviewers (reviewers) and PRSetAssignees:
// usernames appended or removed. As GitLab does, a username that is no
// member of the project (nor already on the list) is skipped, and the
// answer is the merge request's list as it stands; an added reviewer
// leaves its note.
func glabSetPeople(reviewers bool) func(e *Engine, c *call, v graphQLVars) response {
	field, list := "mergeRequestSetAssignees", "assignees"
	if reviewers {
		field, list = "mergeRequestSetReviewers", "reviewers"
	}
	return func(e *Engine, c *call, v graphQLVars) response {
		usernames, err := v.strs("usernames")
		if err != nil {
			return unhandled("%s: %v", field, err)
		}
		r, p, mode, fail, ok := e.glabMutationMR(c, v, field)
		if !ok {
			return fail
		}
		current := &p.Assignees
		if reviewers {
			current = &p.Reviewers
		}
		var added []string
		for _, login := range usernames {
			switch {
			case mode == "REMOVE":
				*current = removeFold(*current, login)
				if reviewers {
					p.ChangeRequests = removeFold(p.ChangeRequests, login)
				}
			case !containsFold(*current, login) && (r.member(login) != nil || strings.EqualFold(login, e.viewer)):
				*current = append(*current, login)
				added = append(added, login)
			}
		}
		if reviewers && len(added) > 0 {
			p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "review_requested", Actor: e.viewer, At: glabNow(), Ref: strings.Join(added, ",")})
		}
		nodes := []map[string]any{}
		for _, login := range *current {
			nodes = append(nodes, map[string]any{"username": login})
		}
		return graphQLData(map[string]any{field: map[string]any{"errors": []string{}, "mergeRequest": map[string]any{list: glabFirstPage(nodes)}}})
	}
}

// glabSetLabels answers PRSetLabels: label ids appended or removed. As
// GitLab does, an id that is none of the project's labels is skipped.
func glabSetLabels(e *Engine, c *call, v graphQLVars) response {
	const field = "mergeRequestSetLabels"
	ids, err := v.strs("labelIds")
	if err != nil {
		return unhandled("%s: %v", field, err)
	}
	r, p, mode, fail, ok := e.glabMutationMR(c, v, field)
	if !ok {
		return fail
	}
	for _, id := range ids {
		label := r.labelByID(id)
		switch {
		case label == nil:
		case mode == "REMOVE":
			p.Labels = removeFold(p.Labels, label.Name)
		case !containsFold(p.Labels, label.Name):
			p.Labels = append(p.Labels, label.Name)
		}
	}
	nodes := []map[string]any{}
	for _, node := range gitlabLabelNodes(r, p.Labels) {
		nodes = append(nodes, map[string]any{"id": node["id"]})
	}
	return graphQLData(map[string]any{field: map[string]any{"errors": []string{}, "mergeRequest": map[string]any{"labels": glabFirstPage(nodes)}}})
}

// glabSetListFirst is the first: the app's set mutations select on the
// list they answer.
const glabSetListFirst = 100

// glabFirstPage is a connection of the first glabSetListFirst nodes, with
// hasNextPage when there are more, as GitLab answers the selection.
func glabFirstPage(nodes []map[string]any) map[string]any {
	more := len(nodes) > glabSetListFirst
	if more {
		nodes = nodes[:glabSetListFirst]
	}
	return map[string]any{"pageInfo": map[string]any{"hasNextPage": more}, "nodes": nodes}
}

// glabSetDraft answers PRSetDraft: the draft prefix added to or dropped
// from the title, with its note; a viewer who may not update the merge
// request is refused.
func glabSetDraft(e *Engine, c *call, v graphQLVars) response {
	const field = "mergeRequestSetDraft"
	draft, err := v.boolean("draft")
	if err != nil {
		return unhandled("%s: %v", field, err)
	}
	r, p, fail, ok := e.glabGraphQLMR(c, v, "projectPath")
	if !ok {
		return fail
	}
	if !e.mayUpdate(r, p) {
		return glabGraphQLDenied(field)
	}
	switch {
	case draft && !p.Draft:
		p.Title = "Draft: " + p.Title
	case !draft && p.Draft:
		p.Title = gitlabDraftTitle.ReplaceAllString(p.Title, "")
	}
	e.glabFollowDraftTitle(p, glabNow())
	return graphQLData(map[string]any{field: map[string]any{"errors": []string{}, "mergeRequest": map[string]any{"draft": p.Draft}}})
}

// glabSearchLabels answers SearchProjectLabels: the project's labels
// matching $search, 20 per page.
func glabSearchLabels(e *Engine, c *call, v graphQLVars) response {
	project, err := v.str("fullPath")
	if err != nil {
		return unhandled("SearchProjectLabels: %v", err)
	}
	search, _ := v["search"].(string)
	r := e.repoOn("gitlab", c.http.host, project)
	if r == nil {
		return graphQLData(map[string]any{"project": nil})
	}
	var names []string
	for _, label := range r.Labels {
		if matches(search, label.Name) {
			names = append(names, label.Name)
		}
	}
	return graphQLData(map[string]any{"project": map[string]any{"labels": githubPage(gitlabLabelNodes(r, names), 20)}})
}

// glabRereview asks a reviewer for a new review: their state reads
// unreviewed and their change request is cleared, with a
// review_requested note. A server that lacks the mutation and a malformed
// user id fail validation; a viewer who may not update the merge request
// is denied; a user who is not a reviewer is the payload's error. Each
// changes nothing.
func glabRereview(e *Engine, c *call, v graphQLVars) response {
	const field = "mergeRequestReviewerRereview"
	userID, err := v.str("userId")
	if err != nil {
		return unhandled("PRRereview: %v", err)
	}
	r, p, fail, ok := e.glabGraphQLMR(c, v, "projectPath")
	if !ok {
		return fail
	}
	if r.lacks(lacksRereview) {
		return glabGraphQLInvalid("Field 'mergeRequestReviewerRereview' doesn't exist on type 'Mutation'")
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(userID, "gid://gitlab/User/"), 10, 64)
	if err != nil || !strings.HasPrefix(userID, "gid://gitlab/User/") {
		return glabGraphQLInvalid(fmt.Sprintf("%q is not a valid ID for UserID.", userID))
	}
	if !e.mayUpdate(r, p) {
		return glabGraphQLDenied(field)
	}
	login, known := e.gitlabLoginOf(r, id)
	if !known || !containsFold(p.Reviewers, login) {
		return graphQLData(map[string]any{field: map[string]any{"errors": []string{"Reviewer not found"}}})
	}
	p.ChangeRequests = removeFold(p.ChangeRequests, login)
	if !containsFold(p.rerequested, login) {
		p.rerequested = append(p.rerequested, login)
	}
	p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "review_requested", Actor: e.viewer, At: glabNow(), Ref: login})
	return graphQLData(map[string]any{field: map[string]any{"errors": []string{}}})
}

// glabBypassChanges overrides the requested changes: the merge request's
// requested_changes check passes. A server that lacks the override fails
// validation; a viewer who cannot merge is the payload's error; each
// changes nothing.
func glabBypassChanges(e *Engine, c *call, v graphQLVars) response {
	r, p, fail, ok := e.glabGraphQLMR(c, v, "projectPath")
	if !ok {
		return fail
	}
	switch {
	case r.lacks(lacksOverride):
		return glabGraphQLInvalid("InputObject 'MergeRequestUpdateInput' doesn't accept argument 'overrideRequestedChanges'")
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
