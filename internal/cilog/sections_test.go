package cilog

import (
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// GitHub ground truth: the run-logs zip of a real run holds each job's
// combined log and, beside it, one file per step. Splitting the combined
// log by the jobs API's step times must give each step exactly its file's
// lines. Times in the two files differ in the last digits, so lines
// compare by their text after the time.

type fixtureStep struct {
	Number      int    `json:"number"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	StartedAt   string `json:"startedAt"`
	CompletedAt string `json:"completedAt"`
}

type fixtureJob struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Log       string            `json:"log"`
	Steps     []fixtureStep     `json:"steps"`
	StepFiles map[string]string `json:"stepFiles"`
}

func loadFixtureJob(t *testing.T, name string) fixtureJob {
	t.Helper()
	raw, err := os.ReadFile("testdata/githubRunLogs.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Jobs []fixtureJob `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, job := range fixture.Jobs {
		if strings.HasPrefix(job.Name, name) {
			return job
		}
	}
	t.Fatalf("no fixture job %s", name)
	return fixtureJob{}
}

func (j fixtureJob) job() Job {
	steps := make([]Step, 0, len(j.Steps))
	for _, s := range j.Steps {
		steps = append(steps, Step(s))
	}
	return Job{Name: j.Name, Status: j.Status, Steps: steps}
}

func afterTime(line string) string {
	if len(line) < githubPrefix {
		return line
	}
	return line[githubPrefix:]
}

func (j fixtureJob) stepLines(number int) []string {
	file, ok := j.StepFiles[strconv.Itoa(number)]
	if !ok {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(file, "\n"), "\n")
	for i := range lines {
		lines[i] = afterTime(lines[i])
	}
	return lines
}

func timeless(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = afterTime(line)
	}
	return out
}

// githubIndex downloads text as job's log (with the BOM the endpoint
// prepends) and indexes it.
func githubIndex(t *testing.T, text string, job Job) (*Store, Key, Index) {
	t.Helper()
	s, _, _ := newTestStore(t, CacheBytes)
	key := githubKey("1")
	fetchText(t, s, key, utf8BOM+text, `"e1"`)
	index, err := s.Index(key, job)
	if err != nil {
		t.Fatal(err)
	}
	return s, key, index
}

func TestGitHubStepsGetExactlyTheirLines(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"svelte-check", "golangci-lint"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := loadFixtureJob(t, name)
			s, key, index := githubIndex(t, fixture.Log, fixture.job())
			for _, step := range fixture.Steps {
				if step.Status == statusSkipped {
					continue
				}
				section := findSection(t, index, "step:"+strconv.Itoa(step.Number))
				if got, want := timeless(sectionText(t, s, key, section)), fixture.stepLines(step.Number); !slices.Equal(got, want) {
					t.Fatalf("step %d %s: %d lines, want %d", step.Number, step.Name, len(got), len(want))
				}
			}
			// Every line belongs to one step, in order, and the byte sizes
			// add up to the log.
			lines, bytes := 0, int64(0)
			for _, section := range index.Sections {
				if section.Lines != section.End-section.Start {
					t.Fatalf("%s: lines %d for range %d-%d", section.Key, section.Lines, section.Start, section.End)
				}
				lines += section.Lines
				bytes += section.Bytes
			}
			if lines != index.TotalLines || bytes != index.TotalBytes {
				t.Fatalf("sections cover %d lines and %d bytes of %d and %d", lines, bytes, index.TotalLines, index.TotalBytes)
			}
		})
	}
}

func TestGitHubStepsInTheSameSecondAsTheFailedOne(t *testing.T) {
	t.Parallel()
	// Steps 8, 14 and 15 all start at 00:04:31; step 8 failed.
	fixture := loadFixtureJob(t, "svelte-check")
	s, key, index := githubIndex(t, fixture.Log, fixture.job())
	failed := findSection(t, index, "step:8")
	if failed.Status != "failed" {
		t.Fatalf("step 8 status %q", failed.Status)
	}
	if lines := timeless(sectionText(t, s, key, failed)); lines[len(lines)-1] != "##[error]Process completed with exit code 2." {
		t.Fatalf("step 8 ends %q", lines[len(lines)-1])
	}
	if got := timeless(sectionText(t, s, key, findSection(t, index, "step:14"))); !slices.Equal(got, []string{"Post job cleanup."}) {
		t.Fatalf("step 14 = %q", got)
	}
	build := findSection(t, index, "step:6")
	if build.Name != "Run pnpm --dir frontend run build" || build.Status != "success" || build.DurationSeconds != 9 {
		t.Fatalf("step 6 = %+v", build)
	}
}

