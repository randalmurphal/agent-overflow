package main

import (
	"testing"

	"agent-overflow/internal/store"
)

func TestBuildDiffReviewPromptRendersFileAndLineAnchors(t *testing.T) {
	prompt := buildDiffReviewPrompt([]store.DiffReviewComment{
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

func TestBuildDiffReviewPromptSkipsBlankBodies(t *testing.T) {
	prompt := buildDiffReviewPrompt([]store.DiffReviewComment{
		{FilePath: "a.ts", Side: "new", NewLine: 1, Body: "  \n"},
		{FilePath: "b.ts", Side: "new", NewLine: 5, Body: "real"},
	})

	want := "b.ts:5:\ncomment: real"
	if prompt != want {
		t.Fatalf("prompt mismatch\n  got:\n%s\n  want:\n%s", prompt, want)
	}
}
