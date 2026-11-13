package forgefake

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
)

// GitLab CI's actions over REST: a job's retry, cancel and play, a
// pipeline's retry of its failed jobs and cancel, and a job's artifacts
// archive, with GitLab's answers: a retry is 201 with the new job, and a
// job GitLab cannot retry is 403 "Job is not retryable", one it cannot
// play 400 "Unplayable Job"; a cancel of a job that is not running is
// answered with the job unchanged, as GitLab's is. Each needs the
// developer role (GitLab's 403 otherwise).

// glabCIDeveloper refuses a viewer below developer.
func (e *Engine) glabCIDeveloper(r *Repo) (response, bool) {
	if gitlabRoles[r.Merge.Permission] < gitlabRoles["developer"] {
		return glabStatus(http.StatusForbidden, "403 Forbidden"), false
	}
	return response{}, true
}

// glabCIJob resolves a POST .../jobs/ID/<action> to the build it names.
func (e *Engine) glabCIJob(c *call, m []string) (*Repo, *Pipeline, *Job, response, bool) {
	if err := noRESTBody(c); err != nil {
		return nil, nil, nil, unhandled("%v", err), false
	}
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return nil, nil, nil, fail, false
	}
	id, _ := strconv.ParseInt(m[2], 10, 64)
	pipeline, job := r.jobIn(id)
	if job == nil || job.Kind == JobKindExternal {
		return nil, nil, nil, glabNotFound("Job"), false
	}
	if fail, ok := e.glabCIDeveloper(r); !ok {
		return nil, nil, nil, fail, false
	}
	return r, pipeline, job, response{}, true
}

// gitlabJobJSON is a job as GitLab's jobs API answers one.
func gitlabJobJSON(r *Repo, pipeline *Pipeline, job Job) map[string]any {
	return map[string]any{
		"id":            job.ID,
		"name":          job.Name,
		"stage":         job.Stage,
		"status":        job.Status,
		"allow_failure": job.AllowFailure,
		"started_at":    nullable(job.StartedAt),
		"finished_at":   nullable(job.CompletedAt),
		"ref":           pipeline.Ref,
		"pipeline":      map[string]any{"id": pipeline.ID, "project_id": r.ID, "status": pipeline.Status, "ref": pipeline.Ref, "sha": pipeline.SHA},
		"web_url":       fmt.Sprintf("https://%s/%s/-/jobs/%d", r.Host, r.Project, job.ID),
	}
}

func glabCreated(value any) response {
	resp := glabOK(jsonResponse(value))
	resp.status = http.StatusCreated
	return resp
}

// gitlabJobRetryable is a build GitLab retries: finished, and not
// replaced by a retry already.
func gitlabJobRetryable(job *Job) bool {
	return job.Kind == "" && !job.Retried && (job.Status == "success" || job.Status == "failed" || job.Status == "canceled")
}

func glabJobRetry(e *Engine, c *call, m []string) response {
	r, pipeline, job, fail, ok := e.glabCIJob(c, m)
	if !ok {
		return fail
	}
	if !gitlabJobRetryable(job) {
		return glabStatus(http.StatusForbidden, "403 Forbidden - Job is not retryable")
	}
	next := e.retryJob(pipeline, job)
	pipeline.refreshStatus()
	return glabCreated(gitlabJobJSON(r, pipeline, next))
}

func glabJobCancel(e *Engine, c *call, m []string) response {
	r, pipeline, job, fail, ok := e.glabCIJob(c, m)
	if !ok {
		return fail
	}
	if !job.Retried && jobLive(job.Status) {
		cancelJob(job)
		pipeline.refreshStatus()
	}
	return glabCreated(gitlabJobJSON(r, pipeline, *job))
}

func glabJobPlay(e *Engine, c *call, m []string) response {
	r, pipeline, job, fail, ok := e.glabCIJob(c, m)
	if !ok {
		return fail
	}
	if job.Retried || job.Status != "manual" {
		return glabStatus(http.StatusBadRequest, "400 Bad request - Unplayable Job")
	}
	job.Status = "pending"
	pipeline.refreshStatus()
	return glabOK(jsonResponse(gitlabJobJSON(r, pipeline, *job)))
}

