package git

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"

	"agent-overflow/internal/forgeapi"
)

// GitLab CI: the MR's head pipeline is one pipeline of staged jobs.
// The pipeline comes from the MR read, its jobs from
// /pipelines/:id/jobs; per-job traces from /jobs/:id/trace. Verified shapes (2026-07): jobs carry id, name,
// stage, status, duration, web_url, allow_failure, started_at; the
// jobs list is ordered newest-first, so stage order is recovered by
// first-seen over ascending job id.

// gitlabCIJobsMaxPages bounds the jobs pages read of one pipeline. A
// pipeline past it shows its newest gitlabCIJobsMaxPages*gitlabPageSize
// jobs.
const gitlabCIJobsMaxPages = 5

type gitlabCIJobRaw struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Stage        string   `json:"stage"`
	Status       string   `json:"status"`
	Duration     *float64 `json:"duration"`
	WebURL       string   `json:"web_url"`
	AllowFailure bool     `json:"allow_failure"`
	StartedAt    *string  `json:"started_at"`
}

// readGitLabPipeline reads the jobs of the merge request's head pipeline,
// which the MR read already named. A merge request with no pipeline costs
// no request.
func readGitLabPipeline(ctx context.Context, client *forgeapi.Client, repo RepoReference, pipeline *gitlabPipelineRaw) (CIPipeline, error) {
	if pipeline == nil || pipeline.ID <= 0 {
		return CIPipeline{}, nil
	}
	request := forgeapi.Request{
		Path:  gitlabProjectPath(repo.Project()) + "/pipelines/" + strconv.FormatInt(pipeline.ID, 10) + "/jobs",
		Query: url.Values{"per_page": {strconv.Itoa(gitlabPageSize)}},
	}
	var jobs []gitlabCIJobRaw
	page, pages := 1, 0
	err := client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		var pageJobs []gitlabCIJobRaw
		if err := json.Unmarshal(resp.Body, &pageJobs); err != nil {
			return false, fmt.Errorf("GitLab pipeline %d jobs: decode response: %w", pipeline.ID, err)
		}
		jobs = append(jobs, pageJobs...)
		pages++
		return len(pageJobs) == gitlabPageSize && pages < gitlabCIJobsMaxPages && advanceGitLabPage(resp, &page), nil
	})
	if err != nil {
		return CIPipeline{}, err
	}
	return CIPipeline{
		Status: NormalizeCIStatus(pipeline.Status, ""),
		URL:    pipeline.WebURL,
		Stages: groupGitLabJobsByStage(jobs),
	}, nil
}

func groupGitLabJobsByStage(raw []gitlabCIJobRaw) []CIStage {
	// The API returns jobs newest-first; ascending job id recovers
	// creation order, which follows stage order.
	sort.Slice(raw, func(i, j int) bool { return raw[i].ID < raw[j].ID })

	var stages []CIStage
	indexByStage := make(map[string]int)
	for _, job := range raw {
		duration := 0.0
		if job.Duration != nil {
			duration = *job.Duration
		}
		ci := CIJob{
			ID:              strconv.FormatInt(job.ID, 10),
			Name:            job.Name,
			Status:          NormalizeCIStatus(job.Status, ""),
			DurationSeconds: duration,
			URL:             job.WebURL,
			AllowFailure:    job.AllowFailure,
			// A trace exists once the job has started; created/manual/
			// skipped jobs 404 on the trace endpoint.
			LogsAvailable: job.StartedAt != nil && *job.StartedAt != "",
		}
		index, ok := indexByStage[job.Stage]
		if !ok {
			index = len(stages)
			indexByStage[job.Stage] = index
			stages = append(stages, CIStage{Name: job.Stage})
		}
		stages[index].Jobs = append(stages[index].Jobs, ci)
	}
	for i := range stages {
		statuses := make([]string, len(stages[i].Jobs))
		for j, job := range stages[i].Jobs {
			statuses[j] = job.Status
		}
		stages[i].Status = AggregateCIStatus(statuses)
	}
	return stages
}

// StreamCIJobLog reads a job's raw trace into the writer begin returns
// (see Core.StreamCIJobLog); internal/cilog cleans it. A 404 (the job has
// not started, or does not exist) is ErrCIJobLogNotFound. GitLab honors
// If-None-Match on a trace, running or completed, and Range only on a
// completed one: a running trace answers a range with 200 and the whole
// body.
func (f *gitlabForge) StreamCIJobLog(ctx context.Context, repo RepoReference, req CIJobLogRequest, begin func(CIJobLogStart) (io.Writer, error)) (bool, error) {
	if err := ValidateCIJobID(req.JobID); err != nil {
		return false, err
	}
	client, err := f.core.gitlabAPI(repo.Host)
	if err != nil {
		return false, err
	}
	request := ciLogRequest(gitlabProjectPath(repo.Project())+"/jobs/"+req.JobID+"/trace", req)
	// The trace is plain text.
	request.Header.Set("Accept", "*/*")
	return streamCILog(ctx, client, request, begin)
}

// CILogStreams is true: the trace endpoint returns what the runner
// has uploaded so far for a running job.
func (f *gitlabForge) CILogStreams() bool { return true }
