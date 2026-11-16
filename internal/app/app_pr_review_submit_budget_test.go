package app

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

// reviewSubmitRun is a submit's run context whose budget end ends, with
// the cause withinMethodBudgetFinishing gives SubmitPRReview's. The test
// ends it while the forge holds a request, so the request is provably in
// flight and every later one provably not started.
func reviewSubmitRun(t *testing.T) (run context.Context, end func()) {
	t.Helper()
	run, stop := testutil.DeadlineContext(context.Background())
	t.Cleanup(func() { stop(nil) })
	return run, func() { stop(budgetExceeded{doing: "Posting the review", budget: 8 * time.Minute}) }
}

// A GitHub review whose budget ends while the forge holds it posts, and
// the file comment after it is not sent: the answer says the review
// posted and why the comment did not, and the comment stays a draft.
func TestSubmitPRReviewStoppedByItsBudgetSaysWhatPosted(t *testing.T) {
	t.Parallel()
	c := newReviewSubmitCase(t, "github")
	line := c.draft(t, true, 3, "Guard this.")
	file, err := c.app.CreateDiffReviewComment(c.thread.ID, store.DiffReviewCommentInput{Scope: string(store.DiffReviewScopePR), SourceKey: prDraftSource(c.pr), FilePath: "main.go", Side: "file", Body: "Split this file."})
	if err != nil {
		t.Fatal(err)
	}
	run, end := reviewSubmitRun(t)
	entered, release := c.gate.hold()
	defer release()
	type answer struct {
		result SubmitPRReviewResult
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		result, err := c.app.submitPRReview(run, c.pr, c.request(true, line, file))
		done <- answer{result, err}
	}()
	<-entered
	end()
	release()
	got := <-done
	const want = "Posting the review took longer than 8m0s, so it was stopped before the rest was sent."
	if got.err != nil || !got.result.Posted || !got.result.PostedReview || got.result.PostedFileComments != 0 || got.result.PartialFailurePath != "main.go" ||
		got.result.PartialFailure != want || got.result.PartialFailureKind != methodBudgetExceededCode {
		t.Fatalf("submit = %+v, %v; want the review posted and the file comment stopped by the budget", got.result, got.err)
	}
	if c.comment(t, true, line.ID).SentAt == 0 || c.comment(t, true, file.ID).SentAt != 0 {
		t.Fatal("want the line comment marked sent and the file comment still a draft")
	}
}

// A GitLab review whose budget ends while its first draft note is being
// created posts nothing: the submit creates no other, removes that one,
// and says the budget stopped it. The drafts stay drafts.
func TestSubmitPRReviewStoppedByItsBudgetBeforeThePublishPostsNothing(t *testing.T) {
	t.Parallel()
	c := newReviewSubmitCase(t, "gitlab")
	c.gate.publish = "/draft_notes"
	first, second := c.draft(t, false, 3, "Guard this."), c.draft(t, false, 5, "And this.")
	run, end := reviewSubmitRun(t)
	entered, release := c.gate.hold()
	defer release()
	done := make(chan error, 1)
	go func() {
		_, err := c.app.submitPRReview(run, c.pr, c.request(false, first, second))
		done <- err
	}()
	<-entered
	end()
	release()
	err := <-done
	if code, message, _ := errorsx.PublicDetails(err); code != methodBudgetExceededCode || message != "Posting the review took longer than 8m0s, so it was stopped." {
		t.Fatalf("submit = %v (code %q, %q); want the budget's stop", err, code, message)
	}
	for _, inv := range c.engine.Invocations(0).Invocations {
		if inv.Route == publishRoute["gitlab"] {
			t.Fatal("the review was published past its budget")
		}
	}
	if c.comment(t, false, first.ID).SentAt != 0 || c.comment(t, false, second.ID).SentAt != 0 {
		t.Fatal("a draft was marked sent")
	}
}

// A submit runs to its outcome whatever its caller does: one whose caller
// already left still posts, which is what its outcome then reports.
func TestSubmitPRReviewRunsWhenItsCallerLeaves(t *testing.T) {
	t.Parallel()
	c := newReviewSubmitCase(t, "github")
	draft := c.draft(t, true, 3, "Guard this.")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := c.app.SubmitPRReview(ctx, c.pr, c.request(true, draft))
	if err != nil || !result.Posted || c.published() != 1 {
		t.Fatalf("submit after its caller left = %+v, %v; want it posted", result, err)
	}
}

// SubmitPRReview's run stops starting forge requests reviewSubmitFinish
// before its budget, so what runs past that point still answers in it.
func TestSubmitPRReviewRunEndsBeforeItsBudget(t *testing.T) {
	t.Parallel()
	run, cancel := withinMethodBudgetFinishing(context.Background(), "SubmitPRReview", "Posting the review", reviewSubmitFinish)
	defer cancel()
	deadline, ok := run.Deadline()
	if !ok {
		t.Fatal("the run has no deadline")
	}
	if left := time.Until(deadline); left > 10*time.Minute-reviewSubmitFinish || left < 10*time.Minute-reviewSubmitFinish-time.Minute {
		t.Fatalf("the run ends in %s, want %s before the 10m budget", left, reviewSubmitFinish)
	}
}
