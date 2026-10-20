package forgefake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GitLab handlers, served over HTTP at /gitlab/api/v4/. Output shapes
// follow the GitLab REST API; the fields emitted are the ones
// internal/git parses.

var (
	gitlabStates      = map[string]string{"open": "opened", "closed": "closed", "merged": "merged"}
	gitlabMergeStatus = map[string]string{"clean": "mergeable", "conflicts": "conflict", "checking": "checking"}
)

func glabNotFound(what string) response {
	return response{
		stdout: []byte(fmt.Sprintf(`{"message":"404 %s Not Found"}`, what)),
		stderr: "404 " + what + " Not Found",
		status: http.StatusNotFound,
		header: http.Header{"Content-Type": {"application/json"}},
	}
}

// glabOK answers a body with the given response headers ("Name: value")
// and a JSON content type unless one of them names another.
func glabOK(body response, headers ...string) response {
	body.header = http.Header{"Content-Type": {"application/json"}}
	for _, header := range headers {
		name, value, _ := strings.Cut(header, ": ")
		body.header.Set(name, value)
	}
	return body
}

// glabProject resolves an escaped project path or numeric id on the
// request's host.
func (e *Engine) glabProject(c *call, escaped string) (*Repo, response, bool) {
	project, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, unhandled("project %q is not path-escaped", escaped), false
	}
	r := e.repoOn("gitlab", c.http.host, project)
	if r == nil {
		return nil, glabNotFound("Project"), false
	}
	return r, response{}, true
}

func (e *Engine) glabMRFrom(c *call, m []string) (*Repo, *Pull, response, bool) {
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return nil, nil, fail, false
	}
	number, _ := strconv.Atoi(m[2])
	p := r.pull(number)
	if p == nil {
		return nil, nil, glabNotFound("Merge Request"), false
	}
	return r, p, response{}, true
}

func gitlabMRURL(r *Repo, p *Pull) string {
	return fmt.Sprintf("https://%s/%s/-/merge_requests/%d", r.Host, r.Project, p.Number)
}

func glabMR(e *Engine, c *call, m []string) response {
	r, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	return glabOK(jsonResponse(e.gitlabMRJSON(r, p)))
}

// gitlabMRJSON is the single merge request as GitLab answers it, the
// merge fields included. Callers hold mu.
func (e *Engine) gitlabMRJSON(r *Repo, p *Pull) map[string]any {
	var pipeline any
	if p.CI != nil {
		pipeline = map[string]any{
			"id":      p.CI.ID,
			"status":  p.CI.Status,
			"web_url": fmt.Sprintf("https://%s/%s/-/pipelines/%d", r.Host, r.Project, p.CI.ID),
		}
	}
	source := r.ID
	if p.HeadRepo != "" {
		source = p.headRepoID
	}
	var mergedAt, closedAt, mergeCommit, squashCommit any
	mergedBy, closedBy, mergeUser := "", "", ""
	switch p.State {
	case "merged":
		mergedAt, mergedBy, mergeUser, mergeCommit = p.MergedAt, p.MergedBy, p.MergedBy, p.MergeCommitSHA
		if p.squash {
			squashCommit = p.squashSHA
		}
	case "closed":
		closedAt, closedBy = p.ClosedAt, p.ClosedBy
	}
	armed := p.Readiness.AutoMerge
	if armed != nil {
		mergeUser = armed.By
	}
	return map[string]any{
		"id":                            r.ID*1000 + int64(p.Number),
		"iid":                           p.Number,
		"project_id":                    r.ID,
		"source_project_id":             source,
		"title":                         p.Title,
		"description":                   p.Body,
		"state":                         gitlabStates[p.State],
		"draft":                         p.Draft,
		"work_in_progress":              p.Draft,
		"source_branch":                 p.HeadRef,
		"target_branch":                 p.BaseRef,
		"sha":                           p.HeadSHA,
		"web_url":                       gitlabMRURL(r, p),
		"author":                        gitlabUser(p.Author, p.AuthorName),
		"changes_count":                 strconv.Itoa(len(diffFiles(p.Diff))),
		"has_conflicts":                 p.Mergeable == "conflicts",
		"detailed_merge_status":         e.gitlabDetailedStatus(r, p),
		"diff_refs":                     gitlabDiffRefs(p),
		"head_pipeline":                 pipeline,
		"user":                          map[string]any{"can_merge": gitlabRoles[r.Merge.Permission] >= gitlabRoles["developer"]},
		"merge_when_pipeline_succeeds":  armed != nil,
		"merge_user":                    gitlabUserOrNulls(mergeUser),
		"merged_at":                     mergedAt,
		"merged_by":                     gitlabUserOrNulls(mergedBy),
		"closed_at":                     closedAt,
		"closed_by":                     gitlabUserOrNulls(closedBy),
		"merge_commit_sha":              mergeCommit,
		"squash_commit_sha":             squashCommit,
		"squash":                        p.squash,
		"force_remove_source_branch":    p.forceRemove,
		"blocking_discussions_resolved": !r.Merge.DiscussionsMustResolve || !gitlabUnresolved(p),
		"merge_after":                   nil,
	}
}