func TestGitHubSkippedStepHasNoRow(t *testing.T) {
	t.Parallel()
	// golangci's step 7 is skipped at 00:05:29, the second steps 12 to 14
	// start in.
	fixture := loadFixtureJob(t, "golangci-lint")
	s, key, index := githubIndex(t, fixture.Log, fixture.job())
	for _, section := range index.Sections {
		if section.Key == "step:7" {
			t.Fatal("the skipped step 7 has a row")
		}
	}
	if lines := timeless(sectionText(t, s, key, findSection(t, index, "step:6"))); lines[len(lines)-1] != "##[error]Process completed with exit code 2." {
		t.Fatalf("step 6 ends %q", lines[len(lines)-1])
	}
	if got := timeless(sectionText(t, s, key, findSection(t, index, "step:12"))); !slices.Equal(got, []string{"Post job cleanup."}) {
		t.Fatalf("step 12 = %q", got)
	}
}

func TestGitHubStepsNotReachedArePendingWithoutLines(t *testing.T) {
	t.Parallel()
	fixture := loadFixtureJob(t, "svelte-check")
	// The job as it ran step 5: later steps have not started.
	cut := slices.IndexFunc(fixture.Steps, func(step fixtureStep) bool { return step.Number == 6 })
	job := fixture.job()
	job.Status = statusRunning
	for i := range job.Steps {
		switch {
		case i == cut-1:
			job.Steps[i].Status, job.Steps[i].CompletedAt = statusRunning, ""
		case i >= cut:
			job.Steps[i] = Step{Number: job.Steps[i].Number, Name: job.Steps[i].Name, Status: statusPending}
		}
	}
	lines := strings.Split(fixture.Log, "\n")
	end := slices.IndexFunc(lines, func(line string) bool { return afterTime(line) == "##[group]Run pnpm --dir frontend run build" })
	s, key, index := githubIndex(t, strings.Join(lines[:end], "\n")+"\n", job)
	running := findSection(t, index, "step:5")
	if got := timeless(sectionText(t, s, key, running)); !slices.Equal(got, fixture.stepLines(5)) || running.Status != statusRunning || running.DurationSeconds != 0 {
		t.Fatalf("running step 5 = %+v", running)
	}
	var pending []string
	for _, section := range index.Sections {
		if section.Status == statusPending {
			pending = append(pending, section.Key)
			if section.Lines != 0 {
				t.Fatalf("pending %s has %d lines", section.Key, section.Lines)
			}
		}
	}
	if want := []string{"step:6", "step:7", "step:8", "step:14", "step:15", "step:16", "step:17"}; !slices.Equal(pending, want) {
		t.Fatalf("pending rows %v, want %v", pending, want)
	}
}

func TestGitHubLogWithoutStepTimesIsTheJob(t *testing.T) {
	t.Parallel()
	_, _, index := githubIndex(t, "a\nb\n", Job{Name: "build", Status: "failed"})
	if want := []Section{{Key: "job", Name: "build", Status: "failed", Start: 0, End: 2, Lines: 2, Bytes: 4}}; !slices.Equal(index.Sections, want) {
		t.Fatalf("sections = %+v", index.Sections)
	}
	_, _, index = githubIndex(t, "a\nb\n", Job{Name: "build", Status: "running", Steps: []Step{
		{Number: 1, Name: "Set up job", Status: statusRunning}, {Number: 2, Name: "Test", Status: statusPending},
	}})
	var got []string
	for _, section := range index.Sections {
		got = append(got, section.Key+":"+strconv.Itoa(section.Start)+"-"+strconv.Itoa(section.End))
	}
	if want := []string{"job:0-2", "step:1:0-0", "step:2:0-0"}; !slices.Equal(got, want) {
		t.Fatalf("sections = %v, want %v", got, want)
	}
}

