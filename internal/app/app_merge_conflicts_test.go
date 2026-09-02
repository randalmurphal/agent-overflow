package app

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/testutil"
)

func TestGetPRMergeConflictsHappyPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock git is unix-only")
	}
	app := newTestAppWithStore(t)
	workspace := testutil.InitGitRepo(t)
	ref := testWorkspaceRef(t, app, workspace)

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "git.log")
	gitPath := filepath.Join(binDir, "git")
	headOID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	treeOID := "cccccccccccccccccccccccccccccccccccccccc"
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$AO_GIT_LOG"
case "$*" in
  "rev-parse --is-inside-work-tree")
    echo true
    exit 0
    ;;
  "fetch origin pull/9/head")
    exit 0
    ;;
  "rev-parse FETCH_HEAD")
    echo "$AO_HEAD_OID"
    exit 0
    ;;
  "fetch origin main")
    exit 0
    ;;
  "merge-tree --write-tree --name-only origin/main $AO_HEAD_OID")
    cat <<EOF
$AO_TREE_OID
main.go

CONFLICT (content): Merge conflict in main.go
EOF
    exit 1
    ;;
  "show $AO_TREE_OID:main.go")
    cat <<EOF
package main
<<<<<<< ours
left
=======
right
>>>>>>> theirs
EOF
    exit 0
    ;;
  *)
    echo "unexpected args: $*" >&2
    exit 9
    ;;
esac
`
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AO_GIT_LOG", logPath)
	t.Setenv("AO_HEAD_OID", headOID)
	t.Setenv("AO_TREE_OID", treeOID)
	app.git = gitops.NewCore()

	pr := gitops.PRReference{Forge: "github", Namespace: "owner", Repo: "repo", Number: 9}
	result, err := app.GetPRMergeConflicts(ref, pr, "main", "feature-branch")
	if err != nil {
		t.Fatalf("GetPRMergeConflicts: %v", err)
	}
	if !result.Conflicted || result.TreeOID != treeOID || result.BaseLabel != "origin/main" || result.HeadLabel != "feature-branch" {
		t.Fatalf("result = %+v", result)
	}
	if strings.Join(result.Paths, ",") != "main.go" {
		t.Fatalf("paths = %v", result.Paths)
	}

	content, err := app.GetMergeConflictFile(ref, treeOID, "main.go")
	if err != nil {
		t.Fatalf("GetMergeConflictFile: %v", err)
	}
	if !strings.Contains(content, "<<<<<<< ours") || !strings.Contains(content, ">>>>>>> theirs") {
		t.Fatalf("content missing conflict markers:\n%s", content)
	}

	var conflictCalls []string
	for _, line := range strings.Split(strings.TrimSpace(readAppTestFile(t, logPath)), "\n") {
		if line == "rev-parse --is-inside-work-tree" {
			continue
		}
		conflictCalls = append(conflictCalls, line)
	}
	want := []string{
		"fetch origin pull/9/head",
		"rev-parse FETCH_HEAD",
		"fetch origin main",
		"merge-tree --write-tree --name-only origin/main " + headOID,
		"show " + treeOID + ":main.go",
	}
	if strings.Join(conflictCalls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("git calls:\n%s\nwant:\n%s", strings.Join(conflictCalls, "\n"), strings.Join(want, "\n"))
	}
}

func readAppTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestGetPRMergeConflictsRequiresLocalClone(t *testing.T) {
	app := newTestAppWithStore(t)
	// A pr-anchor caller has no project of its own: the zero ref is exactly
	// what the frontend sends for one, and it must read as "no local clone"
	// rather than a project-resolution failure.
	pr := gitops.PRReference{Forge: "github", Namespace: "owner", Repo: "repo", Number: 9}
	_, err := app.GetPRMergeConflicts(WorkspaceRef{}, pr, "main", "")
	if err == nil {
		t.Fatal("expected local clone error")
	}
	if err.Error() != "viewing conflicts requires a local clone" {
		t.Fatalf("error = %q", err.Error())
	}
}
