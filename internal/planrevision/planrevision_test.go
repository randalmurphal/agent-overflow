package planrevision

import (
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

func TestBuildPromptRendersSelectedTextAndBody(t *testing.T) {
	got := BuildPrompt([]store.ProposedPlanComment{
		{SelectedText: "Add login route", Body: "Use the standard auth helper."},
		{SelectedText: "", Body: "Also bump the version."},
		{SelectedText: "Refactor cache", Body: ""},
	})
	want := strings.Join([]string{
		"Add login route\ncomment: Use the standard auth helper.",
		"comment: Also bump the version.",
		"Refactor cache\ncomment: ",
	}, "\n\n")
	if got != want {
		t.Fatalf("prompt mismatch\n  got:\n%s\n  want:\n%s", got, want)
	}
}

func TestBuildPromptSkipsEmptyComments(t *testing.T) {
	got := BuildPrompt([]store.ProposedPlanComment{
		{SelectedText: "  ", Body: "  "},
		{SelectedText: "live", Body: "do it"},
	})
	want := "live\ncomment: do it"
	if got != want {
		t.Fatalf("prompt mismatch\n  got:\n%s\n  want:\n%s", got, want)
	}
}

func TestBuildPromptEmptyInputReturnsEmptyString(t *testing.T) {
	if got := BuildPrompt(nil); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
	if got := BuildPrompt([]store.ProposedPlanComment{}); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestIDsOfPreservesOrder(t *testing.T) {
	got := IDsOf([]store.ProposedPlanComment{{ID: "p1"}, {ID: "p2"}, {ID: "p3"}})
	want := []string{"p1", "p2", "p3"}
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