// A raw GitLab trace as the runner writes it.
const testTrace = "Running with gitlab-runner 17.0.0\n" +
	"section_start:1700000000:prepare_executor\r\x1b[0K\x1b[36;1mPreparing the \"docker\" executor\x1b[0;m\n" +
	"Using docker image alpine\n" +
	"\x1b[0Ksection_end:1700000004:prepare_executor\r\x1b[0K\n" +
	"section_start:1700000004:step_script\r\x1b[0K2026-10-11T00:00:04.000000Z \x1b[36;1mExecuting \"step_script\" stage of the job script\x1b[0;m\n" +
	"$ make test\n" +
	"section_start:1700000005:custom_inner[collapsed=true]\r\x1b[0Kinner header\n" +
	"inner line\n" +
	"section_end:1700000006:custom_inner\r\x1b[0K\n" +
	"FAIL\n" +
	"section_end:1700000070:step_script\r\x1b[0K\n" +
	"\x1b[31;1mERROR: Job failed: exit code 1\x1b[0;m\n"

func gitlabIndex(t *testing.T, raw string, job Job) (*Store, Key, Index) {
	t.Helper()
	s, _, _ := newTestStore(t, CacheBytes)
	key := gitlabKey("9")
	fetchText(t, s, key, raw, `"t1"`)
	index, err := s.Index(key, job)
	if err != nil {
		t.Fatal(err)
	}
	return s, key, index
}

func TestGitLabTopLevelSectionsAndOutputRows(t *testing.T) {
	t.Parallel()
	s, key, index := gitlabIndex(t, testTrace, Job{Name: "unit", Status: "failed"})
	type row struct {
		key, name, status string
		duration          float64
	}
	var got []row
	for _, section := range index.Sections {
		got = append(got, row{section.Key, section.Name, section.Status, section.DurationSeconds})
	}
	want := []row{
		{"output:start", "Running with gitlab-runner 17.0.0", SectionOutput, 0},
		{"section:prepare_executor:1700000000", `Preparing the "docker" executor`, SectionDone, 4},
		{"section:step_script:1700000004", `Executing "step_script" stage of the job script`, SectionDone, 66},
		{"output:section:step_script:1700000004", "ERROR: Job failed: exit code 1", SectionOutput, 0},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rows\n%+v\nwant\n%+v", got, want)
	}
	// A nested section stays in its parent's text; no marker is a line.
	script := sectionText(t, s, key, index.Sections[2])
	if want := []string{
		"2026-10-11T00:00:04.000000Z \x1b[36;1mExecuting \"step_script\" stage of the job script\x1b[0;m",
		"$ make test", "inner header", "inner line", "FAIL",
	}; !slices.Equal(script, want) {
		t.Fatalf("step_script text %q", script)
	}
	for _, line := range allLines(t, s, key) {
		if strings.HasPrefix(line, "section_") {
			t.Fatalf("a marker is a line: %q", line)
		}
	}
	total := int64(0)
	for _, section := range index.Sections {
		total += section.Bytes
	}
	if total != index.TotalBytes {
		t.Fatalf("section bytes %d of %d", total, index.TotalBytes)
	}
}

func TestGitLabOpenSectionIsRunningWhileTheJobRuns(t *testing.T) {
	t.Parallel()
	running := strings.Join(strings.SplitAfter(testTrace, "\n")[:8], "")
	s, key, index := gitlabIndex(t, running, Job{Name: "unit", Status: statusRunning})
	last := index.Sections[len(index.Sections)-1]
	if last.Key != "section:step_script:1700000004" || last.Status != statusRunning || last.DurationSeconds != 0 {
		t.Fatalf("open section = %+v", last)
	}
	if lines := sectionText(t, s, key, last); lines[len(lines)-1] != "inner line" {
		t.Fatalf("open section ends %q", lines[len(lines)-1])
	}
	// The same rows once the job failed: the open section takes its status.
	index, err := s.Index(key, Job{Name: "unit", Status: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	if got := index.Sections[len(index.Sections)-1].Status; got != "failed" {
		t.Fatalf("open section of a failed job = %q", got)
	}
}

func TestGitLabTraceWithoutSectionsIsTheJob(t *testing.T) {
	t.Parallel()
	_, _, index := gitlabIndex(t, "line 1\nline 2\n", Job{Name: "unit", Status: statusRunning})
	if want := []Section{{Key: "job", Name: "unit", Status: statusRunning, Start: 0, End: 2, Lines: 2, Bytes: 14}}; !slices.Equal(index.Sections, want) {
		t.Fatalf("sections = %+v", index.Sections)
	}
}