// gitlabUserOrNulls is a merge request's user field: the user, or an
// object of nulls when there is none, as GitLab answers it.
func gitlabUserOrNulls(username string) map[string]any {
	if username == "" {
		return map[string]any{"id": nil, "username": nil, "name": nil}
	}
	return gitlabUser(username, "")
}

// gitlabUnresolved is whether a resolvable thread is open.
func gitlabUnresolved(p *Pull) bool {
	return slices.ContainsFunc(p.Threads, func(t Thread) bool { return !t.Resolved })
}

// gitlabDetailedStatus is GitLab's detailed_merge_status: the first
// merge check that fails, in GitLab's order (open, checking, draft,
// conflicts, rebase, threads the project requires resolved, the pipeline
// it requires, approvals), else mergeable.
func (e *Engine) gitlabDetailedStatus(r *Repo, p *Pull) string {
	switch {
	case p.State != "open":
		return "not_open"
	case p.Mergeable == "checking":
		return "checking"
	case p.Draft:
		return "draft_status"
	case p.Mergeable == "conflicts":
		return "conflict"
	case p.Readiness.Behind:
		return "need_rebase"
	case r.Merge.DiscussionsMustResolve && gitlabUnresolved(p):
		return "discussions_not_resolved"
	}
	if r.Merge.PipelineMustSucceed {
		switch {
		case p.CI == nil:
			return "ci_must_pass"
		case p.CI.Status == "running" || p.CI.Status == "pending":
			return "ci_still_running"
		case p.CI.Status != "success" && p.CI.Status != "skipped":
			return "ci_must_pass"
		}
	}
	if gitlabApprovalsLeft(p) > 0 {
		return "not_approved"
	}
	return "mergeable"
}

// gitlabApprovalsLeft is the approvals the merge request still needs.
func gitlabApprovalsLeft(p *Pull) int {
	return max(p.Readiness.RequiredApprovals-len(p.Reviews), 0)
}

// gitlabViewerApproved is whether the viewer's approval stands.
func gitlabViewerApproved(p *Pull, viewer string) bool {
	return slices.ContainsFunc(p.Reviews, func(r Review) bool { return strings.EqualFold(r.Author, viewer) })
}

// gitlabCanApprove is user_can_approve: the readiness override, else an
// open merge request the viewer has not approved yet and a role of
// developer or above.
func (e *Engine) gitlabCanApprove(r *Repo, p *Pull) bool {
	if p.State != "open" || gitlabViewerApproved(p, e.viewer) {
		return false
	}
	if can := p.Readiness.ViewerCanApprove; can != nil {
		return *can
	}
	return gitlabRoles[r.Merge.Permission] >= gitlabRoles["developer"]
}

func gitlabDiffRefs(p *Pull) any {
	if p.DiffPending {
		return nil
	}
	return map[string]any{"base_sha": p.BaseSHA, "head_sha": p.HeadSHA, "start_sha": p.StartSHA}
}

func glabApprovals(e *Engine, c *call, m []string) response {
	r, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	return glabOK(jsonResponse(e.gitlabApprovalsJSON(r, p)))
}

