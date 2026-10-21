package git

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
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

// readGitLabPipeline reads the jobs of a pipeline already read: the merge
// request's head pipeline, which the MR read named, or ReadPipeline's. A
// merge request with no pipeline costs no request.
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
		ID:     strconv.FormatInt(pipeline.ID, 10),
		Status: NormalizeCIStatus(pipeline.Status, ""),
		URL:    pipeline.WebURL,
		Ref:    pipeline.Ref,
		SHA:    pipeline.SHA,
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

// gitlabMergeRequestRef is the ref of a merge request pipeline: the
// merge request's head, its merged result or its merge train entry.
var gitlabMergeRequestRef = regexp.MustCompile(`^refs/merge-requests/(\d+)/(?:head|merge|train)$`)

// gitlabTagScopeMaxPages bounds the tag scope pages one ListPipelines
// reads: the latest pipelines of that many hundred tags, newer than the
// oldest listed pipeline.
const gitlabTagScopeMaxPages = 10

// ListPipelines lists the project's pipelines, the most recent first: one
// list request and the tag scope reads that say which refs are tags
// (gitlabTagPipelines).
func (f *gitlabForge) ListPipelines(ctx context.Context, repo RepoReference, filter PipelineListFilter) ([]PipelineSummary, error) {
	status, err := pipelineStatusFilter("gitlab", filter.Status)
	if err != nil {
		return nil, err
	}
	client, err := f.core.gitlabAPI(repo.Host)
	if err != nil {
		return nil, err
	}
	query := url.Values{"per_page": {strconv.Itoa(filter.Limit)}}
	if filter.Ref != "" {
		query.Set("ref", filter.Ref)
	}
	if status != "" {
		query.Set("status", status)
	}
	path := gitlabProjectPath(repo.Project()) + "/pipelines"
	var raw []gitlabPipelineRaw
	if _, err := client.JSON(ctx, forgeapi.Request{Path: path, Query: query}, &raw); err != nil {
		return nil, err
	}
	tags, tagged, err := gitlabTagPipelines(ctx, client, path, filter.Ref, raw)
	if err != nil {
		return nil, err
	}
	// A tag's latest pipeline the list did not hold (past its page) is
	// appended when the status filter admits it, at most a list's worth.
	listed := map[int64]bool{}
	for _, pipeline := range raw {
		listed[pipeline.ID] = true
	}
	appended := 0
	for _, pipeline := range tagged {
		if appended == filter.Limit {
			break
		}
		if !listed[pipeline.ID] && (status == "" || pipeline.Status == status) {
			raw = append(raw, pipeline)
			appended++
		}
	}
	rows := make([]PipelineSummary, 0, len(raw))
	for _, pipeline := range raw {
		row := PipelineSummary{
			ID:        strconv.FormatInt(pipeline.ID, 10),
			Status:    NormalizeCIStatus(pipeline.Status, ""),
			Ref:       pipeline.Ref,
			RefKind:   PipelineRefBranch,
			SHA:       pipeline.SHA,
			Source:    pipeline.Source,
			CreatedAt: pipeline.CreatedAt,
			UpdatedAt: pipeline.UpdatedAt,
			URL:       pipeline.WebURL,
		}
		if m := gitlabMergeRequestRef.FindStringSubmatch(pipeline.Ref); m != nil {
			row.RefKind = PipelineRefMergeRequest
			row.MRNumber, _ = strconv.Atoi(m[1])
		} else if tags[pipeline.Ref] {
			row.RefKind = PipelineRefTag
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// gitlabTagPipelines answers which refs of the listed pipelines are tags,
// and the tag pipelines read to learn it. GitLab's list does not say
// (only the single pipeline carries `tag`), and its tag scope answers
// only the latest pipeline of each tag, so a pipeline is a tag's when its
// ref is a ref the tag scope names: a ref is a tag or a branch. The scope
// is read without the status filter, since a tag whose latest pipeline
// has another status is a tag still. It is ordered by id, newest first,
// and the latest pipeline of a listed pipeline's tag is no older than
// that pipeline, so the pages stop at the oldest listed id, a short page,
// or gitlabTagScopeMaxPages. A list of one ref reads that ref's scope
// alone (one row at most); a merge request ref, or an empty list, needs
// no read.
func gitlabTagPipelines(ctx context.Context, client *forgeapi.Client, path, ref string, listed []gitlabPipelineRaw) (map[string]bool, []gitlabPipelineRaw, error) {
	if len(listed) == 0 || gitlabMergeRequestRef.MatchString(ref) {
		return nil, nil, nil
	}
	oldest := listed[0].ID
	for _, pipeline := range listed {
		oldest = min(oldest, pipeline.ID)
	}
	query := url.Values{"per_page": {strconv.Itoa(gitlabPageSize)}, "scope": {"tags"}}
	if ref != "" {
		query.Set("ref", ref)
	}
	refs := map[string]bool{}
	var tagged []gitlabPipelineRaw
	page, pages := 1, 0
	err := client.Pages(ctx, forgeapi.Request{Path: path, Query: query}, func(resp *forgeapi.Response) (bool, error) {
		var pagePipelines []gitlabPipelineRaw
		if err := json.Unmarshal(resp.Body, &pagePipelines); err != nil {
			return false, fmt.Errorf("GitLab tag pipelines: decode response: %w", err)
		}
		for _, pipeline := range pagePipelines {
			refs[pipeline.Ref] = true
		}
		tagged = append(tagged, pagePipelines...)
		pages++
		full := len(pagePipelines) == gitlabPageSize
		return full && pagePipelines[len(pagePipelines)-1].ID >= oldest && pages < gitlabTagScopeMaxPages && advanceGitLabPage(resp, &page), nil
	})
	if err != nil {
		return nil, nil, err
	}
	return refs, tagged, nil
}

// ReadPipeline reads one pipeline and its jobs, as ReadPR reads a merge
// request's head pipeline. prev and stepsFor are unused: GitLab jobs have
// no steps.
func (f *gitlabForge) ReadPipeline(ctx context.Context, repo RepoReference, id string, _ *CIPipeline, _ []string) (CIPipeline, error) {
	client, err := f.core.gitlabAPI(repo.Host)
	if err != nil {
		return CIPipeline{}, err
	}
	var pipeline gitlabPipelineRaw
	if _, err := client.JSON(ctx, forgeapi.Request{Path: gitlabProjectPath(repo.Project()) + "/pipelines/" + id}, &pipeline); err != nil {
		return CIPipeline{}, err
	}
	if strconv.FormatInt(pipeline.ID, 10) != id {
		return CIPipeline{}, fmt.Errorf("GitLab answered pipeline %d for pipeline %s", pipeline.ID, id)
	}
	return readGitLabPipeline(ctx, client, repo, &pipeline)
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
