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
// the workflow name. The PR's statusCheckRollup names the current check
// runs and their run/job ids (via detailsUrl); `gh run view --json jobs`
// then supplies per-job steps. External checks (StatusContext, or check
// runs from non-Actions apps) have no log API and group under
// "External" as link-only entries. Verified shapes 2026-07.

// githubCIMaxRuns bounds the per-run `gh run view` fan-out. A PR
// referencing more workflows than this gets the first N in rollup
// order; the rest still appear as link-only external entries.
const githubCIMaxRuns = 20

const githubCIExternalStage = "External"

var githubActionsJobURLPattern = regexp.MustCompile(`/actions/runs/(\d+)/job/(\d+)`)

func (f *githubForge) ListPRCIJobs(cwd, project string, number int) (CIPipeline, error) {
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

	runIDs, external := splitGitHubChecks(summary.Checks)
	stages := make([]CIStage, 0, len(runIDs)+1)
	indexByWorkflow := make(map[string]int)
	for i, runID := range runIDs {
		if i >= githubCIMaxRuns {
			break
		}
		run, err := f.githubRunJobs(cwd, project, runID)
		if err != nil {
			return CIPipeline{}, err
		}
		index, ok := indexByWorkflow[run.name]
		if !ok {
			index = len(stages)
			indexByWorkflow[run.name] = index
			stages = append(stages, CIStage{Name: run.name})
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

// splitGitHubChecks separates Actions-backed check runs (returning
// their distinct workflow-run ids in rollup order) from external
// checks that can only link out.
func splitGitHubChecks(checks []CheckStatus) (runIDs []string, external []CIJob) {
	seenRuns := make(map[string]bool)
	for _, check := range checks {
		if check.Kind == "CheckRun" {
			if match := githubActionsJobURLPattern.FindStringSubmatch(check.DetailsURL); match != nil {
				if !seenRuns[match[1]] {
					seenRuns[match[1]] = true
					runIDs = append(runIDs, match[1])
				}
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
	return runIDs, external
}

func checkDisplayName(check CheckStatus) string {
	if check.Workflow != "" {
		return check.Workflow + " / " + check.Name
	}
	return check.Name
}

type githubRunJobsResult struct {
	name string
	jobs []CIJob
}

func (f *githubForge) githubRunJobs(cwd, project, runID string) (githubRunJobsResult, error) {
	result, err := f.core.runBinary(
		"gh", cwd,
		"run", "view", runID,
		"--repo", project,
		"--json", "jobs,workflowName",
	)
	if err != nil {
		return githubRunJobsResult{}, normalizeGitHubCLIError(err)
	}
	if result.exitCode != 0 {
		return githubRunJobsResult{}, githubCommandFailure("gh run view failed", result)
	}
	var raw struct {
		WorkflowName string `json:"workflowName"`
		Jobs         []struct {
			DatabaseID  int64  `json:"databaseId"`
			Name        string `json:"name"`
			Status      string `json:"status"`
			Conclusion  string `json:"conclusion"`
			StartedAt   string `json:"startedAt"`
			CompletedAt string `json:"completedAt"`
			URL         string `json:"url"`
			Steps       []struct {
				Number     int    `json:"number"`
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &raw); err != nil {
		return githubRunJobsResult{}, fmt.Errorf("gh run view returned malformed JSON: %w", err)
	}

	name := raw.WorkflowName
	if name == "" {
		name = "Workflow " + runID
	}
	jobs := make([]CIJob, 0, len(raw.Jobs))
	for _, job := range raw.Jobs {
		status := NormalizeCIStatus(job.Status, job.Conclusion)
		steps := make([]CIStep, 0, len(job.Steps))
		for _, step := range job.Steps {
			steps = append(steps, CIStep{
				Number: step.Number,
				Name:   step.Name,
				Status: NormalizeCIStatus(step.Status, step.Conclusion),
			})
		}
		jobs = append(jobs, CIJob{
			ID:              strconv.FormatInt(job.DatabaseID, 10),
			Name:            job.Name,
			Status:          status,
			DurationSeconds: ciDurationSeconds(zeroTimeToEmpty(job.StartedAt), zeroTimeToEmpty(job.CompletedAt)),
			URL:             job.URL,
			// Logs exist once a job has started; queued jobs 404.
			LogsAvailable: status != CIStatusPending && status != CIStatusSkipped,
			Steps:         steps,
		})
	}
	return githubRunJobsResult{name: name, jobs: jobs}, nil
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
		return "", githubCommandFailure("gh api job logs failed", result)
	}
	// The log endpoint prepends a UTF-8 BOM.
	return strings.TrimPrefix(result.stdout, "\ufeff"), nil
}