// gitlabApprovalsJSON is the merge request's approval state. With no
// approvals required (GitLab Free) any approval approves it.
func (e *Engine) gitlabApprovalsJSON(r *Repo, p *Pull) map[string]any {
	approved := []map[string]any{}
	for _, review := range p.Reviews {
		approved = append(approved, map[string]any{
			"user":        gitlabUser(review.Author, review.AuthorName),
			"approved_at": review.SubmittedAt,
		})
	}
	left := gitlabApprovalsLeft(p)
	return map[string]any{
		"approvals_required": p.Readiness.RequiredApprovals,
		"approvals_left":     left,
		"approved":           len(approved) > 0 && left == 0,
		"approved_by":        approved,
		"user_has_approved":  gitlabViewerApproved(p, e.viewer),
		"user_can_approve":   e.gitlabCanApprove(r, p),
	}
}

// glabPage reads per_page and page, refusing any other query parameter
// the route does not list.
func glabPage(rawQuery string, allowed ...string) (url.Values, int, int, error) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, 0, 0, err
	}
	for key := range query {
		if key != "per_page" && key != "page" && !slices.Contains(allowed, key) {
			return nil, 0, 0, fmt.Errorf("query parameter %q is not implemented", key)
		}
	}
	perPage, page := 20, 1
	if raw := query.Get("per_page"); raw != "" {
		if perPage, err = strconv.Atoi(raw); err != nil || perPage <= 0 || perPage > 100 {
			return nil, 0, 0, fmt.Errorf("per_page %q is not 1..100", raw)
		}
	}
	if raw := query.Get("page"); raw != "" {
		if page, err = strconv.Atoi(raw); err != nil || page <= 0 {
			return nil, 0, 0, fmt.Errorf("page %q is not positive", raw)
		}
	}
	return query, perPage, page, nil
}

