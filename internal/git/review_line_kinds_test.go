package git

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The review comments of one review on each kind of line, as the app
// builds them from drafts: an added line, a deleted line, and an unchanged
// line (old 3, new 4) reached from the left half, the right half and the
// unified view, which all post the same way.
func reviewLineKinds() []ReviewLineComment {
	added, deleted, newLine, oldLine := 5, 9, 4, 3
	unchanged := func(body string) ReviewLineComment {
		return ReviewLineComment{Path: "a.go", Body: body, Line: &newLine, OldLine: &oldLine, Side: "right"}
	}
	return []ReviewLineComment{
		{Path: "a.go", Body: "added", Line: &added, Side: "right"},
		{Path: "a.go", Body: "deleted", Line: &deleted, Side: "left"},
		unchanged("unchanged left half"),
		unchanged("unchanged right half"),
		unchanged("unchanged unified"),
	}
}

// GitHub takes an unchanged line as RIGHT at its new number (the
// documented form); a deleted line LEFT at its old number, an added line
// RIGHT at its new number. No other field is sent.
func TestGitHubSubmitReviewPostsEachKindOfLine(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Op == "PRTick":
			return githubData(`{"viewer":{"login":"v"},"repository":{"pullRequest":{"number":5,"headRefOid":"abc123","author":{"login":"a"},"latestReviews":{"nodes":[]},"commits":{"nodes":[]}}}}`)
		case call.Method == http.MethodPost && call.Path == "repos/o/r/pulls/5/reviews":
			return forgeAPIAnswer{Body: `{"id":1}`}
		}
		return forgeUnexpected(t, call)
	})
	review := SubmitReviewRequest{Verdict: ReviewVerdictComment, Body: "mixed", Comments: reviewLineKinds()}
	if result, err := core.SubmitReview(t.Context(), githubTestRef(5), review); err != nil || !result.PostedReview {
		t.Fatalf("SubmitReview = %+v, %v", result, err)
	}
	var posted []byte
	for _, call := range calls.all() {
		if call.Method == http.MethodPost && call.Path == "repos/o/r/pulls/5/reviews" {
			posted = call.Body
		}
	}
	var body struct {
		Comments []json.RawMessage `json:"comments"`
	}
	if err := json.Unmarshal(posted, &body); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"path":"a.go","body":"added","line":5,"side":"RIGHT"}`,
		`{"path":"a.go","body":"deleted","line":9,"side":"LEFT"}`,
		`{"path":"a.go","body":"unchanged left half","line":4,"side":"RIGHT"}`,
		`{"path":"a.go","body":"unchanged right half","line":4,"side":"RIGHT"}`,
		`{"path":"a.go","body":"unchanged unified","line":4,"side":"RIGHT"}`,
	}
	if len(body.Comments) != len(want) {
		t.Fatalf("comments = %s", posted)
	}
	for i, comment := range body.Comments {
		if string(comment) != want[i] {
			t.Fatalf("comment %d = %s, want %s", i, comment, want[i])
		}
	}
}

// GitLab needs both numbers for an unchanged line
// (https://docs.gitlab.com/api/discussions/#create-new-merge-request-thread);
// a deleted line has only old_line, an added line only new_line.
func TestGitLabSubmitReviewPostsEachKindOfLine(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch call.Method + " " + call.Path {
		case "GET " + mr:
			return forgeAPIAnswer{Body: gitlabTestMR(7, 0)}
		case "POST " + mr + "/draft_notes":
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":1}`}
		case "POST " + mr + "/draft_notes/bulk_publish":
			return forgeAPIAnswer{Status: http.StatusNoContent}
		}
		return forgeUnexpected(t, call)
	})
	review := SubmitReviewRequest{Verdict: ReviewVerdictComment, Comments: reviewLineKinds()}
	if result, err := core.SubmitReview(t.Context(), gitlabTestRef(7), review); err != nil || !result.PostedReview {
		t.Fatalf("SubmitReview = %+v, %v", result, err)
	}
	refs := `"base_sha":"base","start_sha":"start","head_sha":"head2","old_path":"a.go","new_path":"a.go","position_type":"text"`
	want := []string{
		`{"note":"added","position":{` + refs + `,"new_line":5}}`,
		`{"note":"deleted","position":{` + refs + `,"old_line":9}}`,
		`{"note":"unchanged left half","position":{` + refs + `,"old_line":3,"new_line":4}}`,
		`{"note":"unchanged right half","position":{` + refs + `,"old_line":3,"new_line":4}}`,
		`{"note":"unchanged unified","position":{` + refs + `,"old_line":3,"new_line":4}}`,
	}
	var notes []string
	for _, call := range calls.all() {
		if call.Method == http.MethodPost && strings.HasSuffix(call.Path, "/draft_notes") {
			notes = append(notes, string(call.Body))
		}
	}
	if strings.Join(notes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("draft notes =\n%s\nwant\n%s", strings.Join(notes, "\n"), strings.Join(want, "\n"))
	}
}

// A range over unchanged lines would need both numbers of its start: the
// comment carries only its end's, so it is refused rather than sent
// half-formed.
func TestGitLabRefusesARangeOnAnUnchangedLine(t *testing.T) {
	t.Parallel()
	end, start, old := 4, 2, 3
	_, err := gitlabPositionForComment(ReviewLineComment{Path: "a.go", Body: "x", Line: &end, StartLine: &start, OldLine: &old, Side: "right"}, &PRDiffRefs{})
	if err == nil {
		t.Fatal("a range on an unchanged line was accepted")
	}
}
