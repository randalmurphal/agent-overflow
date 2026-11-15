package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// checkout is a repository on branch feature, one commit, pushed to origin.
func checkout(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "feature"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
		{"remote", "add", "origin", origin},
		// Pushed: the open-PR lookup asks only for a branch origin has.
		{"update-ref", "refs/remotes/origin/feature", "HEAD"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// A checkout whose origin is on GitHub Enterprise (Server or GHE.com):
// git status reads, the PR features carry one sentence, a forge
// operation fails with it, and nothing reaches the forge, gh's login
// included, though the fake holds an open pull for the branch. A
// github.com checkout on the same gh login still reads as GitHub.
func TestGitHubEnterpriseOriginsSayTheyAreUnsupportedAndCallNoForge(t *testing.T) {
	r := newRig(t, forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{
		{Forge: "github", Host: "github.acme.corp", Project: "corp/widgets", Pulls: []forgefake.Pull{{Number: 1, Title: "Open", HeadRef: "feature"}}},
		{Forge: "github", Project: "acme/widgets", Pulls: []forgefake.Pull{{Number: 2, Title: "Open", HeadRef: "feature"}}},
	}})
	for _, tc := range []struct{ origin, host string }{
		{"git@github.acme.corp:corp/widgets.git", "github.acme.corp"},
		{"https://acme.ghe.com/corp/widgets.git", "acme.ghe.com"},
	} {
		dir := checkout(t, tc.origin)
		status, err := r.core.Status(t.Context(), dir)
		if err != nil {
			t.Fatalf("%s Status: %v", tc.origin, err)
		}
		want := "Pull requests on GitHub Enterprise (" + tc.host + ") are not supported yet."
		if !status.IsRepo || status.Branch != "feature" || status.Forge != "" || status.ForgeUnsupported != want || status.OpenPRURL != "" || status.OpenPRLookupError != "" {
			t.Fatalf("%s status = %+v", tc.origin, status)
		}
		_, err = r.core.OriginRepository(t.Context(), dir)
		if !errors.Is(err, gitops.ErrOriginNotForge) || !strings.Contains(err.Error(), "the origin host "+tc.host+" is GitHub Enterprise, which this release does not support") {
			t.Fatalf("%s OriginRepository = %v", tc.origin, err)
		}
		if _, err := r.core.ListOpenPRs(t.Context(), dir, "feature"); err == nil {
			t.Fatalf("%s ListOpenPRs succeeded", tc.origin)
		}
	}
	if calls := r.engine.Invocations(0).Invocations; len(calls) != 0 {
		t.Fatalf("an unsupported host reached the forge: %+v", calls)
	}

	status, err := r.core.Status(t.Context(), checkout(t, "git@github.com:acme/widgets.git"))
	if err != nil || status.Forge != "github" || status.ForgeUnsupported != "" || !strings.HasSuffix(status.OpenPRURL, "/acme/widgets/pull/2") {
		t.Fatalf("github.com status = %+v, %v", status, err)
	}
}
