package cilog

import (
	"bytes"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Section is one row of a job log: a GitHub step, a GitLab top-level
// section, a run of GitLab lines outside every section, or the whole log
// when nothing splits it. Lines [Start, End) of the log are its text; a
// step the log has not reached has none.
type Section struct {
	// Key is stable for the job while its log grows.
	Key  string `json:"key"`
	Name string `json:"name"`
	// Status is a CI status (success, failed, running, pending, ...), or
	// SectionDone for a closed GitLab section (GitLab reports no result
	// per section) and SectionOutput for lines outside every section.
	Status          string  `json:"status"`
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
	Start           int     `json:"start"`
	End             int     `json:"end"`
	// Lines is End - Start; Bytes is the size of those lines with their
	// newlines.
	Lines int   `json:"lines"`
	Bytes int64 `json:"bytes"`
}

const (
	SectionDone   = "done"
	SectionOutput = "output"
)

// Job is what a log's rows are named and split by: the job's name and
// status, and on GitHub its steps.
type Job struct {
	Name   string
	Status string
	Steps  []Step
}

// Step is one GitHub Actions step as the jobs API reports it. StartedAt
// and CompletedAt are RFC 3339, to the second, empty until it starts or
// completes.
type Step struct {
	Number      int
	Name        string
	Status      string
	StartedAt   string
	CompletedAt string
}

const (
	statusRunning = "running"
	statusPending = "pending"
	statusSkipped = "skipped"
)

func jobLive(status string) bool { return status == statusRunning || status == statusPending }

// gitlabSections are a trace's spans as rows, given the job: a section the
// trace leaves open is running while the job is live and takes its status
// once it ended.
func gitlabSections(spans []span, job Job, idx lineIndex) []Section {
	out := make([]Section, 0, len(spans))
	for _, s := range spans {
		row := Section{Key: s.Key, Name: s.Name, Start: s.Start, End: s.End, Bytes: s.EndByte - s.StartByte, DurationSeconds: s.Duration}
		switch s.Kind {
		case spanSection:
			row.Status = SectionDone
		case spanOutput:
			row.Status = SectionOutput
		case spanOpen:
			row.Status = job.Status
			if jobLive(job.Status) {
				row.Status = statusRunning
			}
		case spanJob:
			row.Name, row.Status = job.Name, job.Status
		}
		row.Lines = row.End - row.Start
		out = append(out, row)
	}
	return out
}

// stepStartLine matches the lines the runner writes as a step's first
// line: a run step's group, a composite action's preparation, a post
// step, container setup and teardown, job completion. stepStartPrefix
// matches a line's start, stepStartWhole a whole line.
var (
	stepStartPrefix = regexp.MustCompile(`^(?:##\[group\]Run |Stop and remove container: )`)
	stepStartWhole  = regexp.MustCompile(`^(?:##\[group\]Checking docker version|Prepare all required actions|Post job cleanup\.|Cleaning up orphan processes)$`)
)

// stepOwnLine reports whether body is the first line the runner writes for
// a step of this name, where the name decides it. Set up job also
// prepares actions, so a composite step's "Prepare all required actions"
// is not one of these.
func stepOwnLine(name string, body []byte, whole bool) bool {
	switch {
	case name == "Initialize containers":
		return whole && string(body) == "##[group]Checking docker version"
	case name == "Stop containers":
		return bytes.HasPrefix(body, []byte("Stop and remove container: "))
	case name == "Complete job":
		return whole && string(body) == "Cleaning up orphan processes"
	case strings.HasPrefix(name, "Post "):
		return whole && string(body) == "Post job cleanup."
	}
	return whole && string(body) == "##[group]"+name
}

func stepStart(body []byte, whole bool) bool {
	return stepStartPrefix.Match(body) || (whole && stepStartWhole.Match(body))
}

// githubBody is a GitHub line without its time prefix; whole reports that
// the head holds the whole line.
func githubBody(head []byte, length int64) ([]byte, bool) {
	whole := int64(len(head)) == length
	if length > githubPrefix && len(head) >= githubPrefix && githubTime.Match(head[:githubPrefix]) {
		return head[githubPrefix:], whole
	}
	return head, whole
}

// utcSecond is an RFC 3339 time floored to the second, as Unix time.
func utcSecond(at string) (int64, bool) {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}

func secondsBetween(startedAt, completedAt string) float64 {
	start, err1 := time.Parse(time.RFC3339, startedAt)
	end, err2 := time.Parse(time.RFC3339, completedAt)
	if err1 != nil || err2 != nil || end.Before(start) {
		return 0
	}
	return end.Sub(start).Seconds()
}

// githubSections splits a GitHub log by its steps. Every line starts with
// its own time to 100 ns, while the jobs API gives each step's start
// floored to the second, and several steps often start in the same
// second. A step's first line is at or after its start second, so the
// time bounds where it can begin; inside the start second, up to and
// including the first line of a later second, the line the runner writes
// when this step begins decides (stepOwnLine), then any step-start line.
// Skipped steps and steps not reached write no lines and take no part in
// the split.
func githubSections(r lineReader, marks []secondMark, job Job) ([]Section, error) {
	lines := r.idx.lines
	ordered := slices.Clone(job.Steps)
	slices.SortStableFunc(ordered, func(a, b Step) int { return a.Number - b.Number })
	type logged struct {
		step   Step
		second int64
	}
	var steps []logged
	for _, step := range ordered {
		if step.Status == statusSkipped || step.StartedAt == "" {
			continue
		}
		if second, ok := utcSecond(step.StartedAt); ok {
			steps = append(steps, logged{step: step, second: second})
		}
	}
	// secondAt is the mark holding line; -1 when the log has no time.
	markAt := func(line int) int {
		k, _ := slices.BinarySearchFunc(marks, line, func(m secondMark, line int) int { return m.line - line })
		if k < len(marks) && marks[k].line == line {
			return k
		}
		return k - 1
	}
	bounds := make(map[int]int, len(steps))
	if lines > 0 && len(steps) > 0 {
		bounds[steps[0].step.Number] = 0
		lo := 1
		for _, s := range steps[1:] {
			// The first line at or after the step's second.
			at := lines
			for k := max(markAt(lo), 0); k < len(marks) && lo < lines; k++ {
				if marks[k].second >= s.second {
					at = max(marks[k].line, lo)
					break
				}
			}
			if at >= lines {
				continue
			}
			end := lines
			if k := markAt(at); k >= 0 && marks[k].second == s.second {
				if k+1 < len(marks) {
					end = marks[k+1].line
				}
			} else {
				end = at
			}
			own, any := -1, -1
			err := r.scan(at, end, func(line int, head []byte, length int64) bool {
				body, whole := githubBody(head, length)
				if own < 0 && stepOwnLine(s.step.Name, body, whole) {
					own = line
					return false
				}
				if any < 0 && stepStart(body, whole) {
					any = line
				}
				return true
			})
			if err != nil {
				return nil, err
			}
			bound := at
			switch {
			case own >= 0:
				bound = own
			case any >= 0:
				bound = any
			}
			bounds[s.step.Number] = bound
			lo = bound + 1
		}
	}

	var starts []int
	for _, s := range steps {
		if bound, ok := bounds[s.step.Number]; ok {
			starts = append(starts, bound)
		}
	}
	var sections []Section
	for _, step := range ordered {
		if step.Status == statusSkipped {
			continue
		}
		row := Section{
			Key:             "step:" + strconv.Itoa(step.Number),
			Name:            step.Name,
			Status:          step.Status,
			DurationSeconds: secondsBetween(step.StartedAt, step.CompletedAt),
		}
		if bound, ok := bounds[step.Number]; ok {
			row.Start, row.End = bound, lines
			for _, other := range starts {
				if other > bound {
					row.End = other
					break
				}
			}
		}
		sections = append(sections, row)
	}
	// A step claimed by a later one at the same line keeps no lines.
	for i := range sections {
		if sections[i].End <= sections[i].Start {
			continue
		}
		for _, later := range sections[i+1:] {
			if later.End > later.Start {
				if later.Start <= sections[i].Start {
					sections[i].End = sections[i].Start
				}
				break
			}
		}
	}
	if len(starts) == 0 && lines > 0 {
		// No step has a start time the log can be split by: the lines are
		// the job's, above its step list.
		sections = slices.Insert(sections, 0, Section{Key: "job", Name: job.Name, Status: job.Status, Start: 0, End: lines})
	}
	for i := range sections {
		row := &sections[i]
		if row.End <= row.Start {
			row.Start, row.End = 0, 0
			continue
		}
		row.Lines = row.End - row.Start
		from, err := r.offset(row.Start)
		if err != nil {
			return nil, err
		}
		to, err := r.offset(row.End)
		if err != nil {
			return nil, err
		}
		row.Bytes = to - from
	}
	return sections, nil
}
