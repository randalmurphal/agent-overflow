package main

import (
	"strings"
	"testing"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// Update branch and Resolve follow what GitHub says the viewer may do,
// through the app's forge code: write access does not reach a fork whose
// author turned maintainer edits off, which says why and whose update
// GitHub refuses, while a fork that allows them updates; a thread the
// viewer may not resolve says why, and GitHub refuses its resolve.
func TestGitHubViewerControlsFollowTheForge(t *testing.T) {
	no := false
	thread := func(id int64) []forgefake.Thread {
		return []forgefake.Thread{{Path: "a.go", Line: intPtr(1), Comments: []forgefake.Comment{{ID: id, Author: "octo", Body: "why?"}}}}
	}
	r := newRig(t, forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{{
		Forge: "github", Project: "acme/ctl",
		Pulls: []forgefake.Pull{
			{Number: 1, Title: "Locked", Author: "octo", HeadRepo: "octo/ctl", HeadRef: "locked", HeadSHA: strings.Repeat("1", 40),
				Readiness: &forgefake.Readiness{Behind: true}, Threads: thread(11), Viewer: &forgefake.ViewerCan{Resolve: &no}},
			{Number: 2, Title: "Open fork", Author: "octo", HeadRepo: "octo/ctl", MaintainerCanModify: true, HeadRef: "open", HeadSHA: strings.Repeat("2", 40),
				Readiness: &forgefake.Readiness{Behind: true}, Threads: thread(21)},
		},
	}}})
	ctx := forgeapi.WithInteractive(t.Context())
	ref := func(n int) gitops.PRReference {
		return gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "ctl", Number: n}
	}

	locked := mergeRead(t, r, ref(1))
	if m := locked.Detail.Merge; m.ViewerCanUpdateBranch || m.CannotUpdateBranchReason != "The author's fork does not allow edits from maintainers, so only the author can update this branch." {
		t.Fatalf("locked fork = %+v", m)
	}
	if err := r.core.UpdatePRBranch(ctx, ref(1), strings.Repeat("1", 40)); err == nil {
		t.Fatal("GitHub updated a fork that refuses maintainer edits")
	}
	if len(locked.Threads) != 1 || locked.Threads[0].CannotResolveReason == "" || locked.Threads[0].CannotReplyReason != "" {
		t.Fatalf("locked threads = %+v", locked.Threads)
	}
	if err := r.core.SetThreadResolved(ctx, ref(1), locked.Threads[0].ID, true); err == nil {
		t.Fatal("GitHub resolved a thread the viewer may not resolve")
	}

	open := mergeRead(t, r, ref(2))
	if m := open.Detail.Merge; !m.ViewerCanUpdateBranch || m.CannotUpdateBranchReason != "" {
		t.Fatalf("open fork = %+v", m)
	}
	if len(open.Threads) != 1 || open.Threads[0].CannotResolveReason != "" {
		t.Fatalf("open threads = %+v", open.Threads)
	}
	if err := r.core.UpdatePRBranch(ctx, ref(2), strings.Repeat("2", 40)); err != nil {
		t.Fatalf("UpdatePRBranch = %v", err)
	}
	if err := r.core.SetThreadResolved(ctx, ref(2), open.Threads[0].ID, true); err != nil {
		t.Fatalf("SetThreadResolved = %v", err)
	}
	// Resolved, the thread is reopened by the same right.
	if again := mergeRead(t, r, ref(2)); !again.Threads[0].IsResolved || again.Threads[0].CannotResolveReason != "" {
		t.Fatalf("resolved thread = %+v", again.Threads[0])
	}
}

// A GitLab reporter who did not open the merge request is told why they
// cannot resolve its threads; the author is not.
func TestGitLabThreadResolveFollowsTheRole(t *testing.T) {
	thread := func(id int64) []forgefake.Thread {
		return []forgefake.Thread{{Path: "a.go", Line: intPtr(1), Comments: []forgefake.Comment{{ID: id, Author: "octo", Body: "why?"}}}}
	}
	r := newRig(t, forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{{
		Forge: "gitlab", Project: "acme/ctl", Merge: &forgefake.RepoMerge{Permission: "reporter"},
		Pulls: []forgefake.Pull{
			{Number: 1, Title: "Theirs", Author: "octo", HeadRef: "theirs", HeadSHA: strings.Repeat("1", 40), Threads: thread(11)},
			{Number: 2, Title: "Mine", Author: "alice", HeadRef: "mine", HeadSHA: strings.Repeat("2", 40), Threads: thread(21)},
		},
	}}})
	ref := func(n int) gitops.PRReference {
		return gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "acme", Repo: "ctl", Number: n}
	}
	if threads := mergeRead(t, r, ref(1)).Threads; len(threads) != 1 || !threads[0].IsResolvable ||
		threads[0].CannotResolveReason != "Only the merge request's author and people with the Developer role or above can resolve threads." {
		t.Fatalf("a reporter's threads = %+v", threads)
	}
	if threads := mergeRead(t, r, ref(2)).Threads; len(threads) != 1 || threads[0].CannotResolveReason != "" {
		t.Fatalf("the author's threads = %+v", threads)
	}
}
