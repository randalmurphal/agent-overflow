package git

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// GitHub CI: Actions has no stage concept, so the "stage" grouping is
// the workflow name. The head commit's statusCheckRollup carries every
// check run with its status, times, workflow and details URL (which names
// the run and job ids); every Actions job has exactly one check run, so
// the rollup alone lists the jobs. Steps are the one thing it lacks: they come from the
// REST jobs list of a run, read only for the runs that hold a job the
// caller shows the steps of. External checks (StatusContext, or check
// runs from non-Actions apps) have no log API and group under "External"
// as link-only entries. Verified shapes 2026-10.

// githubCIMaxRuns bounds the workflow runs listed. A PR referencing more
// runs than this lists the first N in rollup order.
const githubCIMaxRuns = 20

// githubCIJobsPerPage is the REST jobs list page size, the API's maximum.
const githubCIJobsPerPage = 100

const githubCIExternalStage = "External"

var githubActionsJobURLPattern = regexp.MustCompile(`/actions/runs/(\d+)/job/(\d+)`)

// githubCIRun is one workflow run's jobs as the rollup lists them.
type githubCIRun struct {
	id       string
	workflow string
	jobs     []CIJob
}

// githubPipeline builds the pipeline from the rollup's checks, read by
// the same PRTick request as the rest of the tick. Steps cost one REST
// request (more only for a run past 100 jobs) per run holding a stepsFor
// job whose steps can still change: a job prev observed terminal with
// settled steps keeps them. Jobs outside stepsFor carry no steps.
func (f *githubForge) githubPipeline(ctx context.Context, client *forgeapi.Client, repo RepoReference, checks []CheckStatus, prev *CIPipeline, stepsFor []string) (CIPipeline, error) {
	runs, external := splitGitHubChecks(checks)
	if len(runs) > githubCIMaxRuns {
		runs = runs[:githubCIMaxRuns]
	}
	if err := f.fillGitHubSteps(ctx, client, repo, runs, prev, stepsFor); err != nil {
		return CIPipeline{}, err
	}
	stages := make([]CIStage, 0, len(runs)+1)
	indexByWorkflow := make(map[string]int)
	for _, run := range runs {
		name := run.workflow
		if name == "" {
			name = "Workflow " + run.id
		}
		index, ok := indexByWorkflow[name]
		if !ok {
			index = len(stages)
			indexByWorkflow[name] = index
			stages = append(stages, CIStage{Name: name})
		}
		stages[index].Jobs = append(stages[index].Jobs, run.jobs...)
	}
	if len(external) > 0 {
		stages = append(stages, CIStage{Name: githubCIExternalStage, Jobs: external})
	}

	stageStatuses := make([]string, len(stages))
	for i := range stages {
		statuses := make([]string, len(stages[i].Jobs))
		for j, job := range stages[i].Jobs {
			statuses[j] = job.Status
		}
		stages[i].Status = AggregateCIStatus(statuses)
		stageStatuses[i] = stages[i].Status
	}
	if len(stages) == 0 {
		return CIPipeline{}, nil
	}
	return CIPipeline{
		Status: AggregateCIStatus(stageStatuses),
		Stages: stages,
	}, nil
}

// splitGitHubChecks separates Actions-backed check runs, grouped into
// their workflow runs in rollup order, from external checks that can only
// link out.
func splitGitHubChecks(checks []CheckStatus) (runs []githubCIRun, external []CIJob) {
	indexByRun := make(map[string]int)
	for _, check := range checks {
		if check.Kind == "CheckRun" {
			if match := githubActionsJobURLPattern.FindStringSubmatch(check.DetailsURL); match != nil {
				index, ok := indexByRun[match[1]]
				if !ok {
					index = len(runs)
					indexByRun[match[1]] = index
					runs = append(runs, githubCIRun{id: match[1], workflow: check.Workflow})
				}
				status := NormalizeCIStatus(check.Status, check.Conclusion)
				runs[index].jobs = append(runs[index].jobs, CIJob{
					ID:              match[2],
					Name:            check.Name,
					Status:          status,
					DurationSeconds: ciDurationSeconds(check.StartedAt, check.CompletedAt),
					URL:             check.DetailsURL,
					// Logs exist once a job has started: queued jobs 404, and so
					// does a job cancelled before it started, for good.
					LogsAvailable: status != CIStatusPending && status != CIStatusSkipped && check.StartedAt != "",
				})
				continue
			}
		}
		external = append(external, CIJob{
			Name:            checkDisplayName(check),
			Status:          NormalizeCIStatus(check.Status, check.Conclusion),
			DurationSeconds: ciDurationSeconds(check.StartedAt, check.CompletedAt),
			URL:             check.DetailsURL,
			LogsAvailable:   false,
		})
	}
	return runs, external
}

