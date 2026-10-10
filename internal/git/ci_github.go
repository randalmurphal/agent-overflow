package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// GitHub CI: Actions has no stage concept, so the "stage" grouping is
// the workflow name. The PR's statusCheckRollup carries every check run
// with its status, times and details URL (which names the run and job
// ids); every Actions job has exactly one check run, so the rollup alone
// lists the jobs. Steps are the one thing it lacks: they come from the
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

// ListPRCIJobs builds the pipeline from the rollup, one GraphQL request
// per poll. Steps cost one REST request (more only for a run past 100
// jobs) per run holding a stepsFor job whose steps can still change: a job
// prev observed terminal with settled steps keeps them. Jobs outside
// stepsFor carry no steps.
func (f *githubForge) ListPRCIJobs(cwd, project string, number int, prev *CIPipeline, stepsFor []string) (CIPipeline, error) {
	if strings.TrimSpace(project) == "" {
		return CIPipeline{}, errors.New("project (owner/repo) is required")
	}
	if number <= 0 {
		return CIPipeline{}, fmt.Errorf("PR number must be positive, got %d", number)
	}
	result, err := f.core.runBinary(
		"gh", cwd,
		"pr", "view",
		"--repo", project,
		strconv.Itoa(number),
		"--json", "statusCheckRollup",
	)
	if err != nil {
		return CIPipeline{}, normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return CIPipeline{}, githubCommandFailure("gh pr view failed", result)
	}
	var raw struct {
		StatusCheckRollup []json.RawMessage `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &raw); err != nil {
		return CIPipeline{}, fmt.Errorf("gh pr view returned malformed JSON: %w", err)
	}
	summary := parseGitHubCheckSummary(raw.StatusCheckRollup)

	runs, external := splitGitHubChecks(summary.Checks)
	if len(runs) > githubCIMaxRuns {
		runs = runs[:githubCIMaxRuns]
	}
	if err := f.fillGitHubSteps(cwd, project, runs, prev, stepsFor); err != nil {
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
func (f *githubForge) fillGitHubSteps(cwd, project string, runs []githubCIRun, prev *CIPipeline, stepsFor []string) error {
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
		steps, err := f.githubRunSteps(cwd, project, run.id, pending)
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
// jobs, stopping once every wanted job was seen) and returns their steps
// by job id. A job the answer lacks is absent from the map.
func (f *githubForge) githubRunSteps(cwd, project, runID string, wanted map[string]bool) (map[string][]CIStep, error) {
	steps := make(map[string][]CIStep, len(wanted))
	seen := 0
	for page := 1; ; page++ {
		endpoint := "repos/" + project + "/actions/runs/" + runID + "/jobs?per_page=" + strconv.Itoa(githubCIJobsPerPage)
		if page > 1 {
			endpoint += "&page=" + strconv.Itoa(page)
		}
		result, err := f.core.runBinary("gh", cwd, "api", endpoint)
		if err != nil {
			return nil, normalizeGitHubCLIError(err)
		}
		if result.exitCode != 0 {
			return nil, githubCommandFailure("gh api run jobs failed", result)
		}
		var raw struct {
			TotalCount int `json:"total_count"`
			Jobs       []struct {
				ID    int64 `json:"id"`
				Steps []struct {
					Number     int    `json:"number"`
					Name       string `json:"name"`
					Status     string `json:"status"`
					Conclusion string `json:"conclusion"`
				} `json:"steps"`
			} `json:"jobs"`
		}
		if err := json.Unmarshal([]byte(result.stdout), &raw); err != nil {
			return nil, fmt.Errorf("gh api run jobs returned malformed JSON: %w", err)
		}
		for _, job := range raw.Jobs {
			jobID := strconv.FormatInt(job.ID, 10)
			if !wanted[jobID] {
				continue
			}
			jobSteps := make([]CIStep, 0, len(job.Steps))
			for _, step := range job.Steps {
				jobSteps = append(jobSteps, CIStep{
					Number: step.Number,
					Name:   step.Name,
					Status: NormalizeCIStatus(step.Status, step.Conclusion),
				})
			}
			steps[jobID] = jobSteps
		}
		seen += len(raw.Jobs)
		if len(steps) == len(wanted) || len(raw.Jobs) < githubCIJobsPerPage || seen >= raw.TotalCount {
			return steps, nil
		}
	}
}

func (f *githubForge) GetCIJobLog(cwd, project, jobID string) (string, error) {
	if strings.TrimSpace(project) == "" {
		return "", errors.New("project (owner/repo) is required")
	}
	if err := ValidateCIJobID(jobID); err != nil {
		return "", err
	}
	result, err := f.core.runBinaryWithLimit("gh", cwd, maxCILogBytes,
		"api", "repos/"+project+"/actions/jobs/"+jobID+"/logs")
	if err != nil {
		return "", normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return "", ciJobLogFailure(githubCommandFailure("gh api job logs failed", result), result)
	}
	// The log endpoint prepends a UTF-8 BOM.
	return strings.TrimPrefix(result.stdout, "\ufeff"), nil
}

// CILogWhileRunning is false: the Actions log endpoint answers 404 until
// the job completes, and for a while after; the live log is only the
// website's own stream.
func (f *githubForge) CILogWhileRunning() bool { return false }