// paginate slices items for page and answers the X-Next-Page header.
func paginate[T any](items []T, perPage, page int) ([]T, string) {
	start := min((page-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	next := ""
	if end < len(items) {
		next = strconv.Itoa(page + 1)
	}
	return items[start:end], next
}

// glabProjectRead answers `GET projects/P`: the project's identity, its
// merge settings and the viewer's access, conditional on its ETag as
// GitLab's is.
func glabProjectRead(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return fail
	}
	settings := r.Merge
	return glabOK(jsonResponse(map[string]any{
		"id":                                    r.ID,
		"full_name":                             r.Project,
		"path_with_namespace":                   r.Project,
		"merge_method":                          settings.MergeMethod,
		"squash_option":                         settings.SquashOption,
		"remove_source_branch_after_merge":      settings.DeleteBranchOnMerge,
		"only_allow_merge_if_pipeline_succeeds": settings.PipelineMustSucceed,
		"only_allow_merge_if_all_discussions_are_resolved": settings.DiscussionsMustResolve,
		"permissions": map[string]any{
			"project_access": map[string]any{"access_level": gitlabRoles[settings.Permission], "notification_level": 3},
			"group_access":   nil,
		},
	}))
}

func glabDiscussions(e *Engine, c *call, m []string) response {
	_, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	_, perPage, page, err := glabPage(m[3])
	if err != nil {
		return unhandled("%v", err)
	}
	discussions := gitlabDiscussions(p)
	items, next := paginate(discussions, perPage, page)
	return glabOK(jsonResponse(items), "X-Next-Page: "+next, "X-Page: "+strconv.Itoa(page))
}

func gitlabDiscussions(p *Pull) []map[string]any {
	out := make([]map[string]any, 0, len(p.Threads)+len(p.Comments))
	for _, thread := range p.Threads {
		headSHA := p.HeadSHA
		if thread.Outdated {
			headSHA = p.BaseSHA
		}
		position := map[string]any{
			"base_sha":      p.BaseSHA,
			"start_sha":     p.StartSHA,
			"head_sha":      headSHA,
			"old_path":      thread.Path,
			"new_path":      thread.Path,
			"position_type": "text",
			"old_line":      nil,
			"new_line":      nil,
		}
		lineKey := "new_line"
		if thread.Side == "left" {
			lineKey = "old_line"
		}
		if thread.Side == "file" {
			position["position_type"] = "file"
		} else {
			position[lineKey] = *thread.Line
		}
		if thread.StartLine != nil {
			position["line_range"] = map[string]any{
				"start": map[string]any{lineKey: *thread.StartLine, "type": "new"},
				"end":   map[string]any{lineKey: *thread.Line, "type": "new"},
			}
		}
		notes := make([]map[string]any, 0, len(thread.Comments))
		for _, comment := range thread.Comments {
			notes = append(notes, gitlabNote(comment, map[string]any{
				"type": "DiffNote", "position": position, "resolvable": true, "resolved": thread.Resolved,
			}))
		}
		out = append(out, map[string]any{"id": thread.ID, "individual_note": false, "notes": notes})
	}
	for _, comment := range p.Comments {
		out = append(out, map[string]any{
			"id":              fmt.Sprintf("%040x", comment.ID),
			"individual_note": true,
			"notes":           []map[string]any{gitlabNote(comment, map[string]any{"resolvable": false})},
		})
	}
	// The system notes, oldest first, each its own discussion.
	events := slices.Clone(p.Events)
	slices.SortStableFunc(events, func(a, b Event) int {
		at, _ := time.Parse(time.RFC3339, a.At)
		bt, _ := time.Parse(time.RFC3339, b.At)
		return at.Compare(bt)
	})
	for _, event := range events {
		note := gitlabNote(Comment{ID: event.ID, Author: event.Actor, AuthorName: event.ActorName, Body: gitlabNoteBody(event), CreatedAt: event.At}, map[string]any{"resolvable": false})
		note["system"] = true
		out = append(out, map[string]any{"id": fmt.Sprintf("%040x", event.ID), "individual_note": true, "notes": []map[string]any{note}})
	}
	return out
}

// gitlabNoteBody is an event's system note in the phrasing GitLab writes
// (app/services/system_notes/; docs/references/forge-api-measurements.md).
func gitlabNoteBody(event Event) string {
	switch event.Kind {
	case "approved":
		return "approved this merge request"
	case "unapproved":
		return "unapproved this merge request"
	case "marked_draft":
		return "marked this merge request as **draft**"
	case "marked_ready":
		return "marked this merge request as **ready**"
	case "auto_merge_set":
		return "enabled an automatic merge when all merge checks for " + event.Ref + " pass"
	case "auto_merge_canceled":
		if event.Text != "" {
			return "aborted the automatic merge because " + event.Text
		}
		return "canceled the automatic merge"
	case "base_changed":
		return "changed target branch from `" + event.Text + "` to `" + event.Ref + "`"
	case "review_requested":
		var names []string
		for _, name := range strings.Split(event.Ref, ",") {
			if name = strings.TrimSpace(name); name != "" {
				names = append(names, "@"+name)
			}
		}
		switch len(names) {
		case 0, 1:
			return "requested review from " + strings.Join(names, "")
		case 2:
			return "requested review from " + names[0] + " and " + names[1]
		}
		return "requested review from " + strings.Join(names[:len(names)-1], ", ") + ", and " + names[len(names)-1]
	}
	// merged, closed, reopened: the notes of a GitLab from before state
	// changes became resource events.
	return event.Kind
}

// gitlabUser is GitLab's user summary, which always carries a name.
func gitlabUser(username, name string) map[string]any {
	return map[string]any{"username": username, "name": name}
}

func gitlabNote(comment Comment, extra map[string]any) map[string]any {
	note := map[string]any{
		"id":         comment.ID,
		"body":       comment.Body,
		"system":     false,
		"created_at": comment.CreatedAt,
		"author":     gitlabUser(comment.Author, comment.AuthorName),
	}
	for key, value := range extra {
		note[key] = value
	}
	return note
}

// glabMRList answers the three merge request lists the app makes, of the
// project the path names: the open MRs from a source branch name (a
// fork's included, as source_branch matches them), recently merged MRs,
// and named MRs in any state (iids[] with state=all). Items are the full
// view.
func glabMRList(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return fail
	}
	query, perPage, page, err := glabPage(m[2], "state", "source_branch", "order_by", "sort", "iids[]")
	if err != nil {
		return unhandled("%v", err)
	}
	if iids, named := query["iids[]"]; named {
		return glabMRsByIID(r, query, iids, perPage, page)
	}
	if order := query.Get("order_by"); order != "" && order != "updated_at" {
		return unhandled("order_by=%q is not implemented", order)
	}
	if sortOrder := query.Get("sort"); sortOrder != "" && sortOrder != "desc" {
		return unhandled("sort=%q is not implemented", sortOrder)
	}
	state := query.Get("state")
	if state != "opened" && state != "merged" {
		return unhandled("state=%q is not implemented", state)
	}
	branch := query.Get("source_branch")
	var matches []map[string]any
	// Fixture order is creation order; the API's default and the
	// updated_at order the app asks for are both newest first.
	for i := len(r.Pulls) - 1; i >= 0; i-- {
		p := &r.Pulls[i]
		if gitlabStates[p.State] != state || (branch != "" && p.HeadRef != branch) {
			continue
		}
		matches = append(matches, gitlabMRListItem(r, p))
	}
	items, next := paginate(matches, perPage, page)
	if items == nil {
		items = []map[string]any{}
	}
	return glabOK(jsonResponse(items), "X-Next-Page: "+next)
}

