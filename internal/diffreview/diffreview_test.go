package diffreview

import (
	"testing"

	"agent-overflow/internal/store"
)

func TestBuildPromptRendersFileAndLineAnchors(t *testing.T) {
	prompt := BuildPrompt([]store.DiffReviewComment{
		{
			FilePath: "app.ts",
			Side:     "new",
			NewLine:  42,
			Body:     "Extract this into a named helper.",
		},
		{
			FilePath: "legacy.ts",
			Side:     "old",
			OldLine:  17,
			Body:     "Why is this still here?",
		},
		{
			FilePath: "README.md",
			Side:     "file",
			Body:     "Mention the new flag here too.",
		},
	})

	want := "app.ts:42:\ncomment: Extract this into a named helper.\n\n" +
		"legacy.ts:17:\ncomment: Why is this still here?\n\n" +
		"README.md:\ncomment: Mention the new flag here too."
	if prompt != want {
		t.Fatalf("prompt mismatch\n  got:\n%s\n  want:\n%s", prompt, want)
	}
}

func TestBuildPromptSkipsBlankBodies(t *testing.T) {
	prompt := BuildPrompt([]store.DiffReviewComment{
		{FilePath: "a.ts", Side: "new", NewLine: 1, Body: "  \n"},
		{FilePath: "b.ts", Side: "new", NewLine: 5, Body: "real"},
	})

	want := "b.ts:5:\ncomment: real"
	if prompt != want {
		t.Fatalf("prompt mismatch\n  got:\n%s\n  want:\n%s", prompt, want)
	}
}

func TestBuildPromptWithPRContextAddsHeaderAndHunks(t *testing.T) {
	prompt := BuildPromptWithPRContext([]store.DiffReviewComment{
		{ID: "c1", FilePath: "app.ts", Side: "new", NewLine: 5, Body: "Fix this."},
	}, &store.DiffReviewPRContext{
		Number: 12,
		URL:    "https://github.com/o/r/pull/12",
		Comments: []store.DiffReviewPRContextEntry{{
			CommentID:   "c1",
			HunkExcerpt: "   4    4 context\n        5 +added",
		}},
	})

	want := "PR #12 - https://github.com/o/r/pull/12\n\n" +
		"app.ts:5:\ncomment: Fix this.\n\n" +
		"hunk:\n   4    4 context\n        5 +added"
	if prompt != want {
		t.Fatalf("prompt mismatch\n got:\n%s\nwant:\n%s", prompt, want)
	}
}

func TestCommentLinePrefersNewThenOld(t *testing.T) {
	cases := []struct {
		name string
		c    store.DiffReviewComment
		want int
	}{
		{name: "new wins", c: store.DiffReviewComment{NewLine: 5, OldLine: 9}, want: 5},
		{name: "falls back to old", c: store.DiffReviewComment{OldLine: 9}, want: 9},
		{name: "file-level is 0", c: store.DiffReviewComment{}, want: 0},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := CommentLine(tt.c); got != tt.want {
				t.Fatalf("CommentLine = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestIDsOfPreservesOrder(t *testing.T) {
	got := IDsOf([]store.DiffReviewComment{
		{ID: "c1"}, {ID: "c2"}, {ID: "c3"},
	})
	want := []string{"c1", "c2", "c3"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("idx %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestIDsOfReturnsEmptySliceNotNil(t *testing.T) {
	got := IDsOf(nil)
	if got == nil {
		t.Fatal("expected empty slice, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}
