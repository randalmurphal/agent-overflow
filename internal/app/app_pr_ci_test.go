package app

import (
	"agent-overflow/internal/testutil/mockexec"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
	pr := gitops.PRReference{Forge: "gitlab", Namespace: "group/sub", Repo: "repo", Number: 42}
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
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}
	app := newTestAppWithStore(t)
	app.configDir = t.TempDir()

	binDir := t.TempDir()
	script := "#!/bin/sh\nprintf 'full log content\\nsecond line\\n'\n"
	mockexec.Write(t, filepath.Join(binDir, "gh"), script)
	app.forgeCLIs.fake = filepath.Join(binDir, "gh")

	pr := gitops.PRReference{Forge: "github", Namespace: "acme", Repo: "widgets", Number: 7}
	path, err := app.SavePRCIJobLog(pr, "901", "build")
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

	if _, err := app.SavePRCIJobLog(pr, "not-a-number", "build"); err == nil {
		t.Fatal("expected error for invalid job id")
	}
}

func TestSavePRCIJobLogNamesAnUnpublishedLog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}
	app := newTestAppWithStore(t)
	app.configDir = t.TempDir()

	binDir := t.TempDir()
	script := "#!/bin/sh\nprintf '{\"message\":\"Not Found\"}'\necho 'gh: Not Found (HTTP 404)' 1>&2\nexit 1\n"
	mockexec.Write(t, filepath.Join(binDir, "gh"), script)
	app.forgeCLIs.fake = filepath.Join(binDir, "gh")

	pr := gitops.PRReference{Forge: "github", Namespace: "acme", Repo: "widgets", Number: 7}
	_, err := app.SavePRCIJobLog(pr, "901", "build")
	if !errors.Is(err, errCIJobLogUnpublished) {
		t.Fatalf("SavePRCIJobLog error = %v, want the unpublished-log answer", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(app.configDir, "ci-logs")); len(entries) != 0 {
		t.Fatalf("a file was written for a log the forge does not have: %v", entries)
	}
}