// glabMRsByIID answers the merge requests iids names, in any state, newest
// first. An iid the project does not have is absent, as GitLab answers it.
func glabMRsByIID(r *Repo, query url.Values, iids []string, perPage, page int) response {
	if state := query.Get("state"); state != "all" {
		return unhandled("iids[] with state=%q is not implemented", state)
	}
	for _, key := range []string{"source_branch", "order_by", "sort"} {
		if query.Has(key) {
			return unhandled("iids[] with %s is not implemented", key)
		}
	}
	wanted := make(map[int]bool, len(iids))
	for _, raw := range iids {
		iid, err := strconv.Atoi(raw)
		if err != nil || iid <= 0 {
			return unhandled("iids[] %q is not a positive integer", raw)
		}
		wanted[iid] = true
	}
	var matches []map[string]any
	for i := len(r.Pulls) - 1; i >= 0; i-- {
		if p := &r.Pulls[i]; wanted[p.Number] {
			matches = append(matches, gitlabMRListItem(r, p))
		}
	}
	items, next := paginate(matches, perPage, page)
	if items == nil {
		items = []map[string]any{}
	}
	return glabOK(jsonResponse(items), "X-Next-Page: "+next)
}

// gitlabMRListItem is one merge request as the list endpoint answers it.
func gitlabMRListItem(r *Repo, p *Pull) map[string]any {
	var mergedAt any
	if p.MergedAt != "" {
		mergedAt = p.MergedAt
	}
	source := r.ID
	if p.HeadRepo != "" {
		source = p.headRepoID
	}
	return map[string]any{
		"iid":               p.Number,
		"project_id":        r.ID,
		"source_project_id": source,
		"title":             p.Title,
		"state":             gitlabStates[p.State],
		"web_url":           gitlabMRURL(r, p),
		"source_branch":     p.HeadRef,
		"target_branch":     p.BaseRef,
		"sha":               p.HeadSHA,
		"merged_at":         mergedAt,
	}
}

func glabPipelineJobs(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return fail
	}
	id, _ := strconv.ParseInt(m[2], 10, 64)
	pipeline := r.pipeline(id)
	if pipeline == nil {
		return glabNotFound("Pipeline")
	}
	_, perPage, page, err := glabPage(m[3])
	if err != nil {
		return unhandled("%v", err)
	}
	jobs := make([]map[string]any, 0, len(pipeline.Jobs))
	for _, job := range pipeline.Jobs {
		var startedAt, duration any
		if job.StartedAt != "" {
			startedAt = job.StartedAt
			if seconds, ok := jobSeconds(job); ok {
				duration = seconds
			}
		}
		jobs = append(jobs, map[string]any{
			"id":            job.ID,
			"name":          job.Name,
			"stage":         job.Stage,
			"status":        job.Status,
			"duration":      duration,
			"web_url":       fmt.Sprintf("https://%s/%s/-/jobs/%d", r.Host, r.Project, job.ID),
			"allow_failure": job.AllowFailure,
			"started_at":    startedAt,
		})
	}
	// The API lists jobs newest first.
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i]["id"].(int64) > jobs[j]["id"].(int64) })
	items, next := paginate(jobs, perPage, page)
	return glabOK(jsonResponse(items), "X-Next-Page: "+next)
}

func jobSeconds(job Job) (float64, bool) {
	started, err1 := time.Parse(time.RFC3339, job.StartedAt)
	completed, err2 := time.Parse(time.RFC3339, job.CompletedAt)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return completed.Sub(started).Seconds(), true
}

