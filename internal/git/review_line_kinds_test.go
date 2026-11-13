package git

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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
		case "GET " + mr + "/diffs?per_page=100":
			return gitlabTestDiffsAnswer()
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

// gitlabLineKindsDiff is a.go's diff in the review tests: added lines 3
// and 5-8, unchanged lines (old 3 at new 4, old 5 at new 10, ...), a
// removed line at old 9. gitlabRemovalsDiff is b.go's: old lines 2-4
// removed.
const (
	gitlabLineKindsDiff = "@@ -1,10 +1,14 @@\n o1\n o2\n+n3\n o3\n+n5\n+n6\n+n7\n+n8\n o4\n o5\n o6\n o7\n o8\n-o9\n o10\n"
	gitlabRemovalsDiff  = "@@ -1,6 +1,3 @@\n a\n-b\n-c\n-d\n e\n f\n"
)

// gitlabTestDiffFiles is the diff the GitLab review tests position on.
func gitlabTestDiffFiles() map[string]gitlabDiffFile {
	return map[string]gitlabDiffFile{
		"a.go": parseGitLabDiffFile(gitlabDiffRaw{OldPath: "a.go", NewPath: "a.go", Diff: gitlabLineKindsDiff}),
		"b.go": parseGitLabDiffFile(gitlabDiffRaw{OldPath: "b.go", NewPath: "b.go", Diff: gitlabRemovalsDiff}),
	}
}

// gitlabTestDiffsAnswer is the merge request's diffs read answering them.
func gitlabTestDiffsAnswer() forgeAPIAnswer {
	return forgeAPIAnswer{Body: `[{"old_path":"a.go","new_path":"a.go","diff":` + strconv.Quote(gitlabLineKindsDiff) + `},{"old_path":"b.go","new_path":"b.go","diff":` + strconv.Quote(gitlabRemovalsDiff) + `}]`}
}

// A range names each end as GitLab names a diff line: an unchanged line by
// both numbers with no type, an added line by its new number, a removed
// line by its old one, each line code sha1(path) and both of the diff's
// cursors at that line: an added line's old cursor is the next old line,
// a removed line's new cursor the next new line. A line the diff does not
// have is refused.
func TestGitLabRangePositionsNameEachEnd(t *testing.T) {
	t.Parallel()
	files := gitlabTestDiffFiles()
	end, old := 10, 5
	position, err := gitlabPositionForComment(ReviewLineComment{
		Path: "a.go", Body: "x", Line: &end, OldLine: &old, Side: "right",
		Start: &ReviewLineStart{Line: 5, OldLine: 0, Side: "right"},
	}, &PRDiffRefs{BaseSHA: "b", HeadSHA: "h", StartSHA: "s"}, files)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(position)
	code := fmt.Sprintf("%x", sha1.Sum([]byte("a.go")))
	want := `{"base_sha":"b","start_sha":"s","head_sha":"h","old_path":"a.go","new_path":"a.go","position_type":"text","old_line":5,"new_line":10,` +
		`"line_range":{"start":{"line_code":"` + code + `_4_5","type":"new","new_line":5},"end":{"line_code":"` + code + `_5_10","old_line":5,"new_line":10}}}`
	if string(got) != want {
		t.Fatalf("position =\n%s\nwant\n%s", got, want)
	}
	removedEnd := 4
	left, err := gitlabPositionForComment(ReviewLineComment{
		Path: "b.go", Body: "x", Line: &removedEnd, Side: "left", Start: &ReviewLineStart{Line: 2, Side: "left"},
	}, &PRDiffRefs{}, files)
	if err != nil {
		t.Fatal(err)
	}
	bcode := fmt.Sprintf("%x", sha1.Sum([]byte("b.go")))
	if got, _ := json.Marshal(left.LineRange); string(got) != `{"start":{"line_code":"`+bcode+`_2_2","type":"old","old_line":2},"end":{"line_code":"`+bcode+`_4_2","type":"old","old_line":4}}` {
		t.Fatalf("left range = %s", got)
	}
	added := 3
	for name, bad := range map[string]ReviewLineComment{
		"an unchanged start on the left": {Path: "b.go", Body: "x", Line: &removedEnd, Side: "left", Start: &ReviewLineStart{Line: 3, OldLine: 3, Side: "left"}},
		"a line the diff lacks":          {Path: "a.go", Body: "x", Line: &removedEnd, Side: "right"},
		"an unchanged line's wrong pair": {Path: "a.go", Body: "x", Line: &end, OldLine: &added, Side: "right"},
		"a file the diff lacks":          {Path: "c.go", Body: "x", Side: "file"},
	} {
		if _, err := gitlabPositionForComment(bad, &PRDiffRefs{}, files); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A renamed file's position carries both its paths, and its line codes
// hash the new path, as GitLab's do.
func TestGitLabPositionsOnARenamedFileCarryBothPaths(t *testing.T) {
	t.Parallel()
	files := map[string]gitlabDiffFile{}
	file := parseGitLabDiffFile(gitlabDiffRaw{OldPath: "old.go", NewPath: "new.go", Diff: "@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n"})
	files["new.go"], files["old.go"] = file, file
	removed, added := 2, 2
	code := fmt.Sprintf("%x", sha1.Sum([]byte("new.go")))
	for want, comment := range map[string]ReviewLineComment{
		code + "_2_2": {Path: "new.go", Body: "x", Line: &removed, Side: "left", Start: &ReviewLineStart{Line: 2, Side: "left"}},
		code + "_3_2": {Path: "old.go", Body: "x", Line: &added, Side: "right", Start: &ReviewLineStart{Line: 2, Side: "right"}},
	} {
		position, err := gitlabPositionForComment(comment, &PRDiffRefs{}, files)
		if err != nil {
			t.Fatal(err)
		}
		if position.OldPath != "old.go" || position.NewPath != "new.go" || position.LineRange.Start.LineCode != want {
			t.Fatalf("%s %s position = %+v", comment.Path, comment.Side, position)
		}
	}
}

// An unchanged line outside every hunk (expanded context) takes the offset
// of the gap it is in; a pair that does not keep that offset is refused.
func TestGitLabPositionsOnExpandedContext(t *testing.T) {
	t.Parallel()
	file := parseGitLabDiffFile(gitlabDiffRaw{OldPath: "x.go", NewPath: "x.go", Diff: "@@ -5,2 +7,3 @@\n x\n+n\n y\n"})
	code := fmt.Sprintf("%x", sha1.Sum([]byte("x.go")))
	for _, tc := range []struct{ line, old int }{{3, 1}, {7, 5}, {10, 7}, {30, 27}} {
		point, err := file.point(tc.line, tc.old, "right")
		if err != nil || point.LineCode != code+"_"+strconv.Itoa(tc.old)+"_"+strconv.Itoa(tc.line) {
			t.Fatalf("line %d/%d = %+v, %v", tc.line, tc.old, point, err)
		}
	}
	for _, tc := range []struct{ line, old int }{{3, 3}, {8, 6}, {10, 8}} {
		if point, err := file.point(tc.line, tc.old, "right"); err == nil {
			t.Fatalf("line %d/%d = %+v, want a refusal", tc.line, tc.old, point)
		}
	}
}
