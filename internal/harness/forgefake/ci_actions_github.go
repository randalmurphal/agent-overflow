package forgefake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// GitHub Actions' CI actions over REST: re-run a job, re-run a run's
// failed jobs, cancel a run, list a run's artifacts and download one. A
// re-run is a new attempt of the run: each re-run job is replaced by a
// pending one under a new id (retryJob). Refusals carry GitHub's status
// (403, 409, 410); their messages are the fake's.

// ghStatus is a GitHub REST error with its message.
func ghStatus(status int, message string) response {
	out, _ := json.Marshal(map[string]any{"message": message, "documentation_url": "https://docs.github.com/rest/actions", "status": strconv.Itoa(status)})
	return response{stdout: out, stderr: fmt.Sprintf("%d %s", status, message), status: status, header: http.Header{"Content-Type": {"application/json; charset=utf-8"}}}
}

// ghAccepted is an action's empty answer with its status.
func ghAccepted(status int) response {
	resp := jsonResponse(map[string]any{})
	resp.status = status
	return resp
}

// ghCIWriter refuses a viewer below write, as GitHub refuses one that
// cannot run workflows.
func (e *Engine) ghCIWriter(r *Repo) (response, bool) {
	if githubRoles[r.Merge.Permission] < githubRoles["WRITE"] {
		return ghStatus(http.StatusForbidden, "Must have admin rights to Repository."), false
	}
	return response{}, true
}

// ghJobRerun re-runs one job of a completed run (201); a job of a run
// still in progress, or one that has not finished, is GitHub's 403.
func ghJobRerun(e *Engine, c *call, m []string) response {
	if err := noRESTBody(c); err != nil {
		return unhandled("%v", err)
	}
	r := e.repoOn("github", c.http.host, m[1])
	if r == nil {
		return ghHTTPNotFound()
	}
	id, _ := strconv.ParseInt(m[2], 10, 64)
	pipeline, job := r.jobIn(id)
	if job == nil || job.Retried {
		return ghHTTPNotFound()
	}
	if fail, ok := e.ghCIWriter(r); !ok {
		return fail
	}
	if pipeline.live() || jobLive(job.Status) {
		return ghStatus(http.StatusForbidden, "This job cannot be rerun while its workflow run is in progress.")
	}
	if fail, ok := ghRerunWindowOpen(pipeline); !ok {
		return fail
	}
	e.retryJob(pipeline, job)
	pipeline.refreshStatus()
	return ghAccepted(http.StatusCreated)
}

// ghRunRerunFailed re-runs every failed or canceled job of a completed run
// (201); a run in progress, or one without such a job, is GitHub's 403.
func ghRunRerunFailed(e *Engine, c *call, m []string) response {
	if err := noRESTBody(c); err != nil {
		return unhandled("%v", err)
	}
	r, pipeline, fail, ok := e.ghRunFrom(c, m)
	if !ok {
		return fail
	}
	if fail, ok := e.ghCIWriter(r); !ok {
		return fail
	}
	bad := func(job *Job) bool { return job.Status == "failed" || job.Status == "canceled" }
	switch {
	case pipeline.live():
		return ghStatus(http.StatusForbidden, "This workflow run cannot be rerun while it is in progress.")
	case !slices.ContainsFunc(pipeline.current(), bad):
		return ghStatus(http.StatusForbidden, "This workflow run has no failed jobs to rerun.")
	}
	if fail, ok := ghRerunWindowOpen(pipeline); !ok {
		return fail
	}
	e.retryJobs(pipeline, bad)
	pipeline.refreshStatus()
	return ghAccepted(http.StatusCreated)
}

// ghRunCancel cancels a run in progress: its live jobs end canceled
// (202). A completed run is GitHub's 409.
func ghRunCancel(e *Engine, c *call, m []string) response {
	if err := noRESTBody(c); err != nil {
		return unhandled("%v", err)
	}
	r, pipeline, fail, ok := e.ghRunFrom(c, m)
	if !ok {
		return fail
	}
	if fail, ok := e.ghCIWriter(r); !ok {
		return fail
	}
	if !pipeline.live() {
		return ghStatus(http.StatusConflict, "Cannot cancel a workflow run that is completed.")
	}
	for _, job := range pipeline.current() {
		if jobLive(job.Status) {
			cancelJob(job)
		}
	}
	pipeline.refreshStatus()
	if pipeline.Status != "failed" {
		pipeline.Status = "canceled"
	}
	return ghAccepted(http.StatusAccepted)
}

// ghRerunWindow is how long after its creation GitHub re-runs a run.
const ghRerunWindow = 30 * 24 * time.Hour