func glabJobTrace(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return fail
	}
	id, _ := strconv.ParseInt(m[2], 10, 64)
	job := r.job(id)
	if job == nil || job.StartedAt == "" || job.LogWithheld {
		return glabNotFound("Job")
	}
	resp := glabOK(response{ranges: rangeTrace}, "Content-Type: text/plain")
	if job.Status == "running" {
		// A running trace ignores Range: every answer is the whole body.
		resp.ranges = rangeIgnored
	}
	if job.Synthetic == nil {
		resp.body = bytesBody(job.Log)
		return resp
	}
	units := -1
	if job.Status == "running" {
		// Each request of a running trace serves GrowLines more than the
		// one before it.
		served := e.traceRequests[job.ID]
		e.traceRequests[job.ID] = served + 1
		units = min(job.Synthetic.RunningLines+served*job.Synthetic.GrowLines, syntheticUnits("gitlab", job.Synthetic))
	}
	resp.body = newSyntheticBody("gitlab", job, units)
	resp.etag = syntheticETag("gitlab", job, units)
	return resp
}

func glabUpload(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return fail
	}
	name, err := url.PathUnescape(m[3])
	if err != nil {
		return unhandled("upload name %q is not path-escaped", m[3])
	}
	for _, attachment := range r.Attachments {
		if attachment.Secret == m[2] && attachment.Filename == name {
			return glabOK(response{stdout: slices.Clone(attachment.content)}, "Content-Type: "+attachment.ContentType)
		}
	}
	return glabNotFound("File")
}

// glabMRCreate answers `glab mr create --title T --description D --yes
// --no-editor [--target-branch B] [--draft]` with the new merge request's
// URL. The app always passes --yes and --no-editor, the non-interactive
// spelling; without them glab would prompt, which this fake cannot. A draft
// gets GitLab's "Draft: " title prefix. GitLab refuses a second open merge
// request from the same source branch.
func glabMRCreate(e *Engine, c *call) response {
	if len(c.positional) != 0 {
		return unhandled("mr create takes no positional arguments")
	}
	if !c.has("yes") || !c.has("no-editor") {
		return unhandled("mr create without --yes and --no-editor prompts")
	}
	if !c.has("title") || !c.has("description") {
		return unhandled("mr create without --title and --description prompts")
	}
	r, head, err := e.createTarget("gitlab", c)
	if err != nil {
		return response{exit: 1, stderr: err.Error() + "\n"}
	}
	req := createRequest{title: c.flag("title"), body: c.flag("description"), base: defaultString(c.flag("target-branch"), defaultBaseRef), draft: c.has("draft")}
	if strings.TrimSpace(req.title) == "" {
		return response{exit: 1, stderr: "failed to create merge request: 400 {message: {title: [can't be blank]}}\n"}
	}
	if req.draft {
		req.title = "Draft: " + req.title
	}
	if head.Branch == req.base {
		return response{exit: 1, stderr: "failed to create merge request: 400 {message: [You can't use same project/branch for source and target]}\n"}
	}
	for i := range r.Pulls {
		if p := &r.Pulls[i]; p.State == "open" && p.HeadRef == head.Branch {
			return response{exit: 1, stderr: fmt.Sprintf("failed to create merge request: 409 {message: [Another open merge request already exists for this source branch: !%d]}\n", p.Number)}
		}
	}
	pull, err := e.addPull(r, head, req)
	if err != nil {
		return response{exit: 1, stderr: "ao-mockforge: " + err.Error() + "\n"}
	}
	return response{stdout: []byte(gitlabMRURL(r, pull) + "\n")}
}

// glabStatus is a GitLab REST refusal: status and {"message": message}.
func glabStatus(status int, message string) response {
	out, _ := json.Marshal(map[string]any{"message": message})
	return response{stdout: out, stderr: message, status: status, header: http.Header{"Content-Type": {"application/json"}}}
}

// glabNow is the time an action happens, as GitLab spells it.
func glabNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// glabSHAMismatch is GitLab's 409 for a sha that is not the head.
func glabSHAMismatch(p *Pull) response {
	return glabStatus(http.StatusConflict, "SHA does not match HEAD of source branch: "+p.HeadSHA)
}

