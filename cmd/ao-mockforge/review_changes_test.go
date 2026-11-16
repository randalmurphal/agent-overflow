package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// changesFixture is gitlabManageFixture with alice, the viewer, among the
// reviewers, on a host running version.
func changesFixture(version string, noChangeRequests bool) forgefake.Fixture {
	fixture := gitlabManageFixture()
	fixture.Repos[0].ServerVersion = version
	fixture.Repos[0].NoChangeRequests = noChangeRequests
	fixture.Repos[0].Pulls[0].Reviewers = append(fixture.Repos[0].Pulls[0].Reviewers, "alice")
	return fixture
}

// viewerState is alice's reviewer state in the Details read.
func viewerState(t *testing.T, read gitops.PRRead) string {
	t.Helper()
	for _, reviewer := range read.Detail.Manage.Reviewers {
		if reviewer.Login == "alice" {
			return reviewer.State
		}
	}
	t.Fatalf("alice is not a reviewer: %+v", read.Detail.Manage.Reviewers)
	return ""
}

// changesThread is the merge request's changes-requested thread.
func changesThread(read gitops.PRRead, summary string) (gitops.ReviewThread, bool) {
	for _, thread := range read.Threads {
		if thread.Path == "" && len(thread.Comments) == 1 && thread.Comments[0].Body == "Changes requested:\n\n"+summary {
			return thread, true
		}
	}
	return gitops.ReviewThread{}, false
}

// A GitLab Request changes posts its summary as a thread to resolve on
// every release; from 19.3 the publish also sets the viewer's reviewer
// state, which the Details read shows. Before 19.3 nothing sends
// reviewer_state (the fake refuses it there), and the viewer reads as
// reviewed, by the thread. A tier without Request changes records
// reviewed, and the read says so.
func TestGitLabRequestChangesThroughTheAppsForgeCode(t *testing.T) {
	for _, tc := range []struct {
		name, version string
		noChanges     bool
		publishes     int
		state         string
	}{
		{"19.6", "19.6.0-ee", false, 1, gitops.ReviewerChangesRequested},
		{"18.5", "18.5.2", false, 0, gitops.ReviewerCommented},
		{"19.6 without Request changes", "19.6.0-ee", true, 1, gitops.ReviewerCommented},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, changesFixture(tc.version, tc.noChanges))
			ref := gitlabManageRef()
			ctx := forgeapi.WithInteractive(t.Context())
			result, err := r.core.SubmitReview(ctx, ref, gitops.SubmitReviewRequest{Verdict: gitops.ReviewVerdictRequestChanges, Body: "Rename it.", HeadSHA: strings.Repeat("2", 40)})
			if err != nil || !result.PostedReview {
				t.Fatalf("submit = %+v, %v", result, err)
			}
			if n := routeCount(r, 0, "glab api publish draft notes"); n != tc.publishes {
				t.Fatalf("%d publishes, want %d", n, tc.publishes)
			}
			for _, inv := range r.engine.Invocations(0).Invocations {
				if inv.Unhandled {
					t.Fatalf("unhandled %s %s", inv.Method, inv.Path)
				}
			}
			read := mergeRead(t, r, ref)
			thread, ok := changesThread(read, "Rename it.")
			if !ok || !thread.IsResolvable || thread.IsResolved {
				t.Fatalf("no open changes-requested thread to resolve: %+v", read.Threads)
			}
			if got := viewerState(t, read); got != tc.state {
				t.Fatalf("alice's state = %q, want %q", got, tc.state)
			}
		})
	}
}

// Request changes without a summary is refused before any request, on
// both forges, with one sentence.
func TestRequestChangesNeedsASummary(t *testing.T) {
	r := newRig(t, changesFixture("19.6.0-ee", false))
	for _, ref := range []gitops.PRReference{gitlabManageRef(), {Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widgets", Number: 2}} {
		before := len(r.engine.Invocations(0).Invocations)
		_, err := r.core.SubmitReview(forgeapi.WithInteractive(t.Context()), ref, gitops.SubmitReviewRequest{Verdict: gitops.ReviewVerdictRequestChanges, Body: "  ", HeadSHA: strings.Repeat("2", 40)})
		if got := explained(t, ref, gitops.ActionReview, err); got.Kind != gitops.ForgeErrorRefused || got.Message != gitops.ReviewChangesNeedSummary {
			t.Fatalf("%s: %+v", ref.Forge, got)
		}
		if sent := r.engine.Invocations(before).Invocations; len(sent) != 0 {
			t.Fatalf("%s: a refused submit sent %+v", ref.Forge, sent)
		}
	}
}

// A thread GitLab refuses after the publish set the state says what did
// apply: the review (its state) posted, the thread did not.
func TestGitLabRequestChangesThreadRefusedAfterThePublish(t *testing.T) {
	r := newRig(t, changesFixture("19.6.0-ee", false))
	ref := gitlabManageRef()
	if err := r.engine.SetRefusal(forgefake.Refusal{Route: "glab api create discussion", Status: http.StatusUnprocessableEntity, Times: 1, Body: []byte(`{"message":"refused"}`)}); err != nil {
		t.Fatal(err)
	}
	_, err := r.core.SubmitReview(forgeapi.WithInteractive(t.Context()), ref, gitops.SubmitReviewRequest{Verdict: gitops.ReviewVerdictRequestChanges, Body: "Rename it.", HeadSHA: strings.Repeat("2", 40)})
	partial, ok := errors.AsType[*gitops.PartialSubmitError](err)
	if !ok || !partial.PostedReview || partial.Verdict != gitops.SubmitStepChangesThread || partial.Unconfirmed {
		t.Fatalf("submit = %v (%+v)", err, partial)
	}
	read := mergeRead(t, r, ref)
	if _, posted := changesThread(read, "Rename it."); posted {
		t.Fatal("the refused thread is on the merge request")
	}
	if got := viewerState(t, read); got != gitops.ReviewerChangesRequested {
		t.Fatalf("alice's state = %q, want the publish's", got)
	}
}
