package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// newChangesCase is a GitLab review case on a host running version whose
// merge request has alice, the viewer, among its reviewers.
func newChangesCase(t *testing.T, version string, noChangeRequests bool) *reviewSubmitCase {
	t.Helper()
	c := newReviewSubmitCase(t, "gitlab")
	pull := forgefake.Pull{Number: 9, Title: "Widgets", Author: "octo", HeadRef: "feature", HeadSHA: reviewTestHead, State: "open", Diff: reviewTestDiff, Reviewers: []string{"alice"}}
	if _, err := c.engine.Seed(forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{{Forge: "gitlab", Project: "acme/widgets", ServerVersion: version, NoChangeRequests: noChangeRequests, Pulls: []forgefake.Pull{pull}}}}); err != nil {
		t.Fatal(err)
	}
	return c
}

func viewerReviewState(detail gitops.PRDetail) string {
	for _, reviewer := range detail.Manage.Reviewers {
		if reviewer.Login == "alice" {
			return reviewer.State
		}
	}
	return ""
}

// A Request changes refreshes the pane's PR at once, and the frame carries
// the reviewer state the forge recorded: changes requested where GitLab
// took it, reviewed on a tier that downgraded it, so the pane never shows
// a verdict the forge did not record.
func TestSubmitPRReviewRequestChangesShowsTheStateTheForgeRecorded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		noChanges bool
		want      string
	}{
		{"recorded", false, gitops.ReviewerChangesRequested},
		{"downgraded", true, gitops.ReviewerCommented},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newChangesCase(t, "19.6.0-ee", tc.noChanges)
			stubPRCIFetch(c.app)
			c.app.prUpdates.interval = time.Hour
			events := capturePRUpdates(t, c.app)
			sub, err := c.app.SubscribePRUpdates(connCtx(t), c.pr)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = c.app.UnsubscribePRUpdates(context.Background(), sub.ID)
				c.app.prUpdates.wg.Wait()
			})
			if got := viewerReviewState(sub.Detail); got == gitops.ReviewerChangesRequested {
				t.Fatalf("before the submit alice reads %q", got)
			}
			result, err := c.submit(SubmitPRReviewRequest{Verdict: gitops.ReviewVerdictRequestChanges, Body: "Rename it.", HeadSHA: reviewTestHead})
			if err != nil || !result.PostedReview || result.PartialFailure != "" {
				t.Fatalf("submit = %+v, %v", result, err)
			}
			frame := awaitPRUpdate(t, events, "the refresh after the submit")
			if got := viewerReviewState(frame.Detail); got != tc.want {
				t.Fatalf("alice after the submit = %q, want %q", got, tc.want)
			}
			found := false
			for _, thread := range frame.Threads {
				found = found || (thread.Path == "" && thread.IsResolvable && len(thread.Comments) == 1 && thread.Comments[0].Body == "Changes requested:\n\nRename it.")
			}
			if !found {
				t.Fatalf("no changes-requested thread to resolve in the frame: %+v", frame.Threads)
			}
		})
	}
}

// A thread GitLab refuses after the publish set the state is a posted
// review whose sentence says the thread did not post and the summary is
// kept.
func TestSubmitPRReviewRequestChangesThreadRefusedSaysSo(t *testing.T) {
	t.Parallel()
	c := newChangesCase(t, "19.6.0-ee", false)
	if err := c.engine.SetRefusal(forgefake.Refusal{Route: "glab api create discussion", Status: http.StatusUnprocessableEntity, Times: 1, Body: []byte(`{"message":"refused"}`)}); err != nil {
		t.Fatal(err)
	}
	result, err := c.submit(SubmitPRReviewRequest{Verdict: gitops.ReviewVerdictRequestChanges, Body: "Rename it.", HeadSHA: reviewTestHead})
	if err != nil || !result.PostedReview || !strings.HasPrefix(result.PartialFailure, "Its changes-requested thread did not post, so the summary is kept to send again.") {
		t.Fatalf("submit = %+v, %v", result, err)
	}
}