func checkDisplayName(check CheckStatus) string {
	if check.Workflow != "" {
		return check.Workflow + " / " + check.Name
	}
	return check.Name
}

// fillGitHubSteps sets the steps of every stepsFor job. A job that prev
// already observed terminal, with steps that were settled then, keeps
// prev's: they cannot change again. Any other stepsFor job has its run's
// jobs read once.
func (f *githubForge) fillGitHubSteps(ctx context.Context, client *forgeapi.Client, repo RepoReference, runs []githubCIRun, prev *CIPipeline, stepsFor []string) error {
	if len(stepsFor) == 0 {
		return nil
	}
	want := make(map[string]bool, len(stepsFor))
	for _, jobID := range stepsFor {
		want[jobID] = true
	}
	for i := range runs {
		run := &runs[i]
		pending := make(map[string]bool)
		for j := range run.jobs {
			job := &run.jobs[j]
			if !want[job.ID] {
				continue
			}
			if steps, ok := settledGitHubSteps(prev, *job); ok {
				job.Steps = steps
				continue
			}
			pending[job.ID] = true
		}
		if len(pending) == 0 {
			continue
		}
		steps, err := githubRunSteps(ctx, client, repo, run.id, pending)
		if err != nil {
			return err
		}
		for j := range run.jobs {
			if jobSteps, ok := steps[run.jobs[j].ID]; ok {
				run.jobs[j].Steps = jobSteps
			}
		}
	}
	return nil
}

// settledGitHubSteps returns prev's steps for job when they can no longer
// change: the job is terminal now and was terminal in prev, and none of
// prev's steps was live (the jobs list can trail the rollup by a poll). A
// re-run puts the check back in progress, which fails the first test.
func settledGitHubSteps(prev *CIPipeline, job CIJob) ([]CIStep, bool) {
	if prev == nil || CIJobLive(job.Status) {
		return nil, false
	}
	seen := FindCIJob(*prev, job.ID)
	if seen == nil || seen.Steps == nil || CIJobLive(seen.Status) {
		return nil, false
	}
	for _, step := range seen.Steps {
		if CIJobLive(step.Status) {
			return nil, false
		}
	}
	return seen.Steps, true
}

// githubRunSteps reads a run's jobs from the REST API (one request per 100
// jobs, following the next page only until every wanted job was seen)
// and returns their steps by job id. A job the answer lacks is absent
// from the map.
func githubRunSteps(ctx context.Context, client *forgeapi.Client, repo RepoReference, runID string, wanted map[string]bool) (map[string][]CIStep, error) {
	steps := make(map[string][]CIStep, len(wanted))
	err := readGitHubRunJobs(ctx, client, repo, runID, func(jobs []githubRunJobRaw, seen, total int) bool {
		for _, job := range jobs {
			if jobID := strconv.FormatInt(job.ID, 10); wanted[jobID] {
				steps[jobID] = job.steps()
			}
		}
		return len(steps) < len(wanted) && len(jobs) == githubCIJobsPerPage && seen < total
	})
	if err != nil {
		return nil, err
	}
	return steps, nil
}

// githubRunJobRaw is one job of the REST jobs list of a run. An absent
// time is null, which decodes as "".
type githubRunJobRaw struct {
	ID          int64              `json:"id"`
	Name        string             `json:"name"`
	Status      string             `json:"status"`
	Conclusion  string             `json:"conclusion"`
	HTMLURL     string             `json:"html_url"`
	StartedAt   string             `json:"started_at"`
	CompletedAt string             `json:"completed_at"`
	Steps       []githubRunStepRaw `json:"steps"`
}

