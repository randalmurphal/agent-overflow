package git

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/forgeapi"
)

// submitDeadline is a caller context that reaches its deadline while the
// forge holds the request named hold, which then answers: past it the
// submit starts no request, and the one in flight finishes and is reported.
func submitDeadline(t *testing.T) (context.Context, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeoutCause(t.Context(), 300*time.Millisecond, errors.New("the caller's budget ran out"))
	t.Cleanup(cancel)
	return ctx, func() { <-ctx.Done() }
}

// A GitHub submit past its caller's deadline posts the review that was in
// flight and none of the file comments after it, and says which posted:
// the comments it never started are not reported as maybe on the PR.
func TestGitHubSubmitReviewStopsAtItsCallersDeadline(t *testing.T) {
	t.Parallel()
	ctx, wait := submitDeadline(t)
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Op == "ViewerReviews":
			return githubData(`{"repository":{"pullRequest":{"reviews":{"nodes":[]}}}}`)
		case call.Method == http.MethodPost && call.Path == "repos/o/r/pulls/5/reviews":
			wait()
			return forgeAPIAnswer{Body: `{"id":1}`}
		case call.Method == http.MethodPost && call.Path == "repos/o/r/pulls/5/comments":
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":9}`}
		}
		return forgeUnexpected(t, call)
	})
	result, err := core.SubmitReview(ctx, githubTestRef(5), SubmitReviewRequest{
		Verdict: ReviewVerdictComment, HeadSHA: githubTestPinnedHead, Body: "looks fine",
		Comments: []ReviewLineComment{{Path: "b.go", Body: "file", Side: "file"}, {Path: "c.go", Body: "file", Side: "file"}},
	})
	partial, ok := errors.AsType[*PartialSubmitError](err)
	if !ok || !result.PostedReview || result.PostedFileComments != 0 || !partial.PostedReview || partial.FailedPath != "b.go" || partial.Unconfirmed {
		t.Fatalf("SubmitReview = %+v, %v; want the review posted and its file comments stopped, not unconfirmed", result, err)
	}
	if !forgeapi.WriteNotApplied(partial.Err) {
		t.Fatalf("the stop reads as a write that may have applied: %v", partial.Err)
	}
	for _, call := range calls.all() {
		if call.Path == "repos/o/r/pulls/5/comments" {
			t.Fatal("a file comment was sent past the deadline")
		}
	}
}

// A GitLab submit past its caller's deadline creates no further draft
// note, removes the one it created and publishes nothing: the review did
// not post. One still reading when the deadline passes stops there, with
// the cancellation the app tells a cut read by.
func TestGitLabSubmitReviewStopsAtItsCallersDeadline(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	line := 3
	comments := []ReviewLineComment{{Path: "a.go", Body: "one", Line: &line, Side: "right"}, {Path: "a.go", Body: "two", Line: &line, Side: "right"}}
	for _, tc := range []struct {
		name, hold string
		cut        bool
		want       []string
	}{
		{"during a create", "POST " + mr + "/draft_notes", false,
			[]string{"GET metadata", "GET " + mr, "GET " + mr + "/draft_notes?per_page=100", "GET " + mr + "/diffs?per_page=100", "POST " + mr + "/draft_notes", "DELETE " + mr + "/draft_notes/101"}},
		{"during a read", "GET " + mr + "/diffs?per_page=100", true,
			[]string{"GET metadata", "GET " + mr, "GET " + mr + "/draft_notes?per_page=100", "GET " + mr + "/diffs?per_page=100"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, wait := submitDeadline(t)
			forge := &gitlabDraftForge{next: 100, pending: map[int64]string{}}
			held := false
			core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				if call.Method+" "+call.Path == tc.hold && !held {
					held = true
					wait()
				}
				return forge.answer(t, mr)(call)
			})
			result, err := core.SubmitReview(ctx, gitlabTestRef(7), SubmitReviewRequest{Verdict: ReviewVerdictComment, HeadSHA: gitlabTestHead, Comments: comments})
			if err == nil || result.PostedReview {
				t.Fatalf("SubmitReview = %+v, %v; want it stopped with nothing posted", result, err)
			}
			if cut := errors.Is(err, context.Canceled); cut != tc.cut {
				t.Fatalf("SubmitReview = %v; cancelled %t, want %t", err, cut, tc.cut)
			}
			if _, unconfirmed := errors.AsType[*UnconfirmedSubmitError](err); unconfirmed {
				t.Fatalf("a submit stopped before its publish reads as unconfirmed: %v", err)
			}
			if got := gitlabPaths(calls); !slices.Equal(got, tc.want) {
				t.Fatalf("requests = %q\nwant %q", got, tc.want)
			}
			if pending := forge.pendingIDs(); len(pending) != 0 {
				t.Fatalf("drafts left pending: %v", pending)
			}
		})
	}
}

// A GitLab Request changes whose deadline passes during the publish keeps
// what the publish did and posts no changes-requested thread after it,
// and says the thread did not post rather than that it may have.
func TestGitLabRequestChangesStopsAtItsCallersDeadline(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	ctx, wait := submitDeadline(t)
	forge := &gitlabDraftForge{next: 100, pending: map[int64]string{}}
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Op == "" && call.Path == "metadata":
			return gitlabMetadata("19.6.0-ee")
		case call.Method == http.MethodPost && call.Path == mr+"/draft_notes/bulk_publish":
			wait()
		case call.Method == http.MethodPost && call.Path == mr+"/discussions":
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":"abc","individual_note":false,"notes":[{"id":5,"type":"DiscussionNote","resolvable":true}]}`}
		}
		return forge.answer(t, mr)(call)
	})
	result, err := core.SubmitReview(ctx, gitlabTestRef(7), SubmitReviewRequest{Verdict: ReviewVerdictRequestChanges, HeadSHA: gitlabTestHead, Body: "Rename it."})
	partial, ok := errors.AsType[*PartialSubmitError](err)
	if !ok || !result.PostedReview || !partial.PostedReview || partial.Verdict != SubmitStepChangesThread || partial.Unconfirmed {
		t.Fatalf("SubmitReview = %+v, %v; want the publish kept and the thread stopped, not unconfirmed", result, err)
	}
	want := []string{"GET metadata", "GET " + mr, "POST " + mr + "/draft_notes/bulk_publish"}
	if got := gitlabPaths(calls); !slices.Equal(got, want) {
		t.Fatalf("requests = %q\nwant %q", got, want)
	}
}
