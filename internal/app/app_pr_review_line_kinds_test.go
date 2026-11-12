package app

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

// A draft on an unchanged line carries both numbers whichever half it was
// written on, and goes to the forge as that line: its new number on the
// right, with its old number beside it. A deleted line stays left at its
// old number, an added line right at its new number.
func TestReviewLineCommentForDraftOfEachKindOfLine(t *testing.T) {
	t.Parallel()
	n := func(v int) *int { return &v }
	cases := []struct {
		name  string
		draft store.DiffReviewComment
		want  gitops.ReviewLineComment
		ok    bool
	}{
		{"added", store.DiffReviewComment{Side: "new", NewLine: 5}, gitops.ReviewLineComment{Line: n(5), Side: "right"}, true},
		{"deleted", store.DiffReviewComment{Side: "old", OldLine: 9}, gitops.ReviewLineComment{Line: n(9), Side: "left"}, true},
		{"unchanged, left half", store.DiffReviewComment{Side: "old", OldLine: 3, NewLine: 4}, gitops.ReviewLineComment{Line: n(4), OldLine: n(3), Side: "right"}, true},
		{"unchanged, right half", store.DiffReviewComment{Side: "new", OldLine: 3, NewLine: 4}, gitops.ReviewLineComment{Line: n(4), OldLine: n(3), Side: "right"}, true},
		{"unchanged, unified", store.DiffReviewComment{Side: "context", OldLine: 3, NewLine: 4}, gitops.ReviewLineComment{Line: n(4), OldLine: n(3), Side: "right"}, true},
		{"unchanged, one number", store.DiffReviewComment{Side: "context", NewLine: 4}, gitops.ReviewLineComment{}, false},
		{"file", store.DiffReviewComment{Side: "file"}, gitops.ReviewLineComment{Side: "file"}, true},
	}
	for _, tc := range cases {
		tc.draft.FilePath, tc.draft.Body = "main.go", tc.name
		got, ok := reviewLineCommentForDraft(tc.draft)
		want := tc.want
		if tc.ok {
			want.Path, want.Body = "main.go", tc.name
		}
		if ok != tc.ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: = %+v (%v), want %+v (%v)", tc.name, got, ok, want, tc.ok)
		}
	}
}

// One review with a comment on each kind of line posts on both forges,
// and each lands on its line: an unchanged line on its new number.
func TestSubmitPRReviewPostsEachKindOfLine(t *testing.T) {
	t.Parallel()
	for _, forge := range []string{"gitlab", "github"} {
		c := newReviewSubmitCase(t, forge)
		var drafts []store.DiffReviewComment
		for _, input := range []store.DiffReviewCommentInput{
			{FilePath: "main.go", NewLine: 5, Side: "new", Body: "added"},
			{FilePath: "main.go", OldLine: 9, Side: "old", Body: "deleted"},
			{FilePath: "main.go", OldLine: 3, NewLine: 4, Side: "old", Body: "unchanged, left half"},
			{FilePath: "main.go", OldLine: 3, NewLine: 4, Side: "new", Body: "unchanged, right half"},
			{FilePath: "main.go", OldLine: 3, NewLine: 4, Side: "context", Body: "unchanged, unified"},
		} {
			input.Scope, input.SourceKey = string(store.DiffReviewScopePR), prDraftSource(c.pr)
			created, err := c.app.CreatePRDiffReviewComment(c.pr, input)
			if err != nil {
				t.Fatal(err)
			}
			drafts = append(drafts, created)
		}
		result, err := c.submit(c.request(false, drafts...))
		if err != nil || !result.PostedReview || len(result.SentDraftIDs) != len(drafts) {
			t.Fatalf("%s: submit = %+v, %v", forge, result, err)
		}
		threads, err := c.app.gitCore().ListReviewThreads(context.Background(), c.pr)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, thread := range threads {
			if thread.Path == "" {
				continue // the review's summary
			}
			line := 0
			if thread.Line != nil {
				line = *thread.Line
			}
			got[thread.Comments[0].Body] = fmt.Sprintf("%s %d", thread.Side, line)
		}
		want := map[string]string{
			"added": "right 5", "deleted": "left 9",
			"unchanged, left half": "right 4", "unchanged, right half": "right 4", "unchanged, unified": "right 4",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: threads = %v, want %v", forge, got, want)
		}
	}
}
