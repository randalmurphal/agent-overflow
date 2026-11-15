package git

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// A review names the head its diff was loaded at, and a request without
// one is refused before anything reaches either forge.
func TestSubmitReviewRequiresTheLoadedHead(t *testing.T) {
	t.Parallel()
	for _, ref := range []PRReference{githubTestRef(5), gitlabTestRef(7)} {
		core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer { return forgeUnexpected(t, call) })
		for _, head := range []string{"", "  ", "not-a-sha"} {
			if _, err := core.SubmitReview(t.Context(), ref, SubmitReviewRequest{Verdict: ReviewVerdictApprove, HeadSHA: head}); err == nil {
				t.Fatalf("%s: a review at head %q was sent", ref.Forge, head)
			}
		}
		if n := len(calls.all()); n != 0 {
			t.Fatalf("%s: %d requests for refused reviews", ref.Forge, n)
		}
	}
}

// GitHub pins the review, its line comments and its file comments to the
// head the reviewer loaded, however the PR's head moved since: the review
// carries commit_id and each file comment the same commit, with no read of
// the live head.
func TestGitHubSubmitReviewPinsTheLoadedHead(t *testing.T) {
	t.Parallel()
	const loaded = "1111111111111111111111111111111111111111"
	line := 3
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Op == "ViewerReviews":
			return githubData(githubNoViewerReviews)
		case call.Method == http.MethodPost && call.Path == "repos/o/r/pulls/5/reviews":
			return forgeAPIAnswer{Body: `{"id":1}`}
		case call.Method == http.MethodPost && call.Path == "repos/o/r/pulls/5/comments":
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":2}`}
		}
		return forgeUnexpected(t, call)
	})
	review := SubmitReviewRequest{Verdict: ReviewVerdictApprove, Body: "ok", HeadSHA: loaded, Comments: []ReviewLineComment{
		{Path: "a.go", Body: "line", Line: &line, Side: "right"},
		{Path: "b.go", Body: "file", Side: "file"},
	}}
	if result, err := core.SubmitReview(t.Context(), githubTestRef(5), review); err != nil || !result.PostedReview || result.PostedFileComments != 1 {
		t.Fatalf("SubmitReview = %+v, %v", result, err)
	}
	pinned := 0
	for _, call := range calls.all() {
		if call.Op == "PRTick" {
			t.Fatal("the submit read the live head")
		}
		if call.Method != http.MethodPost || call.Op != "" {
			continue
		}
		var body struct {
			CommitID string `json:"commit_id"`
		}
		if err := json.Unmarshal(call.Body, &body); err != nil || body.CommitID != loaded {
			t.Fatalf("%s body = %s, want commit_id %s", call.Path, call.Body, loaded)
		}
		pinned++
	}
	if pinned != 2 {
		t.Fatalf("%d writes pinned, want the review and the file comment", pinned)
	}
}

// GitLab refuses a review whose head is not the merge request's before any
// write, and approves the head the reviewer loaded.
func TestGitLabSubmitReviewRefusesAMovedHead(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch call.Method + " " + call.Path {
		case "GET " + mr:
			return forgeAPIAnswer{Body: gitlabTestMR(7, 0)}
		case "GET " + mr + "/approvals":
			return forgeAPIAnswer{Body: `{"user_can_approve":true}`}
		case "GET " + mr + "/draft_notes?per_page=100":
			return forgeAPIAnswer{Body: `[]`}
		case "POST " + mr + "/draft_notes":
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":1}`}
		case "POST " + mr + "/draft_notes/bulk_publish":
			return forgeAPIAnswer{Status: http.StatusNoContent}
		case "POST " + mr + "/approve":
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{}`}
		}
		return forgeUnexpected(t, call)
	})
	for _, verdict := range []string{ReviewVerdictApprove, ReviewVerdictComment} {
		_, err := core.SubmitReview(t.Context(), gitlabTestRef(7), SubmitReviewRequest{Verdict: verdict, Body: "ok", HeadSHA: "0123456789abcdef"})
		if moved, ok := errors.AsType[*HeadMovedError](err); !ok || moved.Have != "ab12cd34" {
			t.Fatalf("%s at a moved head = %v, want *HeadMovedError", verdict, err)
		}
	}
	for _, call := range calls.all() {
		if call.Method != http.MethodGet {
			t.Fatalf("a refused review sent %s %s", call.Method, call.Path)
		}
	}
	if _, err := core.SubmitReview(t.Context(), gitlabTestRef(7), SubmitReviewRequest{Verdict: ReviewVerdictApprove, Body: "ok", HeadSHA: "ab12cd34"}); err != nil {
		t.Fatalf("SubmitReview at the head = %v", err)
	}
	sent := calls.all()
	last := sent[len(sent)-1]
	var approve map[string]string
	if err := json.Unmarshal(last.Body, &approve); err != nil || last.Path != mr+"/approve" || approve["sha"] != "ab12cd34" {
		t.Fatalf("approval = %s %s, want the loaded head", last.Path, last.Body)
	}
}
