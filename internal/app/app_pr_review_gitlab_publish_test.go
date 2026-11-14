package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
	"agent-overflow/internal/store"
)

// gitlabPublishCase is a GitLab review case whose pull has one open
// discussion of the viewer's, and drafts of one family: a file comment, a
// line comment and a reply into that discussion.
type gitlabPublishCase struct {
	*reviewSubmitCase
	threadOwned       bool
	discussion        string
	file, line, reply store.DiffReviewComment
}

func newGitLabPublishCase(t *testing.T, threadOwned bool) *gitlabPublishCase {
	t.Helper()
	c := &gitlabPublishCase{reviewSubmitCase: newReviewSubmitCase(t, "gitlab"), threadOwned: threadOwned, discussion: strings.Repeat("b", 40)}
	line := 2
	pull := forgefake.Pull{Number: 9, Title: "Widgets", Author: "octo", HeadRef: "feature", State: "open", Diff: reviewTestDiff,
		Threads: []forgefake.Thread{{ID: c.discussion, Path: "main.go", Line: &line, Side: "right", Comments: []forgefake.Comment{{ID: 502, Author: "alice", Body: "open root"}}}}}
	if _, err := c.engine.Seed(forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{{Forge: "gitlab", Project: "acme/widgets", Pulls: []forgefake.Pull{pull}}}}); err != nil {
		t.Fatal(err)
	}
	var err error
	if c.file, err = c.createDraft(threadOwned, store.DiffReviewCommentInput{FilePath: "main.go", Side: "file", Body: "on the file"}); err != nil {
		t.Fatal(err)
	}
	c.line = c.draft(t, threadOwned, 3, "on a line")
	c.reply = c.replyDraft(t, threadOwned, threadWithBody(c.threads(t), "open root"), "a reply")
	return c
}

// submitDroppingTheReply submits review while GitLab's publish is held,
// deleting the reply's discussion meanwhile and running during() before
// the publish goes on.
func (c *gitlabPublishCase) submitDroppingTheReply(t *testing.T, review SubmitPRReviewRequest, during func()) SubmitPRReviewResult {
	t.Helper()
	entered, release := c.gate.hold()
	defer release()
	type answer struct {
		result SubmitPRReviewResult
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		result, err := c.submit(review)
		done <- answer{result, err}
	}()
	<-entered
	deleteNote := httptest.NewRequest(http.MethodDelete, "/gitlab/api/v4/projects/acme%2Fwidgets/merge_requests/9/discussions/"+c.discussion+"/notes/502", nil)
	deleteNote.Host = "gitlab.com"
	deleteNote.Header.Set("Private-Token", mergeForgeToken)
	recorder := httptest.NewRecorder()
	c.engine.ServeHTTP(recorder, deleteNote)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete the discussion = %d %s", recorder.Code, recorder.Body)
	}
	if during != nil {
		during()
	}
	release()
	got := <-done
	if got.err != nil {
		t.Fatalf("submit = %v", got.err)
	}
	return got.result
}

// expectSettledByThePublish checks that the file and line comments, which
// GitLab published, are sent and the reply it dropped stays a draft.
func (c *gitlabPublishCase) expectSettledByThePublish(t *testing.T, result SubmitPRReviewResult) {
	t.Helper()
	sent := slices.Clone(result.SentDraftIDs)
	slices.Sort(sent)
	want := []string{c.file.ID, c.line.ID}
	slices.Sort(want)
	if !slices.Equal(sent, want) {
		t.Fatalf("sent %v, want the file and line comments %v (result %+v)", result.SentDraftIDs, want, result)
	}
	for id, status := range map[string]string{c.file.ID: "sent", c.line.ID: "sent", c.reply.ID: "draft"} {
		if got := c.comment(t, c.threadOwned, id).Status; got != status {
			t.Fatalf("draft %s is %q, want %q", id, got, status)
		}
	}
	bodies := c.postedBodies(t)
	if n := slices.Index(bodies, "on the file"); n < 0 || slices.Contains(bodies[n+1:], "on the file") {
		t.Fatalf("posted bodies = %q, want the file comment once", bodies)
	}
}

// A publish that drops a held reply still published the file and line
// comments beside it: on a partial result each draft settles by what the
// publish did, for a thread's drafts and a PR's.
func TestSubmitPRReviewGitLabDroppedReplySettlesTheRestByThePublish(t *testing.T) {
	t.Parallel()
	for _, threadOwned := range []bool{false, true} {
		c := newGitLabPublishCase(t, threadOwned)
		result := c.submitDroppingTheReply(t, c.request(threadOwned, c.file, c.line, c.reply), nil)
		if !result.PostedReview || result.PartialFailureThreadID != c.discussion || result.PartialFailureKind != gitops.ForgeErrorNotFound {
			t.Fatalf("thread=%v: submit = %+v", threadOwned, result)
		}
		c.expectSettledByThePublish(t, result)
	}
}

// Replies whose confirming read failed and an approval GitLab refused are
// two outcomes, and the answer, and the attempt's recorded outcome, carry
// both: the replies' uncertainty with their thread, and the refusal. The
// drafts settle by the publish, not by the approval.
func TestSubmitPRReviewGitLabRefusedApprovalKeepsTheRepliesUncertainty(t *testing.T) {
	t.Parallel()
	for _, threadOwned := range []bool{false, true} {
		c := newGitLabPublishCase(t, threadOwned)
		review := c.request(threadOwned, c.file, c.line, c.reply)
		review.Verdict = gitops.ReviewVerdictApprove
		review.AttemptID = "approve-refused"
		result := c.submitDroppingTheReply(t, review, func() {
			for _, refusal := range []forgefake.Refusal{
				{Route: "glab api discussions", Status: http.StatusUnprocessableEntity, Times: 1, Body: []byte(`{"message":"refused"}`)},
				{Route: "glab api approve", Status: http.StatusConflict, Times: 1, Body: []byte(`{"message":"SHA does not match HEAD of source branch"}`)},
			} {
				if err := c.engine.SetRefusal(refusal); err != nil {
					t.Fatal(err)
				}
			}
		})
		check := func(what string, result SubmitPRReviewResult) {
			t.Helper()
			if !result.PostedReview || result.PartialFailureThreadID != c.discussion || result.PartialFailureKind != "unconfirmed" ||
				!strings.Contains(result.PartialFailure, "may already be on the PR") || !strings.Contains(result.PartialFailure, "The approval did not go through") {
				t.Fatalf("thread=%v: %s = %+v", threadOwned, what, result)
			}
		}
		check("submit", result)
		outcome, err := c.app.GetPRReviewSubmitOutcome(context.Background(), c.pr, "approve-refused")
		if err != nil || outcome.Result == nil {
			t.Fatalf("thread=%v: outcome = %+v, %v", threadOwned, outcome, err)
		}
		check("outcome", *outcome.Result)
		c.expectSettledByThePublish(t, result)
	}
}
