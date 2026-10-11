package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

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
func readGitLabPipeline(ctx context.Context, client *forgeapi.Client, ref PRReference, pipeline *gitlabPipelineRaw) (CIPipeline, error) {
	if pipeline == nil || pipeline.ID <= 0 {
		return CIPipeline{}, nil
	}
	request := forgeapi.Request{
		Path:  gitlabProjectPath(ref.Project()) + "/pipelines/" + strconv.FormatInt(pipeline.ID, 10) + "/jobs",
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

// GetCIJobLog reads a job's trace, keeping its last maxCILogBytes: a
// longer trace is read from its tail and starts at the first whole line.
// A 404 (the job has not started, or does not exist) is
// ErrCIJobLogNotFound. GitLab honors If-None-Match on a trace, running or
// completed.
func (f *gitlabForge) GetCIJobLog(ctx context.Context, ref PRReference, req CIJobLogRequest) (CIJobLog, error) {
	if err := ValidateCIJobID(req.JobID); err != nil {
		return CIJobLog{}, err
	}
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return CIJobLog{}, err
	}
	tail := forgeapi.NewTailBuffer(maxCILogBytes)
	request := ciLogRequest(gitlabProjectPath(ref.Project())+"/jobs/"+req.JobID+"/trace", req.ETag)
	// The trace is plain text.
	request.Header.Set("Accept", "*/*")
	resp, err := client.Stream(ctx, request, tail, ciLogReadLimit)
	if err != nil {
		if errors.Is(err, forgeapi.ErrNotFound) {
			return CIJobLog{}, fmt.Errorf("%w: %w", ErrCIJobLogNotFound, err)
		}
		return CIJobLog{}, err
	}
	if resp.NotModified {
		return CIJobLog{ETag: req.ETag, NotModified: true}, nil
	}
	text, _ := ciLogText(resp, tail)
	return CIJobLog{Text: cleanGitLabTrace(string(text)), ETag: resp.Header.Get("ETag")}, nil
}

var (
	// Timestamped trace prefix (GitLab 17+): "<RFC3339 ts> 00O+ ":
	// two-digit stream number, O/E stream type, optional continuation
	// marker. The timestamp is kept; the stream flags are noise.
	gitlabTraceStreamPrefix = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d+Z) \d{2}[OE][+ ]?`)
	// Collapsible-section protocol markers: section_start:<unix>:<name>
	// optionally followed by [key=value,...] options, then a \r that
	// erases the marker in a real terminal. The group is the marker
	// without the \r.
	gitlabSectionMarker = regexp.MustCompile(`(section_(?:start|end):\d+:[A-Za-z0-9_.-]+(?:\[[^\]]*\])?)\r?`)
	// A line of the cleaned trace that is one section marker.
	gitlabSectionMarkerLine = regexp.MustCompile(`^section_(?:start|end):\d+:[A-Za-z0-9_.-]+(?:\[[^\]]*\])?$`)
	// CSI erase-in-line (ESC[K / ESC[0K / ESC[1K / ESC[2K); the ANSI
	// renderer handles colors but not erase controls, so they'd leak
	// through as visible "[0K" artifacts.
	ansiEraseInLine = regexp.MustCompile("\x1b\\[[0-2]?K")
)

// cleanGitLabTrace normalizes a raw job trace for display. Stream flags
// are stripped from timestamped lines (the timestamp is kept), erase-line
// escapes are removed, and carriage-return overwrites are resolved
// terminal-style: the final rewrite of a progress line wins.
//
// Section markers stay, each on a line of its own holding exactly the
// marker as GitLab wrote it, section_start:<unix>:<name>[options] or
// section_end:<unix>:<name>, with no timestamp prefix (<unix> is seconds).
// They keep their order within the line: text before a marker comes
// before it, text after it after it. The line after a section_start is
// the section's header, the text GitLab shows on the collapsed section.
func cleanGitLabTrace(raw string) string {
	lines := strings.Split(raw, "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		timestamp := ""
		if m := gitlabTraceStreamPrefix.FindStringSubmatch(line); m != nil {
			timestamp = m[1]
			line = line[len(m[0]):]
		}
		markers := gitlabSectionMarker.FindAllStringSubmatchIndex(line, -1)
		if markers == nil {
			// A genuinely blank line stays blank (a bare timestamp would
			// read as an artifact).
			cleaned = append(cleaned, cleanGitLabTraceText(line, timestamp))
			continue
		}
		at := 0
		for _, m := range markers {
			if text := cleanGitLabTraceText(line[at:m[0]], timestamp); text != "" {
				cleaned = append(cleaned, text)
			}
			cleaned = append(cleaned, line[m[2]:m[3]])
			at = m[1]
		}
		if text := cleanGitLabTraceText(line[at:], timestamp); text != "" {
			cleaned = append(cleaned, text)
		}
	}
	return strings.Join(cleaned, "\n")
}

// StripCISectionMarkers drops the section marker lines cleanGitLabTrace
// keeps, for a log written out as plain text. A GitHub log has none.
func StripCISectionMarkers(text string) string {
	if !strings.Contains(text, "section_") {
		return text
	}
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if !gitlabSectionMarkerLine.MatchString(line) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// cleanGitLabTraceText is one run of trace text with its overwrites
// resolved and erase escapes removed, under the line's timestamp; empty
// when nothing visible is left.
func cleanGitLabTraceText(text, timestamp string) string {
	if i := strings.LastIndexByte(text, '\r'); i >= 0 {
		text = text[i+1:]
	}
	text = ansiEraseInLine.ReplaceAllString(text, "")
	if text == "" || timestamp == "" {
		return text
	}
	return timestamp + " " + text
}

// CILogStreams is true: the trace endpoint returns what the runner
// has uploaded so far for a running job.
func (f *gitlabForge) CILogStreams() bool { return true }