type githubRunStepRaw struct {
	Number      int    `json:"number"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
}

func (j githubRunJobRaw) steps() []CIStep {
	steps := make([]CIStep, 0, len(j.Steps))
	for _, step := range j.Steps {
		steps = append(steps, CIStep{
			Number:      step.Number,
			Name:        step.Name,
			Status:      NormalizeCIStatus(step.Status, step.Conclusion),
			StartedAt:   step.StartedAt,
			CompletedAt: step.CompletedAt,
		})
	}
	return steps
}

// readGitHubRunJobs reads a run's jobs list, 100 per request, handing each
// page to visit with the jobs seen so far and the run's total; visit
// answers whether to read the next page.
func readGitHubRunJobs(ctx context.Context, client *forgeapi.Client, repo RepoReference, runID string, visit func(jobs []githubRunJobRaw, seen, total int) bool) error {
	request := forgeapi.Request{
		Path:  githubRepoPath(repo) + "/actions/runs/" + runID + "/jobs",
		Query: url.Values{"per_page": {strconv.Itoa(githubCIJobsPerPage)}},
	}
	seen := 0
	return client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		var raw struct {
			TotalCount int               `json:"total_count"`
			Jobs       []githubRunJobRaw `json:"jobs"`
		}
		if err := json.Unmarshal(resp.Body, &raw); err != nil {
			return false, fmt.Errorf("GitHub run %s jobs: decode response: %w", runID, err)
		}
		seen += len(raw.Jobs)
		return visit(raw.Jobs, seen, raw.TotalCount), nil
	})
}

// githubRunRaw is one workflow run of the REST API.
type githubRunRaw struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	DisplayTitle string `json:"display_title"`
	HeadBranch   string `json:"head_branch"`
	HeadSHA      string `json:"head_sha"`
	Event        string `json:"event"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	HTMLURL      string `json:"html_url"`
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
}

// ListPipelines lists the repository's workflow runs, the most recent
// first, in one request: each run is one row.
func (f *githubForge) ListPipelines(ctx context.Context, repo RepoReference, filter PipelineListFilter) ([]PipelineSummary, error) {
	if _, _, err := splitGitHubProject(repo.Project()); err != nil {
		return nil, err
	}
	status, err := pipelineStatusFilter("github", filter.Status)
	if err != nil {
		return nil, err
	}
	client, err := f.core.githubAPI(repo.Host)
	if err != nil {
		return nil, err
	}
	query := url.Values{"per_page": {strconv.Itoa(filter.Limit)}}
	if filter.Ref != "" {
		query.Set("branch", filter.Ref)
	}
	if status != "" {
		query.Set("status", status)
	}
	var raw struct {
		WorkflowRuns []githubRunRaw `json:"workflow_runs"`
	}
	if _, err := client.JSON(ctx, forgeapi.Request{Path: githubRepoPath(repo) + "/actions/runs", Query: query}, &raw); err != nil {
		return nil, err
	}
	rows := make([]PipelineSummary, 0, len(raw.WorkflowRuns))
	for _, run := range raw.WorkflowRuns {
		rows = append(rows, run.summary())
	}
	return rows, nil
}

func (run githubRunRaw) summary() PipelineSummary {
	row := PipelineSummary{
		ID:        strconv.FormatInt(run.ID, 10),
		Status:    NormalizeCIStatus(run.Status, run.Conclusion),
		Name:      run.Name,
		Ref:       run.HeadBranch,
		RefKind:   PipelineRefBranch,
		SHA:       run.HeadSHA,
		Title:     run.DisplayTitle,
		Source:    run.Event,
		CreatedAt: run.CreatedAt,
		UpdatedAt: run.UpdatedAt,
		URL:       run.HTMLURL,
	}
	switch {
	case strings.HasPrefix(run.Event, "pull_request"):
		row.RefKind = PipelineRefMergeRequest
		if len(run.PullRequests) > 0 {
			row.MRNumber = run.PullRequests[0].Number
		}
	case run.Event == "release":
		row.RefKind = PipelineRefTag
	}
	return row
}

