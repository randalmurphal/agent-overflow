package harnessrpc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"agent-overflow/internal/harness/forgefake"
)

// A seeded pull's head is bound to a git repository when the e2e helpers
// published it to the fake forge's origin: a bare repository at
// <data root>/forge-origin/<project>.git (e2e/tests/forge-helpers.ts,
// publishPullRequest), which the workspace's git proxy serves as the
// forge's origin. A commit the fake makes on such a pull (an applied
// suggestion) is written there as a real commit on the head branch and
// the pull's head ref, so the app can fetch the head the forge reports.

// forgeOriginRoot is where the e2e helpers keep the fake forge's origin
// repositories.
func forgeOriginRoot(dataRoot string) string {
	return filepath.Join(dataRoot, "forge-origin")
}

// forgePullHeadRef is the ref a forge keeps a pull's head under.
func forgePullHeadRef(forge string, number int) string {
	if forge == "gitlab" {
		return fmt.Sprintf("refs/merge-requests/%d/head", number)
	}
	return fmt.Sprintf("refs/pull/%d/head", number)
}

// forgeOriginCommit is forgefake.Options.Commit for the origin
// repositories under root: "" when the pull's head repository has no
// repository there or it lacks the parent commit, else the sha of the
// commit written: Path set to Content (keeping the path's mode) on top of
// Parent, with Message, on refs/heads/<Branch> (which must still point
// at Parent) and the pull's head ref.
func forgeOriginCommit(root string) func(forgefake.CommitRequest) (string, error) {
	return func(req forgefake.CommitRequest) (string, error) {
		if root == "" || req.Project == "" || strings.Contains(req.Project, "..") {
			return "", nil
		}
		bare := filepath.Join(root, filepath.FromSlash(req.Project)+".git")
		if info, err := os.Stat(bare); err != nil || !info.IsDir() {
			return "", nil
		}
		index, err := os.CreateTemp("", "ao-forge-origin-index-*")
		if err != nil {
			return "", err
		}
		indexPath := index.Name()
		_ = index.Close()
		_ = os.Remove(indexPath) // git writes it; only the name is needed
		defer func() { _ = os.Remove(indexPath) }()
		git := func(stdin string, args ...string) (string, error) {
			cmd := exec.Command("git", args...)
			cmd.Env = forgeOriginGitEnv(bare, indexPath)
			cmd.Stdin = strings.NewReader(stdin)
			var out, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &stderr
			if err := cmd.Run(); err != nil {
				return "", fmt.Errorf("git %s in %s: %w: %s", strings.Join(args, " "), bare, err, strings.TrimSpace(stderr.String()))
			}
			return strings.TrimSpace(out.String()), nil
		}
		if _, err := git("", "cat-file", "-e", req.Parent+"^{commit}"); err != nil {
			return "", nil // a pull seeded with a head the repository lacks is bound to none
		}
		mode := "100644"
		entry, err := git("", "ls-tree", req.Parent, "--", req.Path)
		if err != nil {
			return "", err
		}
		if fields := strings.Fields(entry); len(fields) > 0 {
			mode = fields[0]
		}
		blob, err := git(req.Content, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", err
		}
		if _, err := git("", "read-tree", req.Parent); err != nil {
			return "", err
		}
		if _, err := git("", "update-index", "--add", "--cacheinfo", mode+","+blob+","+req.Path); err != nil {
			return "", err
		}
		tree, err := git("", "write-tree")
		if err != nil {
			return "", err
		}
		sha, err := git(req.Message, "commit-tree", tree, "-p", req.Parent, "-F", "-")
		if err != nil {
			return "", err
		}
		if req.Branch == "" {
			return "", errors.New("the pull has no head branch")
		}
		if _, err := git("", "update-ref", "refs/heads/"+req.Branch, sha, req.Parent); err != nil {
			return "", err
		}
		if _, err := git("", "update-ref", forgePullHeadRef(req.Forge, req.Number), sha); err != nil {
			return "", err
		}
		return sha, nil
	}
}

// forgeOriginGitEnv runs git on the bare repository with no system or
// global configuration and a fixed identity, as the forge commits.
func forgeOriginGitEnv(bare, index string) []string {
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env,
		"GIT_DIR="+bare,
		"GIT_INDEX_FILE="+index,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=Agent Overflow Harness",
		"GIT_AUTHOR_EMAIL=harness@agent-overflow.invalid",
		"GIT_COMMITTER_NAME=Agent Overflow Harness",
		"GIT_COMMITTER_EMAIL=harness@agent-overflow.invalid",
	)
}
