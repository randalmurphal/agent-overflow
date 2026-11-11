package app

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
	"agent-overflow/internal/store"
)

var reviewSubmitPR = gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "acme", Repo: "widgets", Number: 9}

const reviewSubmitSource = "pr:gitlab:acme/widgets#9"

// reviewSubmitCase is a GitLab merge request on the fake forge, with a
// thread whose drafts and the PR's own drafts the submits post.
type reviewSubmitCase struct {
	app    *App
	engine *forgefake.Engine
	thread store.Thread
}

func newReviewSubmitCase(t *testing.T) *reviewSubmitCase {
	t.Helper()
	app, _ := newTestAppWithStorePath(t)
	engine := forgefake.New(forgefake.Options{APIToken: mergeForgeToken})
	if _, err := engine.Seed(forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "gitlab", Project: "acme/widgets", Pulls: []forgefake.Pull{
		{Number: 9, Title: "Widgets", HeadRef: "feature", State: "open"},
	}}}}); err != nil {
		t.Fatal(err)
	}
	useForgefake(t, app, engine)
	thread := testThread("review-submit-thread")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	return &reviewSubmitCase{app: app, engine: engine, thread: thread}
}

// draft writes one pr-scope draft, the thread's or (threadID "") the PR's.
func (c *reviewSubmitCase) draft(t *testing.T, threadID string, line int, body string) store.DiffReviewComment {
	t.Helper()
	input := store.DiffReviewCommentInput{Scope: string(store.DiffReviewScopePR), SourceKey: reviewSubmitSource, FilePath: "main.go", NewLine: line, Side: "new", Body: body}
	if threadID == "" {
		created, err := c.app.CreatePRDiffReviewComment(reviewSubmitPR, input)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	created, err := c.app.CreateDiffReviewComment(threadID, input)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func (c *reviewSubmitCase) input(submitID, threadID string, drafts ...store.DiffReviewComment) SubmitPRReviewInput {
	ids := make([]string, 0, len(drafts))
	for _, draft := range drafts {
		ids = append(ids, draft.ID)
	}
	return SubmitPRReviewInput{SubmitID: submitID, Verdict: gitops.ReviewVerdictComment, Drafts: SubmitPRReviewDrafts{
		ThreadID: threadID, Scope: string(store.DiffReviewScopePR), SourceKey: reviewSubmitSource, CommentIDs: ids, HeadSHA: "head",
	}}
}

// published counts the review publishes the forge answered.
func (c *reviewSubmitCase) published() int {
	n := 0
	for _, inv := range c.engine.Invocations(0).Invocations {
		if inv.Route == "glab api publish draft notes" && inv.Status < 300 {
			n++
		}
	}
	return n
}

func (c *reviewSubmitCase) status(t *testing.T, owner store.DiffReviewOwner, id string) store.DiffReviewComment {
	t.Helper()
	got, err := c.app.store.GetDiffReviewComment(owner, id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The answer of a submit the forge accepted is lost; the same submit again
// posts nothing and answers the first one's outcome, and its drafts were
// marked sent by the first: a thread's and a PR's.
func TestSubmitPRReviewRetriedAfterALostAnswerPostsOnce(t *testing.T) {
	t.Parallel()
	for _, prOwned := range []bool{false, true} {
		c := newReviewSubmitCase(t)
		threadID, owner := c.thread.ID, store.ThreadReviewOwner(c.thread.ID)
		if prOwned {
			threadID, owner = "", store.PRReviewOwner(reviewSubmitPR.Key())
		}
		draft := c.draft(t, threadID, 3, "Guard this.")
		input := c.input("submit-a", threadID, draft)
		first, err := c.app.SubmitPRReview(context.Background(), reviewSubmitPR, input)
		if err != nil || !first.PostedReview {
			t.Fatalf("prOwned=%v: first = %+v, %v", prOwned, first, err)
		}
		// The answer never reached the pane: it submits again.
		again, err := c.app.SubmitPRReview(context.Background(), reviewSubmitPR, input)
		if err != nil || again != first {
			t.Fatalf("prOwned=%v: retry = %+v, %v; want %+v", prOwned, again, err, first)
		}
		if n := c.published(); n != 1 {
			t.Fatalf("prOwned=%v: published %d reviews, want 1", prOwned, n)
		}
		got := c.status(t, owner, draft.ID)
		wantTurn := "pr:head"
		if prOwned {
			wantTurn = ""
		}
		if got.Status != "sent" || got.SentTurnID != wantTurn {
			t.Fatalf("prOwned=%v: draft = %+v, want sent on %q", prOwned, got, wantTurn)
		}
	}
}

// Attempts of one submit that overlap post once: the second waits for the
// first's record.
func TestSubmitPRReviewConcurrentRepeatsPostOnce(t *testing.T) {
	t.Parallel()
	c := newReviewSubmitCase(t)
	draft := c.draft(t, "", 3, "Guard this.")
	input := c.input("submit-overlap", "", draft)
	var wg sync.WaitGroup
	results := make([]SubmitPRReviewResult, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Go(func() { results[i], errs[i] = c.app.SubmitPRReview(context.Background(), reviewSubmitPR, input) })
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || !results[i].PostedReview {
			t.Fatalf("attempt %d = %+v, %v", i, results[i], errs[i])
		}
	}
	if n := c.published(); n != 1 {
		t.Fatalf("published %d reviews, want 1", n)
	}
}

// A submit the forge refused before it accepted the review records
// nothing and leaves the drafts unsent; its retry posts once.
func TestSubmitPRReviewRefusedThenRetriedPostsOnce(t *testing.T) {
	t.Parallel()
	c := newReviewSubmitCase(t)
	draft := c.draft(t, c.thread.ID, 3, "Guard this.")
	input := c.input("submit-refused", c.thread.ID, draft)
	if err := c.engine.SetRefusal(forgefake.Refusal{Route: "glab api publish draft notes", Status: http.StatusForbidden, Times: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.app.SubmitPRReview(context.Background(), reviewSubmitPR, input); err == nil {
		t.Fatal("a refused submit succeeded")
	}
	if got := c.status(t, store.ThreadReviewOwner(c.thread.ID), draft.ID); got.Status != "draft" {
		t.Fatalf("after the refusal the draft is %+v", got)
	}
	if _, ok, err := c.app.store.GetPRReviewSubmit("submit-refused"); err != nil || ok {
		t.Fatalf("a refused submit was recorded: %v, %v", ok, err)
	}
	result, err := c.app.SubmitPRReview(context.Background(), reviewSubmitPR, input)
	if err != nil || !result.PostedReview {
		t.Fatalf("retry = %+v, %v", result, err)
	}
	if n := c.published(); n != 1 {
		t.Fatalf("published %d reviews, want 1", n)
	}
	if got := c.status(t, store.ThreadReviewOwner(c.thread.ID), draft.ID); got.Status != "sent" {
		t.Fatalf("after the retry the draft is %+v", got)
	}
}

// Two submits are two reviews: each posts and marks its own drafts, and a
// repeat of the first answers the first without touching the second's.
// An id is refused for another PR or owner, and a submit of drafts that
// are not drafts any more posts nothing.
func TestSubmitPRReviewKeepsSubmitsApart(t *testing.T) {
	t.Parallel()
	c := newReviewSubmitCase(t)
	a := c.draft(t, "", 3, "First.")
	first, err := c.app.SubmitPRReview(context.Background(), reviewSubmitPR, c.input("submit-1", "", a))
	if err != nil {
		t.Fatal(err)
	}
	b := c.draft(t, "", 4, "Second.")
	if _, err := c.app.SubmitPRReview(context.Background(), reviewSubmitPR, c.input("submit-2", "", b)); err != nil {
		t.Fatal(err)
	}
	if n := c.published(); n != 2 {
		t.Fatalf("published %d reviews, want 2", n)
	}
	owner := store.PRReviewOwner(reviewSubmitPR.Key())
	if c.status(t, owner, a.ID).Status != "sent" || c.status(t, owner, b.ID).Status != "sent" {
		t.Fatal("each submit marks its own drafts sent")
	}
	if again, err := c.app.SubmitPRReview(context.Background(), reviewSubmitPR, c.input("submit-1", "", a)); err != nil || again != first {
		t.Fatalf("repeat of the first = %+v, %v", again, err)
	}
	if n := c.published(); n != 2 {
		t.Fatalf("a repeat posted: %d reviews", n)
	}
	other := reviewSubmitPR
	other.Number = 10
	if _, err := c.app.SubmitPRReview(context.Background(), other, c.input("submit-1", "", a)); err == nil {
		t.Fatal("a submit id answered for another PR")
	}
	if _, err := c.app.SubmitPRReview(context.Background(), reviewSubmitPR, c.input("submit-1", c.thread.ID, a)); err == nil {
		t.Fatal("a submit id answered for another owner")
	}
	// A new submit of drafts already sent is refused before the forge.
	if _, err := c.app.SubmitPRReview(context.Background(), reviewSubmitPR, c.input("submit-3", "", a)); err == nil {
		t.Fatal("drafts already sent were posted again")
	}
	if n := c.published(); n != 2 {
		t.Fatalf("a refused submit posted: %d reviews", n)
	}
	if _, err := c.app.SubmitPRReview(context.Background(), reviewSubmitPR, SubmitPRReviewInput{Verdict: gitops.ReviewVerdictComment}); err == nil {
		t.Fatal("a submit with no id was accepted")
	}
}

// A draft posts as a file comment or a line on its side; one with no line
// to anchor posts nothing.
func TestReviewLineCommentForDraft(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		draft store.DiffReviewComment
		ok    bool
		side  string
		line  int
	}{
		{draft: store.DiffReviewComment{Side: "file"}, ok: true, side: "file"},
		{draft: store.DiffReviewComment{Side: "old", OldLine: 4, NewLine: 9}, ok: true, side: "left", line: 4},
		{draft: store.DiffReviewComment{Side: "new", OldLine: 9, NewLine: 5}, ok: true, side: "right", line: 5},
		{draft: store.DiffReviewComment{Side: "context", OldLine: 9, NewLine: 6}, ok: true, side: "right", line: 6},
		{draft: store.DiffReviewComment{Side: "old", NewLine: 5}},
		{draft: store.DiffReviewComment{Side: "new", OldLine: 5}},
	} {
		tc.draft.FilePath, tc.draft.Body = "main.go", "b"
		got, ok := reviewLineCommentForDraft(tc.draft)
		if ok != tc.ok {
			t.Fatalf("%+v: ok = %v", tc.draft, ok)
		}
		if !ok {
			continue
		}
		gotLine := 0
		if got.Line != nil {
			gotLine = *got.Line
		}
		if got.Side != tc.side || gotLine != tc.line || got.Path != "main.go" || got.Body != "b" {
			t.Fatalf("%+v: comment = %+v (line %d)", tc.draft, got, gotLine)
		}
	}
}

// A review that landed carried every line comment; of its file comments,
// a partial failure posted only the first PostedFileComments.
func TestPostedDraftIDsStopAtTheFailedFileComment(t *testing.T) {
	t.Parallel()
	drafts := []store.DiffReviewComment{{ID: "l1", Side: "new"}, {ID: "f1", Side: "file"}, {ID: "f2", Side: "file"}, {ID: "l2", Side: "old"}, {ID: "f3", Side: "file"}}
	for _, tc := range []struct {
		result SubmitPRReviewResult
		want   string
	}{
		{SubmitPRReviewResult{PostedReview: true, PostedFileComments: 3}, "l1 f1 f2 l2 f3"},
		{SubmitPRReviewResult{PostedReview: true, PostedFileComments: 1, PartialFailurePath: "b.go"}, "l1 f1 l2"},
		{SubmitPRReviewResult{PostedReview: true, PartialFailurePath: "a.go"}, "l1 l2"},
	} {
		if got := strings.Join(postedDraftIDs(drafts, tc.result), " "); got != tc.want {
			t.Fatalf("%+v: posted %q, want %q", tc.result, got, tc.want)
		}
	}
}
