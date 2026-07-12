// Package harness holds the backend-side engines of the agent test
// harness: workspace git fixtures, and the wire-level event replayer.
// The RPC surface that drives them lives in the main package
// (app_harness*.go); the mock-provider script format and control wire
// live in the scenario/ and control/ subpackages.
package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"agent-overflow/internal/workspacepath"
)

// RepoSpec describes a throwaway git repository a seed builds for a
// project workspace, so git-dependent features (checkpoints, diffs,
// git status, branches) operate on real history instead of stubs.
type RepoSpec struct {
	// DefaultBranch defaults to "main".
	DefaultBranch string `json:"defaultBranch,omitempty"`
	// Commits apply in order; each writes its files, stages everything,
	// and commits. An empty list still produces a repo with one empty
	// initial commit so HEAD exists.
	Commits []CommitSpec `json:"commits,omitempty"`
	// Dirty files are written after the last commit and left unstaged —
	// the state diff views and git-status badges light up on.
	Dirty map[string]string `json:"dirty,omitempty"`
}

// CommitSpec is one commit: file contents by workspace-relative path.
type CommitSpec struct {
	Message string            `json:"message,omitempty"`
	Files   map[string]string `json:"files,omitempty"`
}

// CreateRepo materialises spec at dir (created; must not already
// contain a .git). Identity comes from repo-local config so the result
// is independent of the harness HOME's .gitconfig.
func CreateRepo(dir string, spec RepoSpec) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("harness: create repo dir: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return fmt.Errorf("harness: %s is already a git repository", dir)
	}

	branch := spec.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	if err := runGit(dir, "init", "-b", branch); err != nil {
		// Older git without -b: init then create the branch explicitly.
		if err := runGit(dir, "init"); err != nil {
			return err
		}
		if err := runGit(dir, "checkout", "-b", branch); err != nil {
			return err
		}
	}
	for _, kv := range [][2]string{
		{"user.name", "Agent Overflow Harness"},
		{"user.email", "harness@agent-overflow.invalid"},
		{"commit.gpgsign", "false"},
	} {
		if err := runGit(dir, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}

	commits := spec.Commits
	if len(commits) == 0 {
		commits = []CommitSpec{{Message: "initial commit"}}
	}
	for i, commit := range commits {
		if err := writeWorkspaceFiles(dir, commit.Files); err != nil {
			return fmt.Errorf("harness: commit %d: %w", i+1, err)
		}
		if err := runGit(dir, "add", "-A"); err != nil {
			return err
		}
		msg := commit.Message
		if msg == "" {
			msg = fmt.Sprintf("commit %d", i+1)
		}
		if err := runGit(dir, "commit", "--allow-empty", "-m", msg); err != nil {
			return err
		}
	}

	if err := writeWorkspaceFiles(dir, spec.Dirty); err != nil {
		return fmt.Errorf("harness: dirty files: %w", err)
	}
	return nil
}

// writeWorkspaceFiles writes each rel→content pair under root,
// validating every path through the shared workspace-relative rules so
// a seed spec can't escape its own repo dir.
func writeWorkspaceFiles(root string, files map[string]string) error {
	for rel, content := range files {
		clean, err := workspacepath.NormalizeRelative(rel)
		if err != nil {
			return fmt.Errorf("file %q: %w", rel, err)
		}
		full := filepath.Join(root, clean)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("file %q: %w", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return fmt.Errorf("file %q: %w", rel, err)
		}
	}
	return nil
}

func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("harness: git %s in %s: %w: %s", strings.Join(args, " "), dir, err, strings.TrimSpace(string(out)))
	}
	return nil
}
