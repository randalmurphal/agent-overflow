package def

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInlinePrompts(t *testing.T) {
	dir := t.TempDir()
	writePrompt := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writePrompt("phase.md", "phase body")
	writePrompt("unit.md", "unit body")
	writePrompt("join.md", "join body")

	original := Workflow{ID: "flow", Phases: []Phase{{
		ID: "work", Prompt: "phase.md",
		FanOut: []Unit{{ID: "lens", Prompt: "unit.md"}, {ID: "empty"}},
		Join:   &Unit{ID: "combine", Prompt: "join.md"},
	}}}
	got, err := InlinePrompts(ResolvedWorkflow{Workflow: original, Path: filepath.Join(dir, "workflow.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Phases[0].Prompt != "phase body" || got.Phases[0].FanOut[0].Prompt != "unit body" || got.Phases[0].Join.Prompt != "join body" {
		t.Fatalf("inlined workflow = %#v", got)
	}
	if got.Phases[0].FanOut[1].Prompt != "" {
		t.Fatalf("empty prompt = %q, want empty", got.Phases[0].FanOut[1].Prompt)
	}
	if original.Phases[0].Prompt != "phase.md" || original.Phases[0].FanOut[0].Prompt != "unit.md" || original.Phases[0].Join.Prompt != "join.md" {
		t.Fatalf("source workflow was mutated: %#v", original)
	}
}

func TestInlinePromptsRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "outside.md")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })
	large := filepath.Join(dir, "large.md")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", int(MaxPromptBytes)+1)), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		prompt string
		want   string
	}{
		{name: "missing", prompt: "missing.md", want: "phase \"work\" prompt \"missing.md\""},
		{name: "escape", prompt: "../outside.md", want: "resolves outside"},
		{name: "size", prompt: "large.md", want: "exceeds 4194304-byte limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := InlinePrompts(ResolvedWorkflow{
				Workflow: Workflow{ID: "flow", Phases: []Phase{{ID: "work", Prompt: test.prompt}}},
				Path:     filepath.Join(dir, "workflow.yaml"),
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("InlinePrompts error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestInlinePromptsEmptyWorkflowIsNoOp(t *testing.T) {
	original := Workflow{ID: "empty", Phases: []Phase{{ID: "work"}}}
	got, err := InlinePrompts(ResolvedWorkflow{Workflow: original, Path: filepath.Join(t.TempDir(), "workflow.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Phases[0].Prompt != "" {
		t.Fatalf("prompt = %q, want empty", got.Phases[0].Prompt)
	}
}
