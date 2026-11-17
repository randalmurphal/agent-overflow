package main

import (
	"strings"
	"testing"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

func githubQueueFixture() forgefake.Fixture {
	return forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{{
		Forge: "github", Project: "acme/queue", ID: 5250,
		Merge: &forgefake.RepoMerge{MergeQueue: true, DefaultMethod: "squash", DeleteBranchOnMerge: false},
		Pulls: []forgefake.Pull{
			{Number: 1, Title: "Ready", Author: "octo", HeadRef: "ready", HeadSHA: strings.Repeat("1", 40)},
			{
				Number: 2, Title: "Checking", Author: "octo", HeadRef: "checking", HeadSHA: strings.Repeat("2", 40),
				Readiness: &forgefake.Readiness{RequiredChecks: []string{"unit"}},
				CI:        &forgefake.Pipeline{ID: 220, Jobs: []forgefake.Job{{ID: 221, Name: "unit", Status: "running"}}},
			},
			{Number: 3, Title: "Queued", Author: "octo", HeadRef: "queued", HeadSHA: strings.Repeat("3", 40),
				Queued: &forgefake.QueueEntry{Position: 1, By: "bob"}},
		},
	}, {
		Forge: "github", Project: "acme/triage", ID: 5251,
		Merge: &forgefake.RepoMerge{MergeQueue: true, Permission: "TRIAGE"},
		Pulls: []forgefake.Pull{{Number: 1, Title: "Queued", Author: "octo", HeadRef: "queued", HeadSHA: strings.Repeat("4", 40),
			Queued: &forgefake.QueueEntry{Position: 1}}},
	}}}
}

func countInvocations(r *rig, route string) int {
	n := 0
	for _, inv := range r.engine.Invocations(0).Invocations {
		if inv.Route == route {
			n++
		}
	}
	return n
}