// ReadPipeline reads one workflow run and its jobs (one request per 100
// jobs, at most githubReadMaxPages) into one stage named after the
// workflow. The jobs list carries every job's steps, so stepsFor costs
// nothing more and prev is not needed.
func (f *githubForge) ReadPipeline(ctx context.Context, repo RepoReference, id string, _ *CIPipeline, stepsFor []string) (CIPipeline, error) {
	if _, _, err := splitGitHubProject(repo.Project()); err != nil {
		return CIPipeline{}, err
	}
	client, err := f.core.githubAPI(repo.Host)
	if err != nil {
		return CIPipeline{}, err
	}
	var run githubRunRaw
	if _, err := client.JSON(ctx, forgeapi.Request{Path: githubRepoPath(repo) + "/actions/runs/" + id}, &run); err != nil {
		return CIPipeline{}, err
	}
	want := make(map[string]bool, len(stepsFor))
	for _, jobID := range stepsFor {
		want[jobID] = true
	}
	var jobs []CIJob
	pages := 0
	err = readGitHubRunJobs(ctx, client, repo, id, func(page []githubRunJobRaw, seen, total int) bool {
		for _, raw := range page {
			job := raw.job()
			if want[job.ID] {
				job.Steps = raw.steps()
			}
			jobs = append(jobs, job)
		}
		pages++
		return len(page) == githubCIJobsPerPage && seen < total && pages < githubReadMaxPages
	})
	if err != nil {
		return CIPipeline{}, err
	}
	pipeline := CIPipeline{
		ID:     id,
		Status: NormalizeCIStatus(run.Status, run.Conclusion),
		URL:    run.HTMLURL,
		Ref:    run.HeadBranch,
		SHA:    run.HeadSHA,
	}
	if len(jobs) > 0 {
		name := run.Name
		if name == "" {
			name = "Workflow " + id
		}
		statuses := make([]string, len(jobs))
		for i, job := range jobs {
			statuses[i] = job.Status
		}
		pipeline.Stages = []CIStage{{Name: name, Status: AggregateCIStatus(statuses), Jobs: jobs}}
	}
	return pipeline, nil
}

// job is the run job as the pipeline shows it. Its log exists once it
// started, as for a rollup check run.
func (j githubRunJobRaw) job() CIJob {
	status := NormalizeCIStatus(j.Status, j.Conclusion)
	return CIJob{
		ID:              strconv.FormatInt(j.ID, 10),
		Name:            j.Name,
		Status:          status,
		DurationSeconds: ciDurationSeconds(j.StartedAt, j.CompletedAt),
		URL:             j.HTMLURL,
		LogsAvailable:   status != CIStatusPending && status != CIStatusSkipped && j.StartedAt != "",
	}
}

// StreamCIJobLog reads a job's log into the writer begin returns (see
// Core.StreamCIJobLog). The logs endpoint redirects to a blob; the
// request's Range and If-None-Match go with the hop, and the ETag and
// Content-Range are the blob's. A 404 (the job's log blob does not exist
// yet, see CILogStreams) is ErrCIJobLogNotFound. The body starts with the
// UTF-8 byte order mark the endpoint prepends.
func (f *githubForge) StreamCIJobLog(ctx context.Context, repo RepoReference, req CIJobLogRequest, begin func(CIJobLogStart) (io.Writer, error)) (bool, error) {
	if err := ValidateCIJobID(req.JobID); err != nil {
		return false, err
	}
	client, err := f.core.githubAPI(repo.Host)
	if err != nil {
		return false, err
	}
	if _, _, err := splitGitHubProject(repo.Project()); err != nil {
		return false, err
	}
	return streamCILog(ctx, client, ciLogRequest(githubRepoPath(repo)+"/actions/jobs/"+req.JobID+"/logs", req), begin)
}

// CILogStreams is false: the Actions log endpoint answers 404 until the
// job's log blob exists, which every running job measured on 2026-10-11
// reached only once it completed, while the jobs API could still report
// it running (docs/references/forge-api-measurements.md). A followed
// running job's log is still asked for, at the pipeline's live cadence,
// so that window shows the log as soon as it is served.
func (f *githubForge) CILogStreams() bool { return false }

// githubRepoPath is the REST path of a repository, each segment
// escaped.
func githubRepoPath(ref RepoReference) string {
	owner, repo, _ := strings.Cut(ref.Project(), "/")
	return "repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}
