package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
func (f *githubForge) githubPipeline(ctx context.Context, client *forgeapi.Client, ref PRReference, checks []CheckStatus, prev *CIPipeline, stepsFor []string) (CIPipeline, error) {
	runs, external := splitGitHubChecks(checks)
	if len(runs) > githubCIMaxRuns {
		runs = runs[:githubCIMaxRuns]
	}
	if err := f.fillGitHubSteps(ctx, client, ref, runs, prev, stepsFor); err != nil {
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
func (f *githubForge) fillGitHubSteps(ctx context.Context, client *forgeapi.Client, ref PRReference, runs []githubCIRun, prev *CIPipeline, stepsFor []string) error {
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
		steps, err := githubRunSteps(ctx, client, ref, run.id, pending)
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
func githubRunSteps(ctx context.Context, client *forgeapi.Client, ref PRReference, runID string, wanted map[string]bool) (map[string][]CIStep, error) {
	steps := make(map[string][]CIStep, len(wanted))
	request := forgeapi.Request{
		Path:  githubRepoPath(ref) + "/actions/runs/" + runID + "/jobs",
		Query: url.Values{"per_page": {strconv.Itoa(githubCIJobsPerPage)}},
	}
	seen := 0
	err := client.Pages(ctx, request, func(resp *forgeapi.Response) (bool, error) {
		var raw struct {
			TotalCount int `json:"total_count"`
			Jobs       []struct {
				ID    int64 `json:"id"`
				Steps []struct {
					Number      int    `json:"number"`
					Name        string `json:"name"`
					Status      string `json:"status"`
					Conclusion  string `json:"conclusion"`
					StartedAt   string `json:"started_at"`
					CompletedAt string `json:"completed_at"`
				} `json:"steps"`
			} `json:"jobs"`
		}
		if err := json.Unmarshal(resp.Body, &raw); err != nil {
			return false, fmt.Errorf("GitHub run %s jobs: decode response: %w", runID, err)
		}
		for _, job := range raw.Jobs {
			jobID := strconv.FormatInt(job.ID, 10)
			if !wanted[jobID] {
				continue
			}
			jobSteps := make([]CIStep, 0, len(job.Steps))
			for _, step := range job.Steps {
				jobSteps = append(jobSteps, CIStep{
					Number:      step.Number,
					Name:        step.Name,
					Status:      NormalizeCIStatus(step.Status, step.Conclusion),
					StartedAt:   step.StartedAt,
					CompletedAt: step.CompletedAt,
				})
			}
			steps[jobID] = jobSteps
		}
		seen += len(raw.Jobs)
		return len(steps) < len(wanted) && len(raw.Jobs) == githubCIJobsPerPage && seen < raw.TotalCount, nil
	})
	if err != nil {
		return nil, err
	}
	return steps, nil
}

// GetCIJobLog reads a job's log, keeping its last maxCILogBytes: a longer
// log is read from its tail and starts at the first whole line. A 404 (the
// job's log blob does not exist yet, see CILogStreams) is
// ErrCIJobLogNotFound. The logs endpoint redirects to a blob; the request's
// If-None-Match goes with the hop, and the ETag is the blob's.
func (f *githubForge) GetCIJobLog(ctx context.Context, ref PRReference, req CIJobLogRequest) (CIJobLog, error) {
	if err := ValidateCIJobID(req.JobID); err != nil {
		return CIJobLog{}, err
	}
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return CIJobLog{}, err
	}
	if _, _, err := splitGitHubProject(ref.Project()); err != nil {
		return CIJobLog{}, err
	}
	tail := forgeapi.NewTailBuffer(maxCILogBytes)
	resp, err := client.Stream(ctx, ciLogRequest(githubRepoPath(ref)+"/actions/jobs/"+req.JobID+"/logs", req.ETag), tail, ciLogReadLimit)
	if err != nil {
		if errors.Is(err, forgeapi.ErrNotFound) {
			return CIJobLog{}, fmt.Errorf("%w: %w", ErrCIJobLogNotFound, err)
		}
		return CIJobLog{}, err
	}
	if resp.NotModified {
		return CIJobLog{ETag: req.ETag, NotModified: true}, nil
	}
	etag := resp.Header.Get("ETag")
	text, cut := ciLogText(resp, tail)
	if cut {
		return CIJobLog{Text: string(text), ETag: etag}, nil
	}
	// The log endpoint prepends a UTF-8 BOM.
	return CIJobLog{Text: strings.TrimPrefix(string(text), "\ufeff"), ETag: etag}, nil
}

// CILogStreams is false: the Actions log endpoint answers 404 until the
// job's log blob exists, which every running job measured on 2026-10-11
// reached only once it completed, while the jobs API could still report
// it running (docs/references/forge-api-measurements.md). A followed
// running job's log is still asked for, at the pipeline's live cadence,
// so that window shows the log as soon as it is served.
func (f *githubForge) CILogStreams() bool { return false }

// githubRepoPath is the REST path of ref's repository, each segment
// escaped.
func githubRepoPath(ref PRReference) string {
	owner, repo, _ := strings.Cut(ref.Project(), "/")
	return "repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}