// glabApprove approves the merge request as the viewer: `{sha}` pins the
// head (409 when it moved). A viewer GitLab does not take an approval
// from, the merge request's own approver included, is refused with 401.
// The approval joins approved_by and leaves its system note.
func glabApprove(e *Engine, c *call, m []string) response {
	var in struct {
		SHA string `json:"sha"`
	}
	if err := decodeRESTBody(c, &in, true); err != nil {
		return unhandled("%v", err)
	}
	r, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	switch {
	case !e.gitlabCanApprove(r, p):
		return glabStatus(http.StatusUnauthorized, "401 Unauthorized")
	case in.SHA != "" && in.SHA != p.HeadSHA:
		return glabSHAMismatch(p)
	}
	now := glabNow()
	p.Reviews = append(p.Reviews, Review{ID: e.ids.next(), Author: e.viewer, State: "APPROVED", SubmittedAt: now, CommitSHA: p.HeadSHA})
	p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "approved", Actor: e.viewer, At: now})
	resp := glabOK(jsonResponse(e.gitlabApprovalsJSON(r, p)))
	resp.status = http.StatusCreated
	return resp
}

// glabUnapprove withdraws the viewer's approval (404 when there is none)
// and leaves its system note.
func glabUnapprove(e *Engine, c *call, m []string) response {
	if err := noRESTBody(c); err != nil {
		return unhandled("%v", err)
	}
	r, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	if !gitlabViewerApproved(p, e.viewer) {
		return glabStatus(http.StatusNotFound, "404 Not Found")
	}
	p.Reviews = slices.DeleteFunc(p.Reviews, func(r Review) bool { return strings.EqualFold(r.Author, e.viewer) })
	p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "unapproved", Actor: e.viewer, At: glabNow()})
	resp := glabOK(jsonResponse(e.gitlabApprovalsJSON(r, p)))
	resp.status = http.StatusCreated
	return resp
}

// gitlabArmMethod is the method an auto-merge of the project merges
// with: squash when asked, else the project's merge method.
func gitlabArmMethod(r *Repo, squash bool) string {
	switch {
	case squash:
		return "squash"
	case r.Merge.MergeMethod == "ff":
		return "rebase"
	}
	return "merge"
}

// glabMerge answers `PUT .../merge` with `{sha, squash,
// should_remove_source_branch, auto_merge, merge_when_pipeline_succeeds}`,
// either auto-merge key alone or both together.
// A merge request that is not open is refused with 405, a viewer who
// cannot merge with 401, a moved head with 409. Auto-merge on a merge
// request that cannot merge yet arms it (the system note names the head)
// and answers it with merge_when_pipeline_succeeds; a conflict refuses
// it (405). A mergeable merge request merges now, auto-merge asked or
// not: merged by the viewer now with a new merge commit (and a squash
// commit when squashed), its source branch removed when asked. Anything
// else that cannot merge is refused with 405.
func glabMerge(e *Engine, c *call, m []string) response {
	var in struct {
		SHA                       string `json:"sha"`
		Squash                    *bool  `json:"squash"`
		ShouldRemoveSourceBranch  *bool  `json:"should_remove_source_branch"`
		AutoMerge                 bool   `json:"auto_merge"`
		MergeWhenPipelineSucceeds bool   `json:"merge_when_pipeline_succeeds"`
	}
	if err := decodeRESTBody(c, &in, true); err != nil {
		return unhandled("%v", err)
	}
	r, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	switch {
	case p.State != "open":
		return glabStatus(http.StatusMethodNotAllowed, "405 Method Not Allowed")
	case gitlabRoles[r.Merge.Permission] < gitlabRoles["developer"]:
		return glabStatus(http.StatusUnauthorized, "401 Unauthorized")
	case in.SHA != "" && in.SHA != p.HeadSHA:
		return glabSHAMismatch(p)
	}
	squash := p.squash
	switch {
	case r.Merge.SquashOption == "always":
		squash = true
	case r.Merge.SquashOption == "never":
		squash = false
	case in.Squash != nil:
		squash = *in.Squash
	}
	remove := p.forceRemove
	if in.ShouldRemoveSourceBranch != nil {
		remove = *in.ShouldRemoveSourceBranch
	}
	status := e.gitlabDetailedStatus(r, p)
	if status != "mergeable" {
		if !(in.AutoMerge || in.MergeWhenPipelineSucceeds) || status == "conflict" {
			return glabStatus(http.StatusMethodNotAllowed, "405 Method Not Allowed")
		}
		now := glabNow()
		p.squash, p.forceRemove = squash, remove
		p.Readiness.AutoMerge = &AutoMerge{Method: gitlabArmMethod(r, squash), By: e.viewer, At: now}
		p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "auto_merge_set", Actor: e.viewer, At: now, Ref: p.HeadSHA})
		return glabOK(jsonResponse(e.gitlabMRJSON(r, p)))
	}
	now := glabNow()
	p.squash, p.forceRemove = squash, remove
	p.State, p.MergedAt, p.MergedBy, p.ClosedAt, p.ClosedBy = "merged", now, e.viewer, now, e.viewer
	p.MergeCommitSHA = e.ids.sha()
	if squash {
		p.squashSHA = e.ids.sha()
	}
	p.Readiness.AutoMerge = nil
	if remove && p.HeadRepo == "" {
		r.deleteBranch(p.HeadRef)
	}
	return glabOK(jsonResponse(e.gitlabMRJSON(r, p)))
}

