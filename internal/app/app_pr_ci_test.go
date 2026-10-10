package app

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
)

func TestTailCapLog(t *testing.T) {
	t.Parallel()
	full, truncated := tailCapLog("a\nb\nc\n", 100)
	if truncated || full != "a\nb\nc\n" {
		t.Fatalf("short log must pass through, got (%q, %v)", full, truncated)
	}

	log := "line one is long\nline two\nline three\n"
	tail, truncated := tailCapLog(log, 15)
	if !truncated {
		t.Fatal("expected truncation")
	}
	// The tail must start at a line boundary, never mid-line.
	if tail != "line three\n" {
		t.Fatalf("tail = %q, want %q", tail, "line three\n")
	}
}

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

func TestSavePRCIJobLogWritesFullLog(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.configDir = t.TempDir()
	const content = "full log content\nsecond line\n"
	app.git = githubAPITestCore(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/github/rest/repos/acme/widgets/actions/jobs/901/logs" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
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
	if string(data) != "full log content\nsecond line\n" {
		t.Fatalf("saved content = %q", data)
	}

	if _, err := app.SavePRCIJobLog(t.Context(), pr, "not-a-number", "build"); err == nil {
		t.Fatal("expected error for invalid job id")
	}
}

func TestSavePRCIJobLogNamesAnUnpublishedLog(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
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
	if entries, _ := os.ReadDir(filepath.Join(app.configDir, "ci-logs")); len(entries) != 0 {
		t.Fatalf("a file was written for a log the forge does not have: %v", entries)
	}
}
