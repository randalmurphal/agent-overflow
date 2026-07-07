package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// GitLab CI: the MR's head pipeline is one pipeline of staged jobs.
// Jobs come from /pipelines/:id/jobs; per-job traces from
// /jobs/:id/trace. Verified shapes (2026-07): jobs carry id, name,
// stage, status, duration, web_url, allow_failure, started_at; the
// jobs list is ordered newest-first, so stage order is recovered by
// first-seen over ascending job id.

const (
	gitlabCIJobsPerPage  = 100
	gitlabCIJobsMaxPages = 5
)

func gitLabPipelineJobsEndpoint(project string, pipelineID int, page int) string {
	return "projects/" + url.PathEscape(project) + "/pipelines/" + strconv.Itoa(pipelineID) +
		"/jobs?per_page=" + strconv.Itoa(gitlabCIJobsPerPage) + "&page=" + strconv.Itoa(page)
}

func gitLabJobTraceEndpoint(project, jobID string) string {
	return "projects/" + url.PathEscape(project) + "/jobs/" + jobID + "/trace"
}

func (f *gitlabForge) ListPRCIJobs(cwd, project string, number int) (CIPipeline, error) {
	if strings.TrimSpace(project) == "" {
		return CIPipeline{}, errors.New("project (namespace/repo) is required")
	}
	if number <= 0 {
		return CIPipeline{}, fmt.Errorf("MR number must be positive, got %d", number)
	}

	// The single-MR endpoint carries head_pipeline (the list endpoint
	// omits it), so resolve the pipeline id from the MR first.
	result, err := f.core.runBinary("glab", cwd, "api", gitLabMREndpoint(project, number))
	if err != nil {
		return CIPipeline{}, normalizeGitLabCLIError(err)
	}
	if result.exitCode != 0 {
		return CIPipeline{}, gitlabCommandFailure("glab api merge request view failed", result)
	}
	var mr struct {
		HeadPipeline *struct {
			ID     int    `json:"id"`
			Status string `json:"status"`
			WebURL string `json:"web_url"`
		} `json:"head_pipeline"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &mr); err != nil {
		return CIPipeline{}, fmt.Errorf("glab api merge request view returned malformed JSON: %w", err)
	}
	if mr.HeadPipeline == nil || mr.HeadPipeline.ID <= 0 {
		return CIPipeline{}, nil
	}

	jobs, err := f.gitlabPipelineJobs(cwd, project, mr.HeadPipeline.ID)
	if err != nil {
		return CIPipeline{}, err
	}
	return CIPipeline{
		Status: NormalizeCIStatus(mr.HeadPipeline.Status, ""),
		URL:    mr.HeadPipeline.WebURL,
		Stages: groupGitLabJobsByStage(jobs),
	}, nil
}

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

func (f *gitlabForge) gitlabPipelineJobs(cwd, project string, pipelineID int) ([]gitlabCIJobRaw, error) {
	var jobs []gitlabCIJobRaw
	for page := 1; page <= gitlabCIJobsMaxPages; page++ {
		result, err := f.core.runBinary("glab", cwd, "api", gitLabPipelineJobsEndpoint(project, pipelineID, page))
		if err != nil {
			return nil, normalizeGitLabCLIError(err)
		}
		if result.exitCode != 0 {
			return nil, gitlabCommandFailure("glab api pipeline jobs failed", result)
		}
		var pageJobs []gitlabCIJobRaw
		if err := json.Unmarshal([]byte(result.stdout), &pageJobs); err != nil {
			return nil, fmt.Errorf("glab api pipeline jobs returned malformed JSON: %w", err)
		}
		jobs = append(jobs, pageJobs...)
		if len(pageJobs) < gitlabCIJobsPerPage {
			break
		}
	}
	return jobs, nil
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

func (f *gitlabForge) GetCIJobLog(cwd, project, jobID string) (string, error) {
	if strings.TrimSpace(project) == "" {
		return "", errors.New("project (namespace/repo) is required")
	}
	if err := ValidateCIJobID(jobID); err != nil {
		return "", err
	}
	result, err := f.core.runBinaryWithLimit("glab", cwd, maxCILogBytes,
		"api", gitLabJobTraceEndpoint(project, jobID))
	if err != nil {
		return "", normalizeGitLabCLIError(err)
	}
	if result.exitCode != 0 {
		return "", gitlabCommandFailure("glab api job trace failed", result)
	}
	return cleanGitLabTrace(result.stdout), nil
}

var (
	// Timestamped trace prefix (GitLab 17+): "<RFC3339 ts> 00O+ " —
	// two-digit stream number, O/E stream type, optional continuation
	// marker. The timestamp is kept; the stream flags are noise.
	gitlabTraceStreamPrefix = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d+Z) \d{2}[OE][+ ]?`)
	// Collapsible-section protocol markers: section_start:<unix>:<name>
	// optionally followed by [key=value,...] options and a \r that
	// erases the marker in a real terminal.
	gitlabSectionMarker = regexp.MustCompile(`section_(?:start|end):\d+:[A-Za-z0-9_.-]+(?:\[[^\]]*\])?\r?`)
	// CSI erase-in-line (ESC[K / ESC[0K / ESC[1K / ESC[2K); the ANSI
	// renderer handles colors but not erase controls, so they'd leak
	// through as visible "[0K" artifacts.
	ansiEraseInLine = regexp.MustCompile("\x1b\\[[0-2]?K")
)

// cleanGitLabTrace normalizes a raw job trace for plain-text display:
// stream flags are stripped from timestamped lines (timestamp kept),
// section_start/section_end markers and erase-line escapes are removed
// (they only mean something to GitLab's log viewer), and carriage-return
// overwrites are resolved terminal-style — the final rewrite of a
// progress line wins.
func cleanGitLabTrace(raw string) string {
	lines := strings.Split(raw, "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		timestamp := ""
		if m := gitlabTraceStreamPrefix.FindStringSubmatch(line); m != nil {
			timestamp = m[1]
			line = line[len(m[0]):]
		}
		hadMarker := gitlabSectionMarker.MatchString(line)
		line = gitlabSectionMarker.ReplaceAllString(line, "")
		if i := strings.LastIndexByte(line, '\r'); i >= 0 {
			line = line[i+1:]
		}
		line = ansiEraseInLine.ReplaceAllString(line, "")
		if line == "" {
			// Marker-only lines vanish entirely; genuinely blank lines
			// stay blank (a bare timestamp would read as an artifact).
			if !hadMarker {
				cleaned = append(cleaned, "")
			}
			continue
		}
		if timestamp != "" {
			line = timestamp + " " + line
		}
		cleaned = append(cleaned, line)
	}
	return strings.Join(cleaned, "\n")
}