// glabCancelAutoMerge disarms an armed auto-merge (409 when none is
// armed, 403 for a viewer who cannot merge) and answers the merge request
// with its system note added.
func glabCancelAutoMerge(e *Engine, c *call, m []string) response {
	if err := noRESTBody(c); err != nil {
		return unhandled("%v", err)
	}
	r, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	switch {
	case gitlabRoles[r.Merge.Permission] < gitlabRoles["developer"]:
		return glabStatus(http.StatusForbidden, "403 Forbidden")
	case p.Readiness.AutoMerge == nil:
		return glabStatus(http.StatusConflict, "409 Conflict: Can't cancel the automatic merge")
	}
	p.Readiness.AutoMerge = nil
	p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: "auto_merge_canceled", Actor: e.viewer, At: glabNow()})
	resp := glabOK(jsonResponse(e.gitlabMRJSON(r, p)))
	resp.status = http.StatusCreated
	return resp
}

// glabRebase rebases the source branch onto the target: 202
// {"rebase_in_progress": true}, the head moved to a new commit and no
// longer behind. A viewer who cannot push, or a merge request that is
// not open, is refused with 403.
func glabRebase(e *Engine, c *call, m []string) response {
	if err := noRESTBody(c); err != nil {
		return unhandled("%v", err)
	}
	r, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	if p.State != "open" || gitlabRoles[r.Merge.Permission] < gitlabRoles["developer"] {
		return glabStatus(http.StatusForbidden, "403 Forbidden - Cannot push to source branch")
	}
	p.HeadSHA = e.ids.sha()
	p.Readiness.Behind = false
	resp := glabOK(jsonResponse(map[string]any{"rebase_in_progress": true}))
	resp.status = http.StatusAccepted
	return resp
}

// glabUpdateMR answers `PUT .../merge_requests/N` with `{title}`, the one
// edit the app makes: the title is set and the draft state follows its
// prefix, leaving the system note of the change.
func glabUpdateMR(e *Engine, c *call, m []string) response {
	var in struct {
		Title *string `json:"title"`
	}
	if err := decodeRESTBody(c, &in, false); err != nil {
		return unhandled("%v", err)
	}
	if in.Title == nil {
		return unhandled("a merge request update without a title is not implemented")
	}
	r, p, fail, ok := e.glabMRFrom(c, m)
	if !ok {
		return fail
	}
	switch {
	case strings.TrimSpace(*in.Title) == "":
		out, _ := json.Marshal(map[string]any{"message": map[string]any{"title": []string{"can't be blank"}}})
		return response{stdout: out, stderr: "400 title can't be blank", status: http.StatusBadRequest, header: http.Header{"Content-Type": {"application/json"}}}
	case gitlabRoles[r.Merge.Permission] < gitlabRoles["developer"] && !strings.EqualFold(p.Author, e.viewer):
		return glabStatus(http.StatusForbidden, "403 Forbidden")
	}
	p.Title = *in.Title
	if draft := gitlabDraftTitle.MatchString(p.Title); draft != p.Draft {
		p.Draft = draft
		kind := "marked_ready"
		if draft {
			kind = "marked_draft"
		}
		p.Events = append(p.Events, Event{ID: e.ids.next(), Kind: kind, Actor: e.viewer, At: glabNow()})
	}
	return glabOK(jsonResponse(e.gitlabMRJSON(r, p)))
}
