package app

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	gitops "agent-overflow/internal/git"
)

func TestCILogFileName(t *testing.T) {
	t.Parallel()
	pr := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "group/sub", Repo: "repo", Number: 42}
	name := ciLogFileName(pr, "1234", "unit tests (linux/amd64)")
	if name != "gitlab-group-sub-repo-pr42-1234-unit-tests--linux-amd64.log" {
		t.Fatalf("name = %q", name)
	}
	if strings.ContainsAny(name, "/\\ ") {
		t.Fatalf("name contains unsafe characters: %q", name)
	}

	long := strings.Repeat("x", 200)
	if got := sanitizeCIFileSegment(long); len(got) > 60 {
		t.Fatalf("segment not capped: %d chars", len(got))
	}
}

// savedLogs lists the saved logs in the ci-logs directory, leaving out the
// cache beside them.
func savedLogs(t *testing.T, app *App) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(app.configDir, "ci-logs", "*"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, name := range names {
		if filepath.Base(name) != "cache" {
			out = append(out, filepath.Base(name))
		}
	}
	return out
}

// Save writes the whole log, downloading it into the cache when the cache
// does not hold it; a re-save copies the cached log without asking the
// forge and refreshes the same file.
func TestSavePRCIJobLogWritesFullLog(t *testing.T) {
	t.Parallel()
	app := NewApp()
	app.configDir = t.TempDir()
	const content = "full log content\nsecond line\n"
	var requests atomic.Int32
	app.git = githubAPITestCore(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/github/rest/repos/acme/widgets/actions/jobs/901/logs" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		_, _ = io.WriteString(w, content)
	})

	pr := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widgets", Number: 7}
	path, err := app.SavePRCIJobLog(t.Context(), pr, "901", "build")
	if err != nil {
		t.Fatalf("SavePRCIJobLog: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("path %q is not absolute", path)
	}
	if filepath.Base(path) != "github-acme-widgets-pr7-901-build.log" {
		t.Fatalf("unexpected file name %q", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved log: %v", err)
	}
	if string(data) != content {
		t.Fatalf("saved content = %q", data)
	}
	again, err := app.SavePRCIJobLog(t.Context(), pr, "901", "build")
	if err != nil || again != path || requests.Load() != 1 {
		t.Fatalf("re-save = %q, %v after %d requests; want the same file from the cache", again, err, requests.Load())
	}
	if got := savedLogs(t, app); !slices.Equal(got, []string{filepath.Base(path)}) {
		t.Fatalf("ci-logs holds %v", got)
	}

	if _, err := app.SavePRCIJobLog(t.Context(), pr, "not-a-number", "build"); err == nil {
		t.Fatal("expected error for invalid job id")
	}
}

func TestSavePRCIJobLogNamesAnUnpublishedLog(t *testing.T) {
	t.Parallel()
	app := NewApp()
	app.configDir = t.TempDir()
	app.git = githubAPITestCore(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})

	pr := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widgets", Number: 7}
	_, err := app.SavePRCIJobLog(t.Context(), pr, "901", "build")
	if !errors.Is(err, errCIJobLogUnpublished) {
		t.Fatalf("SavePRCIJobLog error = %v, want the unpublished-log answer", err)
	}
	if got := savedLogs(t, app); len(got) != 0 {
		t.Fatalf("a file was written for a log the forge does not have: %v", got)
	}
}

func TestSavePRCIJobLogDropsGitLabSectionMarkers(t *testing.T) {
	t.Parallel()
	app := NewApp()
	app.configDir = t.TempDir()
	const trace = "Running with gitlab-runner\n" +
		"section_start:1714557600:step_script\r\x1b[0KExecuting\n" +
		"$ make test\n" +
		"section_end:1714557605:step_script\r\x1b[0K\n"
	app.git = githubAPITestCore(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/jobs/901/trace") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, trace)
	})

	pr := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "acme", Repo: "widgets", Number: 7}
	path, err := app.SavePRCIJobLog(t.Context(), pr, "901", "unit")
	if err != nil {
		t.Fatalf("SavePRCIJobLog: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved log: %v", err)
	}
	if want := "Running with gitlab-runner\nExecuting\n$ make test\n"; string(data) != want {
		t.Fatalf("saved content = %q, want %q", data, want)
	}
}

