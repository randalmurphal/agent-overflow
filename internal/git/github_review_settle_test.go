package git

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// A review POST whose answer is lost (a 502 after GitHub may have applied
// it) is settled by the viewer's reviews: a new one matching the request
// is this submit's and the submit goes on, none means it did not post, and
// anything else is unconfirmed. A refusal needs no read.
func TestGitHubSubmitReviewSettlesALostAnswer(t *testing.T) {
	t.Parallel()
	const old = `{"databaseId":1,"viewerDidAuthor":true,"state":"COMMENTED","body":"looks fine","commit":{"oid":"` + githubTestPinnedHead + `"}}`
	review := func(id int, viewer bool, state, body string) string {
		return fmt.Sprintf(`{"databaseId":%d,"viewerDidAuthor":%t,"state":%q,"body":%q,"commit":{"oid":%q}}`, id, viewer, state, body, githubTestPinnedHead)
	}
	cases := []struct {
		name      string
		post      int
		after     string // the reviews after the POST; "" fails the read
		posted    bool
		unconfirm bool
		reads     int
	}{
		{name: "applied", post: http.StatusBadGateway, after: old + "," + review(2, true, "COMMENTED", "looks fine"), posted: true, reads: 2},
		{name: "not applied", post: http.StatusBadGateway, after: old + "," + review(2, false, "COMMENTED", "looks fine"), reads: 2},
		{name: "another review", post: http.StatusBadGateway, after: old + "," + review(2, true, "COMMENTED", "something else"), unconfirm: true, reads: 2},
		{name: "two new reviews", post: http.StatusBadGateway, after: old + "," + review(2, true, "COMMENTED", "looks fine") + "," + review(3, true, "COMMENTED", "looks fine"), unconfirm: true, reads: 2},
		{name: "pending review", post: http.StatusBadGateway, after: old + "," + review(2, true, "PENDING", "looks fine"), reads: 2},
		{name: "read fails", post: http.StatusBadGateway, unconfirm: true, reads: 2},
		{name: "refused", post: http.StatusUnprocessableEntity, reads: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var reads, files atomic.Int32
			core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				switch {
				case call.Op == "ViewerReviews":
					if reads.Add(1) == 1 {
						return githubData(`{"repository":{"pullRequest":{"reviews":{"nodes":[` + old + `]}}}}`)
					}
					if tc.after == "" {
						return forgeAPIAnswer{Status: http.StatusInternalServerError, Body: `{"message":"Server Error"}`}
					}
					return githubData(`{"repository":{"pullRequest":{"reviews":{"nodes":[` + tc.after + `]}}}}`)
				case call.Method == http.MethodPost && call.Path == "repos/o/r/pulls/5/reviews":
					return forgeAPIAnswer{Status: tc.post, Body: `{"message":"lost"}`}
				case call.Method == http.MethodPost && call.Path == "repos/o/r/pulls/5/comments":
					files.Add(1)
					return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":9}`}
				}
				return forgeUnexpected(t, call)
			})
			result, err := core.SubmitReview(t.Context(), githubTestRef(5), SubmitReviewRequest{
				Verdict: ReviewVerdictComment, HeadSHA: githubTestPinnedHead, Body: " looks fine ",
				Comments: []ReviewLineComment{{Path: "b.go", Body: "file", Side: "file"}},
			})
			if got := reads.Load(); got != int32(tc.reads) {
				t.Fatalf("%d reads of the reviews, want %d", got, tc.reads)
			}
			_, unconfirmed := errors.AsType[*UnconfirmedSubmitError](err)
			switch {
			case tc.posted:
				if err != nil || !result.PostedReview || result.PostedFileComments != 1 || files.Load() != 1 {
					t.Fatalf("SubmitReview = %+v, %v, want the review settled as posted and its file comment sent", result, err)
				}
			case tc.unconfirm:
				if !unconfirmed || result.PostedReview || files.Load() != 0 {
					t.Fatalf("SubmitReview = %+v, %v, want unconfirmed with nothing after it", result, err)
				}
				if !strings.Contains(err.Error(), "lost") {
					t.Fatalf("unconfirmed submit lost the POST's error: %v", err)
				}
			default:
				if err == nil || unconfirmed || result.PostedReview || files.Load() != 0 {
					t.Fatalf("SubmitReview = %+v, %v, want not posted", result, err)
				}
			}
		})
	}
}
