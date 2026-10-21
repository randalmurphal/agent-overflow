package forgefake

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Pipelines on any ref: GitLab's pipelines list and single pipeline,
// GitHub's workflow runs list and single run. Both answer a repository's
// Pipelines and its pulls' CI, newest (highest id) first.

var (
	pipelineRefKinds = map[string]bool{"branch": true, "tag": true, "merge_request": true}
	// gitlabMergeRequestPipelineRef is a GitLab merge request pipeline's
	// ref: the merge request's head, merged result or train entry.
	gitlabMergeRequestPipelineRef = regexp.MustCompile(`^refs/merge-requests/(\d+)/(?:head|merge|train)$`)
	// githubRunStatusFilters are the runs list's status filter values the
	// fake answers: a run status or a conclusion.
	githubRunStatusFilters = map[string]bool{"completed": true, "in_progress": true, "queued": true, "success": true, "failure": true, "cancelled": true, "skipped": true}
	// gitlabPipelineStatuses are GitLab's pipeline statuses, each a valid
	// status filter.
	gitlabPipelineStatuses = map[string]bool{
		"created": true, "waiting_for_resource": true, "preparing": true, "pending": true, "running": true, "success": true,
		"failed": true, "canceled": true, "skipped": true, "manual": true, "scheduled": true,
	}
	githubRunSources = map[string]string{"branch": "push", "tag": "release", "merge_request": "pull_request"}
	gitlabSources    = map[string]string{"branch": "push", "tag": "push", "merge_request": "merge_request_event"}
)

// normalizeRun fills and validates what a pipeline ran on. pull is the
// pull whose CI it is, nil for one of the repository's Pipelines.
func (p *Pipeline) normalizeRun(forge string, pull *Pull, ids *idSource) error {
	kind, ref, sha := "branch", "main", ""
	if pull != nil {
		kind, sha = "merge_request", pull.HeadSHA
	}
	p.RefKind = defaultString(p.RefKind, kind)
	if !pipelineRefKinds[p.RefKind] {
		return fmt.Errorf("refKind %q is not branch, tag or merge_request", p.RefKind)
	}
	switch {
	case pull != nil && forge == "gitlab" && p.RefKind == "merge_request":
		ref = fmt.Sprintf("refs/merge-requests/%d/head", pull.Number)
	case pull != nil:
		ref = pull.HeadRef
	case p.RefKind != "branch" && p.Ref == "":
		return fmt.Errorf("a %s pipeline names its ref", p.RefKind)
	}
	p.Ref = defaultString(p.Ref, ref)
	if forge == "gitlab" && (p.RefKind == "merge_request") != gitlabMergeRequestPipelineRef.MatchString(p.Ref) {
		return fmt.Errorf("ref %q: a GitLab pipeline is a merge_request pipeline exactly when its ref is refs/merge-requests/N/head, merge or train", p.Ref)
	}
	if sha == "" {
		sha = ids.sha()
	}
	p.SHA = defaultString(p.SHA, sha)
	sources := githubRunSources
	if forge == "gitlab" {
		sources = gitlabSources
	}
	p.Source = defaultString(p.Source, sources[p.RefKind])
	p.Title = defaultString(p.Title, p.Name)
	p.CreatedAt = defaultString(p.CreatedAt, "2026-01-01T00:00:00Z")
	p.UpdatedAt = defaultString(p.UpdatedAt, p.CreatedAt)
	for name, value := range map[string]string{"createdAt": p.CreatedAt, "updatedAt": p.UpdatedAt} {
		if _, err := time.Parse(time.RFC3339, value); err != nil {
			return fmt.Errorf("%s %q is not RFC 3339", name, value)
		}
	}
	return nil
}

// normalizePipelines fills the repository's Pipelines and refuses a
// pipeline id that two pipelines share.
func (r *Repo) normalizePipelines(ids *idSource) error {
	for i := range r.Pipelines {
		pipeline := &r.Pipelines[i]
		if err := pipeline.normalize(ids); err != nil {
			return fmt.Errorf("pipelines[%d]: %w", i, err)
		}
		if err := pipeline.normalizeRun(r.Forge, nil, ids); err != nil {
			return fmt.Errorf("pipelines[%d]: %w", i, err)
		}
	}
	seen := map[int64]bool{}
	for _, pipeline := range r.pipelines() {
		if seen[pipeline.ID] {
			return fmt.Errorf("pipeline id %d is declared twice", pipeline.ID)
		}
		seen[pipeline.ID] = true
	}
	return nil
}

// pipelines is every pipeline of the repository, its pulls' CI and its
// Pipelines, newest (highest id) first.
func (r *Repo) pipelines() []*Pipeline {
	var out []*Pipeline
	for i := range r.Pulls {
		if ci := r.Pulls[i].CI; ci != nil {
			out = append(out, ci)
		}
	}
	for i := range r.Pipelines {
		out = append(out, &r.Pipelines[i])
	}
	slices.SortStableFunc(out, func(a, b *Pipeline) int { return cmp.Compare(b.ID, a.ID) })
	return out
}

