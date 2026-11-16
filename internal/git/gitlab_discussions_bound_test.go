package git

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// A merge request whose discussions run past ten pages is read to ten,
// which the threads read says; a submit with held replies there cannot
// tell a gone discussion from one past the bound and fails before any
// write.
func TestGitLabDiscussionsReadIsBounded(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if reply, ok := gitlabManageExtras(call); ok {
			return reply
		}
		switch {
		case call.Method == http.MethodGet && call.Path == mr:
			return forgeAPIAnswer{Body: gitlabTestMR(7, 0)}
		case call.Method == http.MethodGet && strings.HasPrefix(call.Path, mr+"/discussions?"):
			page := 1
			if _, after, ok := strings.Cut(call.Path, "page="); ok {
				page, _ = strconv.Atoi(strings.SplitN(after, "&", 2)[0])
			}
			return forgeAPIAnswer{Body: "[" + gitlabDiscussion(fmt.Sprintf("d%d", page), "ab12cd34") + "]", Header: http.Header{"X-Next-Page": {strconv.Itoa(page + 1)}}}
		case call.Method == http.MethodGet && call.Path == mr+"/draft_notes?per_page=100":
			return forgeAPIAnswer{Body: `[]`}
		case call.Path == gitlabTestProject:
			return forgeAPIAnswer{Body: gitlabTestProjectSettings}
		case call.Path == "user":
			return forgeAPIAnswer{Body: `{"username":"viewer"}`}
		}
		return forgeUnexpected(t, call)
	})
	read, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Threads: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Threads) != 10 || read.ThreadsTruncated != "Only the first 1000 threads and comments of this conversation are shown; the rest are on forge.example." {
		t.Fatalf("%d threads, truncated %q", len(read.Threads), read.ThreadsTruncated)
	}
	if n := countPrefix(gitlabPaths(calls), "GET "+mr+"/discussions"); n != 10 {
		t.Fatalf("%d discussions pages read, want 10", n)
	}
	seen := len(calls.all())
	_, err = core.SubmitReview(t.Context(), gitlabTestRef(7), SubmitReviewRequest{Verdict: ReviewVerdictComment, HeadSHA: gitlabTestHead, Comments: []ReviewLineComment{
		{Body: "held", InReplyTo: &ReviewThreadReply{ThreadID: "d3"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "more than 1000 discussions") {
		t.Fatalf("SubmitReview = %v, want the bound named", err)
	}
	for _, call := range calls.all()[seen:] {
		if call.Method != http.MethodGet {
			t.Fatalf("a submit past the bound sent %s %s", call.Method, call.Path)
		}
	}
}