// ghRerunWindowOpen refuses a re-run of a run created more than
// ghRerunWindow ago with GitHub's 403.
func ghRerunWindowOpen(pipeline *Pipeline) (response, bool) {
	created, err := time.Parse(time.RFC3339, pipeline.CreatedAt)
	if err == nil && time.Since(created) > ghRerunWindow {
		return ghStatus(http.StatusForbidden, "Unable to re-run this workflow run because it was created over a month ago"), false
	}
	return response{}, true
}

// expiresAt is when GitHub stops keeping the artifact: its ExpiresAt, or
// the run's creation for an expired one and 90 days after it (GitHub's
// default retention) otherwise.
func (a *Artifact) expiresAt(pipeline *Pipeline) string {
	if a.ExpiresAt != "" {
		return a.ExpiresAt
	}
	created, err := time.Parse(time.RFC3339, pipeline.CreatedAt)
	if err != nil || a.Expired {
		return pipeline.CreatedAt
	}
	return created.Add(90 * 24 * time.Hour).UTC().Format(time.RFC3339)
}

func (e *Engine) ghRunFrom(c *call, m []string) (*Repo, *Pipeline, response, bool) {
	r := e.repoOn("github", c.http.host, m[1])
	if r == nil {
		return nil, nil, ghHTTPNotFound(), false
	}
	id, _ := strconv.ParseInt(m[2], 10, 64)
	pipeline := r.pipeline(id)
	if pipeline == nil {
		return nil, nil, ghHTTPNotFound(), false
	}
	return r, pipeline, response{}, true
}

// ghRunArtifacts lists a run's artifacts, expired ones included as GitHub
// lists them, paginated by per_page (30 by default, at most 100) and page
// with a Link rel="next".
func ghRunArtifacts(e *Engine, c *call, m []string) response {
	query, perPage, page, err := glabPage(m[3])
	if err != nil {
		return unhandled("%v", err)
	}
	if query.Get("per_page") == "" {
		perPage = 30
	}
	r, pipeline, fail, ok := e.ghRunFrom(c, m)
	if !ok {
		return fail
	}
	items := make([]map[string]any, 0, len(pipeline.Artifacts))
	for _, artifact := range pipeline.Artifacts {
		items = append(items, map[string]any{
			"id":                   artifact.ID,
			"node_id":              fmt.Sprintf("MDg6QXJ0aWZhY3Q%d", artifact.ID),
			"name":                 artifact.Name,
			"size_in_bytes":        len(artifact.content),
			"url":                  fmt.Sprintf("https://api.%s/repos/%s/actions/artifacts/%d", r.Host, r.Project, artifact.ID),
			"archive_download_url": fmt.Sprintf("https://api.%s/repos/%s/actions/artifacts/%d/zip", r.Host, r.Project, artifact.ID),
			"expired":              artifact.Expired,
			"expires_at":           artifact.expiresAt(pipeline),
			"created_at":           pipeline.CreatedAt,
			"updated_at":           pipeline.UpdatedAt,
			"workflow_run":         map[string]any{"id": pipeline.ID, "head_branch": pipeline.Ref, "head_sha": pipeline.SHA},
		})
	}
	page_, next := paginate(items, perPage, page)
	resp := jsonResponse(map[string]any{"total_count": len(items), "artifacts": page_})
	if next != "" {
		query.Set("per_page", strconv.Itoa(perPage))
		query.Set("page", next)
		link := fmt.Sprintf("%s/github/rest/repos/%s/actions/runs/%d/artifacts?%s", c.http.base, r.Project, pipeline.ID, query.Encode())
		resp.header.Set("Link", "<"+link+`>; rel="next"`)
	}
	return resp
}

// ghArtifactZip serves an artifact's zip, answered directly where the
// real endpoint redirects to its blob; an expired one is GitHub's 410.
func ghArtifactZip(e *Engine, c *call, m []string) response {
	r := e.repoOn("github", c.http.host, m[1])
	if r == nil {
		return ghHTTPNotFound()
	}
	id, _ := strconv.ParseInt(m[2], 10, 64)
	for _, pipeline := range r.pipelines() {
		for _, artifact := range pipeline.Artifacts {
			if artifact.ID != id {
				continue
			}
			if artifact.Expired {
				return ghStatus(http.StatusGone, "Artifact has expired")
			}
			return response{body: bytesBody(slices.Clone(artifact.content)), header: http.Header{"Content-Type": {"application/zip"}}}
		}
	}
	return ghHTTPNotFound()
}
