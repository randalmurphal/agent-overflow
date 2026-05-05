package main

import (
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

func TestBuildDiffReviewPromptIncludesFileAndLineAnchors(t *testing.T) {
	prompt := buildDiffReviewPrompt([]store.DiffReviewComment{
		{
			FilePath:     "app.ts",
			Side:         "new",
			NewLine:      42,
			SelectedText: "+const next = true",
			Body:         "Extract this into a named helper.",
		},
		{
			FilePath: "README.md",
			Side:     "file",
			Body:     "Mention the new flag here too.",
		},
	})

	for _, want := range []string{
		`"filePath":"app.ts"`,
		`"newLine":42`,
		`"selectedText":"+const next = true"`,
		`"comment":"Extract this into a named helper."`,
		`"filePath":"README.md"`,
		`"side":"file"`,
		`"comment":"Mention the new flag here too."`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}