// A download the forge cuts off is resumed until the log is whole before
// it is saved; a failed save leaves no file under the name.
func TestSavePRCIJobLogResumesACutDownload(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	body := strings.Repeat("0123456789abcdef\n", 1000)
	f.setLog("7", body, nil)
	f.cutLog("7", 5000)
	path, err := f.app.SavePRCIJobLog(t.Context(), testPR, "7", "unit")
	if err != nil {
		t.Fatalf("SavePRCIJobLog: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != body {
		t.Fatalf("saved %d bytes, %v; want the whole %d", len(data), err, len(body))
	}
	if requests := f.requests(); len(requests) != 2 || requests[1].From != 5000 {
		t.Fatalf("requests %+v, want the cut download and its resume from 5000", requests)
	}
	// A cache cleared under the saved log is downloaded again by the next
	// save.
	if err := os.RemoveAll(filepath.Join(f.app.configDir, "ci-logs", "cache")); err != nil {
		t.Fatal(err)
	}
	if again, err := f.app.SavePRCIJobLog(t.Context(), testPR, "7", "unit"); err != nil || again != path {
		t.Fatalf("save after the cache was cleared = %q, %v", again, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != body {
		t.Fatalf("saved %d bytes after the cache was cleared, %v", len(data), err)
	}

	f.setLog("8", "", errors.New("forge down"))
	if _, err := f.app.SavePRCIJobLog(t.Context(), testPR, "8", "lint"); err == nil {
		t.Fatal("a failed download was saved")
	}
	if got := savedLogs(t, f.app); !slices.Equal(got, []string{filepath.Base(path)}) {
		t.Fatalf("ci-logs holds %v after a failed save", got)
	}
}

// githubStepLog is a GitHub job log of two steps, as the jobs API times
// them in githubSteps.
const githubStepLog = "2026-10-11T00:00:00.1000000Z Current runner version: '2.329.0'\n" +
	"2026-10-11T00:00:01.1000000Z ##[group]Run make test\n" +
	"2026-10-11T00:00:01.2000000Z \x1b[32mok\x1b[0m\n"

var githubSteps = []gitops.CIStep{
	{Number: 1, Name: "Set up job", Status: gitops.CIStatusSuccess, StartedAt: "2026-10-11T00:00:00Z", CompletedAt: "2026-10-11T00:00:01Z"},
	{Number: 2, Name: "Run make test", Status: gitops.CIStatusSuccess, StartedAt: "2026-10-11T00:00:01Z", CompletedAt: "2026-10-11T00:00:02Z"},
}

// A section is saved from the cached log by the key the follow's index
// names, server-side, whatever its size.
func TestSavePRCIJobLogSectionWritesTheSection(t *testing.T) {
	t.Parallel()
	f := newPRCIFixture(t, true)
	f.publish(pipelineWith(gitops.CIStatusSuccess, gitops.CIJob{ID: "901", Name: "build", Status: gitops.CIStatusSuccess, LogsAvailable: true, Steps: githubSteps}), nil)
	f.setLog("901", githubStepLog, nil)
	sub := f.subscribe(t)
	awaitCIEvent(t, f.ciEvents, "pipeline frame")
	pr := testPR
	if _, err := f.app.SavePRCIJobLogSection(pr, "901", "build", "step:2"); !errors.Is(err, errCILogNotDownloaded) {
		t.Fatalf("section of a log not downloaded = %v", err)
	}
	result, err := f.app.SetPRCILogFollows(t.Context(), sub.ID, []string{"901"})
	if err != nil {
		t.Fatalf("SetPRCILogFollows: %v", err)
	}
	sections := result.Logs["901"].Sections
	if len(sections) != 2 || sections[1].Key != "step:2" || sections[1].Start != 1 || sections[1].Lines != 2 {
		t.Fatalf("sections = %+v", sections)
	}

	path, err := f.app.SavePRCIJobLogSection(pr, "901", "build", "step:2")
	if err != nil {
		t.Fatalf("SavePRCIJobLogSection: %v", err)
	}
	if filepath.Base(path) != "github-owner-repo-pr9-901-build-Run-make-test.log" {
		t.Fatalf("unexpected file name %q", filepath.Base(path))
	}
	// The file is plain text: the escape sequences the view colors with
	// are dropped.
	want := "2026-10-11T00:00:01.1000000Z ##[group]Run make test\n2026-10-11T00:00:01.2000000Z ok\n"
	if data, err := os.ReadFile(path); err != nil || string(data) != want {
		t.Fatalf("saved content = %q, %v; want %q", data, err, want)
	}
	// A re-save refreshes the same file.
	if again, err := f.app.SavePRCIJobLogSection(pr, "901", "build", "step:2"); err != nil || again != path {
		t.Fatalf("re-save = %q, %v; want %q", again, err, path)
	}

	if _, err := f.app.SavePRCIJobLogSection(pr, "901", "build", "step:9"); err == nil {
		t.Fatal("a section the index does not name was saved")
	}
	if _, err := f.app.SavePRCIJobLogSection(pr, "../x", "build", "step:2"); err == nil {
		t.Fatal("expected error for invalid job id")
	}
	if _, err := f.app.SavePRCIJobLogSection(gitops.PRReference{Forge: "github"}, "901", "build", "step:2"); err == nil {
		t.Fatal("expected error for an invalid pull request")
	}
	if _, err := f.app.SavePRCIJobLogSection(pr, "901", "build", ""); err == nil {
		t.Fatal("expected error for an empty section key")
	}
	if got := savedLogs(t, f.app); !slices.Equal(got, []string{filepath.Base(path)}) {
		t.Fatalf("ci-logs holds %v, want only the saved section", got)
	}
}