// glabCIPipeline resolves a POST .../pipelines/ID/<action>.
func (e *Engine) glabCIPipeline(c *call, m []string) (*Repo, *Pipeline, response, bool) {
	if err := noRESTBody(c); err != nil {
		return nil, nil, unhandled("%v", err), false
	}
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return nil, nil, fail, false
	}
	id, _ := strconv.ParseInt(m[2], 10, 64)
	pipeline := r.pipeline(id)
	if pipeline == nil {
		return nil, nil, glabNotFound("Pipeline"), false
	}
	if fail, ok := e.glabCIDeveloper(r); !ok {
		return nil, nil, fail, false
	}
	return r, pipeline, response{}, true
}

// glabPipelineRetry retries the pipeline's failed and canceled builds
// (201 with the pipeline); one with none is answered unchanged.
func glabPipelineRetry(e *Engine, c *call, m []string) response {
	r, pipeline, fail, ok := e.glabCIPipeline(c, m)
	if !ok {
		return fail
	}
	e.retryJobs(pipeline, func(job *Job) bool {
		return job.Kind == "" && (job.Status == "failed" || job.Status == "canceled")
	})
	pipeline.refreshStatus()
	return glabCreated(gitlabPipelineJSON(r, pipeline))
}

// glabPipelineCancel cancels the pipeline's pending and running jobs
// (200 with the pipeline); a manual job waits on.
func glabPipelineCancel(e *Engine, c *call, m []string) response {
	r, pipeline, fail, ok := e.glabCIPipeline(c, m)
	if !ok {
		return fail
	}
	if pipeline.live() {
		for _, job := range pipeline.current() {
			if jobLive(job.Status) {
				cancelJob(job)
			}
		}
		pipeline.refreshStatus()
		if pipeline.Status != "failed" {
			pipeline.Status = "canceled"
		}
	}
	return glabOK(jsonResponse(gitlabPipelineJSON(r, pipeline)))
}

// glabJobArtifacts serves a build's artifacts archive; a job without one
// is GitLab's 404.
func glabJobArtifacts(e *Engine, c *call, m []string) response {
	r, fail, ok := e.glabProject(c, m[1])
	if !ok {
		return fail
	}
	id, _ := strconv.ParseInt(m[2], 10, 64)
	job := r.job(id)
	if job == nil || job.Artifacts == nil {
		return glabStatus(http.StatusNotFound, "404 Not Found")
	}
	return response{body: bytesBody(slices.Clone(job.Artifacts.content)), header: http.Header{
		"Content-Type":        {"application/octet-stream"},
		"Content-Disposition": {`attachment; filename="` + job.Artifacts.Name + `"`},
	}}
}

// gitlabJobActionFields are a PipelineJobs row's action fields as GitLab
// answers them: retryable, cancelable and playable from the build's
// status, the viewer's permissions from the role, and the artifacts a
// build holds (its archive, and its trace once started). A bridge or
// commit status answers nothing actionable.
func gitlabJobActionFields(r *Repo, job Job) map[string]any {
	developer := gitlabRoles[r.Merge.Permission] >= gitlabRoles["developer"]
	build := job.Kind == ""
	playable := build && job.Status == "manual"
	var artifacts any
	if build {
		nodes := []map[string]any{}
		if job.Artifacts != nil {
			nodes = append(nodes, map[string]any{"fileType": "ARCHIVE"})
		}
		if job.StartedAt != "" {
			nodes = append(nodes, map[string]any{"fileType": "TRACE"})
		}
		artifacts = map[string]any{"nodes": nodes}
	}
	return map[string]any{
		"retryable":  gitlabJobRetryable(&job),
		"cancelable": build && jobLive(job.Status),
		"playable":   playable,
		"canPlayJob": playable && developer,
		"userPermissions": map[string]any{
			"updateBuild":      developer,
			"cancelBuild":      developer,
			"readJobArtifacts": gitlabRoles[r.Merge.Permission] >= gitlabRoles["reporter"],
		},
		"artifacts": artifacts,
	}
}

// gitlabPipelineActionFields are PipelineJobs' pipeline-level action
// fields: retryable once finished with a failed or canceled build,
// cancelable while live, and the viewer's permissions from the role.
func gitlabPipelineActionFields(r *Repo, pipeline *Pipeline) map[string]any {
	developer := gitlabRoles[r.Merge.Permission] >= gitlabRoles["developer"]
	live := pipeline.live()
	retryable := !live && slices.ContainsFunc(pipeline.current(), func(job *Job) bool {
		return job.Kind == "" && (job.Status == "failed" || job.Status == "canceled")
	})
	return map[string]any{
		"retryable":       retryable,
		"cancelable":      live,
		"userPermissions": map[string]any{"updatePipeline": developer, "cancelPipeline": developer},
	}
}