// ghRuns answers the REST workflow runs list: filtered by branch (the
// run's head_branch) and status (a run status or a conclusion),
// paginated by per_page (30 by default, at most 100) and page with a Link
// rel="next" on the fake's own listener.
func ghRuns(e *Engine, c *call, m []string) response {
	query, perPage, page, err := glabPage(m[2], "branch", "status")
	if err != nil {
		return unhandled("%v", err)
	}
	if query.Get("per_page") == "" {
		perPage = 30
	}
	status := query.Get("status")
	if query.Has("status") && !githubRunStatusFilters[status] {
		return unhandled("status=%q is not implemented", status)
	}
	r := e.repoOn("github", c.http.host, m[1])
	if r == nil {
		return ghHTTPNotFound()
	}
	runs := []map[string]any{}
	for _, pipeline := range r.pipelines() {
		run := githubRunJSON(r, pipeline)
		if query.Has("branch") && pipeline.Ref != query.Get("branch") {
			continue
		}
		if status != "" && run["status"] != status && run["conclusion"] != status {
			continue
		}
		runs = append(runs, run)
	}
	items, next := paginate(runs, perPage, page)
	resp := jsonResponse(map[string]any{"total_count": len(runs), "workflow_runs": items})
	if next != "" {
		query.Set("per_page", strconv.Itoa(perPage))
		query.Set("page", next)
		link := fmt.Sprintf("%s/github/rest/repos/%s/actions/runs?%s", c.http.base, r.Project, query.Encode())
		resp.header.Set("Link", "<"+link+`>; rel="next"`)
	}
	return resp
}

// ghRun answers one workflow run.
func ghRun(e *Engine, c *call, m []string) response {
	r := e.repoOn("github", c.http.host, m[1])
	if r == nil {
		return ghHTTPNotFound()
	}
	id, _ := strconv.ParseInt(m[2], 10, 64)
	pipeline := r.pipeline(id)
	if pipeline == nil {
		return ghHTTPNotFound()
	}
	return jsonResponse(githubRunJSON(r, pipeline))
}

// githubRunJSON is a workflow run as the REST API answers it. A
// pull_request run lists the repository's own open pulls whose head is
// its branch, as GitHub does (a fork's run lists none).
func githubRunJSON(r *Repo, pipeline *Pipeline) map[string]any {
	status, conclusion := githubRESTState(pipeline.Status)
	pulls := []map[string]any{}
	if strings.HasPrefix(pipeline.Source, "pull_request") {
		for i := range r.Pulls {
			p := &r.Pulls[i]
			if p.State == "open" && p.HeadRepo == "" && p.HeadRef == pipeline.Ref {
				pulls = append(pulls, map[string]any{
					"number": p.Number,
					"head":   map[string]any{"ref": p.HeadRef, "sha": p.HeadSHA},
					"base":   map[string]any{"ref": p.BaseRef, "sha": p.StartSHA},
				})
			}
		}
	}
	return map[string]any{
		"id":            pipeline.ID,
		"name":          pipeline.Name,
		"display_title": pipeline.Title,
		"head_branch":   pipeline.Ref,
		"head_sha":      pipeline.SHA,
		"event":         pipeline.Source,
		"status":        status,
		"conclusion":    conclusion,
		"created_at":    pipeline.CreatedAt,
		"updated_at":    pipeline.UpdatedAt,
		"html_url":      fmt.Sprintf("https://%s/%s/actions/runs/%d", r.Host, r.Project, pipeline.ID),
		"pull_requests": pulls,
	}
}

// glabPipelines answers the project's pipelines list: filtered by ref
// and status, paginated by per_page (20 by default) and page with
// X-Next-Page.
func glabPipelines(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return fail
	}
	query, perPage, page, err := glabPage(m[2], "ref", "status", "scope")
	if err != nil {
		return unhandled("%v", err)
	}
	status := query.Get("status")
	if query.Has("status") && !gitlabPipelineStatuses[status] {
		return unhandled("status=%q is not a GitLab pipeline status", status)
	}
	// scope=tags and scope=branches narrow by what the ref is, as GitLab
	// does; the list itself never says.
	scope := query.Get("scope")
	if query.Has("scope") && scope != "tags" && scope != "branches" {
		return unhandled("scope=%q is not implemented", scope)
	}
	rows := []map[string]any{}
	for _, pipeline := range r.pipelines() {
		if (query.Has("ref") && pipeline.Ref != query.Get("ref")) || (status != "" && pipeline.Status != status) {
			continue
		}
		if (scope == "tags" && pipeline.RefKind != "tag") || (scope == "branches" && pipeline.RefKind != "branch") {
			continue
		}
		rows = append(rows, gitlabPipelineJSON(r, pipeline))
	}
	items, next := paginate(rows, perPage, page)
	return glabOK(jsonResponse(items), "X-Next-Page: "+next, "X-Page: "+strconv.Itoa(page))
}

// glabPipeline answers one pipeline: the list's fields and its tag flag.
func glabPipeline(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return fail
	}
	id, _ := strconv.ParseInt(m[2], 10, 64)
	pipeline := r.pipeline(id)
	if pipeline == nil {
		return glabNotFound("Pipeline")
	}
	out := gitlabPipelineJSON(r, pipeline)
	out["tag"] = pipeline.RefKind == "tag"
	return glabOK(jsonResponse(out))
}

// gitlabPipelineJSON is a pipeline as GitLab's list answers it. Callers
// hold mu.
func gitlabPipelineJSON(r *Repo, pipeline *Pipeline) map[string]any {
	return map[string]any{
		"id":         pipeline.ID,
		"project_id": r.ID,
		"sha":        pipeline.SHA,
		"ref":        pipeline.Ref,
		"status":     pipeline.Status,
		"source":     pipeline.Source,
		"created_at": pipeline.CreatedAt,
		"updated_at": pipeline.UpdatedAt,
		"web_url":    fmt.Sprintf("https://%s/%s/-/pipelines/%d", r.Host, r.Project, pipeline.ID),
		"name":       nil,
	}
}