// A base that requires GitHub's merge queue, through the forge-neutral
// MergeOptions.Queue: Merge joins the queue now or when ready with the
// queue's method, the queued pull reads its place and who added it,
// Cancel auto-merge takes it out, and nothing is merged around the queue.
func TestGitHubMergeQueueThroughTheAppsForgeCode(t *testing.T) {
	r := newRig(t, githubQueueFixture())
	ctx := forgeapi.WithInteractive(t.Context())
	ready := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "queue", Number: 1}
	checking, queued := ready, ready
	checking.Number, queued.Number = 2, 3
	readyHead := strings.Repeat("1", 40)

	read := mergeRead(t, r, ready)
	merge := read.Detail.Merge
	if q := merge.Queue; q == nil || !q.Required || q.Queued || len(merge.Methods) != 0 || merge.DeleteBranchOffered || !merge.AutoMergeAvailable || !merge.ViewerCanMerge || merge.AutoMerge != nil {
		t.Fatalf("queue base options = %+v (queue %+v)", merge, merge.Queue)
	}
	if read.Detail.Readiness.State != gitops.MergeStateReady {
		t.Fatalf("ready readiness = %+v", read.Detail.Readiness)
	}

	// An edited commit message is refused before any write.
	message := gitops.MergeMessage{Title: "Ship"}
	if _, err := r.core.MergePR(ctx, ready, gitops.MergeRequest{Method: "merge", HeadSHA: readyHead, CommitMessage: &message}); explained(t, ready, gitops.ActionMerge, err).Kind != gitops.ForgeErrorRefused || countInvocations(r, "gh graphql EnqueuePR") != 0 {
		t.Fatalf("a queue merge with a message = %v", err)
	}
	// Merge now joins the queue; no method or branch delete is sent.
	outcome, err := r.core.MergePR(ctx, ready, gitops.MergeRequest{HeadSHA: readyHead, DeleteBranch: true})
	if err != nil || !outcome.AutoMerge || outcome.Merged {
		t.Fatalf("merge on a queue base = %+v, %v", outcome, err)
	}
	if vars := decodeRaw(t, lastInvocation(t, r, "gh graphql EnqueuePR").Variables); vars["expectedHeadOid"] != readyHead || len(vars) != 2 {
		t.Fatalf("EnqueuePR variables = %v", vars)
	}
	read = mergeRead(t, r, ready)
	q, armed := read.Detail.Merge.Queue, read.Detail.Merge.AutoMerge
	if read.Detail.State != "open" || q == nil || !q.Queued || q.Position != 2 || q.Size != 2 || !q.CanRemove || armed == nil || armed.ByLogin != "alice" || armed.At == "" || !armed.CanCancel {
		t.Fatalf("after joining: state %q, queue %+v, armed %+v", read.Detail.State, q, armed)
	}
	// A second merge is answered as armed without another request.
	if outcome, err := r.core.MergePR(ctx, ready, gitops.MergeRequest{HeadSHA: readyHead}); err != nil || !outcome.AutoMerge {
		t.Fatalf("merge of a queued pull = %+v, %v", outcome, err)
	}
	if n := countInvocations(r, "gh graphql EnqueuePR"); n != 1 {
		t.Fatalf("EnqueuePR sent %d times", n)
	}

	// Cancel auto-merge on a queued pull takes it out of the queue.
	if err := r.core.CancelPRAutoMerge(ctx, ready); err != nil {
		t.Fatalf("CancelPRAutoMerge of a queued pull: %v", err)
	}
	if got := mergeRead(t, r, ready).Detail.Merge; got.Queue == nil || got.Queue.Queued || got.AutoMerge != nil || countInvocations(r, "gh graphql DequeuePR") != 1 {
		t.Fatalf("after removal: %+v (queue %+v)", got, got.Queue)
	}
	// Join when ready on a pull that could merge now joins at once.
	if outcome, err := r.core.MergePR(ctx, ready, gitops.MergeRequest{HeadSHA: readyHead, AutoMerge: true}); err != nil || !outcome.AutoMerge {
		t.Fatalf("merge when ready of a ready pull = %+v, %v", outcome, err)
	}
	if got := mergeRead(t, r, ready).Detail.Merge.Queue; got == nil || !got.Queued {
		t.Fatalf("after joining when ready: %+v", got)
	}

	// A pull whose checks run joins when ready: auto-merge with no method.
	if _, err := r.core.MergePR(ctx, checking, gitops.MergeRequest{HeadSHA: strings.Repeat("2", 40)}); explained(t, checking, gitops.ActionMerge, err).Kind != gitops.ForgeErrorRefused {
		t.Fatalf("joining now while checks run = %v", err)
	}
	outcome, err = r.core.MergePR(ctx, checking, gitops.MergeRequest{HeadSHA: strings.Repeat("2", 40), AutoMerge: true})
	if err != nil || !outcome.AutoMerge {
		t.Fatalf("merge when ready = %+v, %v", outcome, err)
	}
	if vars := decodeRaw(t, lastInvocation(t, r, "gh graphql EnableQueuedAutoMerge").Variables); vars["method"] != nil || len(vars) != 2 {
		t.Fatalf("EnableQueuedAutoMerge variables = %v", vars)
	}
	if got := mergeRead(t, r, checking).Detail.Merge; got.AutoMerge == nil || !got.AutoMerge.CanCancel || got.Queue == nil || got.Queue.Queued {
		t.Fatalf("armed on a queue base = %+v", got)
	}
	// Cancel on an armed pull not yet queued disarms it.
	if err := r.core.CancelPRAutoMerge(ctx, checking); err != nil || countInvocations(r, "gh graphql DequeuePR") != 1 {
		t.Fatalf("cancel of an armed pull = %v", err)
	}
	if got := mergeRead(t, r, checking).Detail.Merge; got.AutoMerge != nil {
		t.Fatalf("after cancel: %+v", got)
	}

	// A pull already queued reads its place and who queued it.
	read = mergeRead(t, r, queued)
	if got := read.Detail.Merge; got.Queue == nil || !got.Queue.Queued || got.Queue.Position != 1 || got.Queue.Size != 2 || got.AutoMerge == nil || got.AutoMerge.ByLogin != "bob" {
		t.Fatalf("seeded entry = %+v (queue %+v)", got, got.Queue)
	}
	// A viewer who may not merge is not offered the removal.
	triage := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "triage", Number: 1}
	if got := mergeRead(t, r, triage).Detail.Merge; got.Queue == nil || !got.Queue.Queued || got.Queue.CanRemove || got.AutoMerge == nil || got.AutoMerge.CanCancel {
		t.Fatalf("triage options = %+v (queue %+v)", got, got.Queue)
	}

	for _, route := range []string{"gh graphql MergePR", "gh graphql MergePRMessage", "gh graphql EnableAutoMerge", "gh graphql EnableAutoMergeMessage"} {
		if n := countInvocations(r, route); n != 0 {
			t.Errorf("%s sent %d times on a queue base", route, n)
		}
	}
	for _, inv := range r.engine.Invocations(0).Invocations {
		if inv.Unhandled {
			t.Errorf("unhandled %s %s: %s", inv.Method, inv.Path, inv.Detail)
		}
	}
}
