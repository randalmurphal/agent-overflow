package threadtitle

import (
	"errors"
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "plain", raw: "Fix the bug", want: "Fix the bug"},
		{name: "trims whitespace", raw: "  Trimmed  ", want: "Trimmed"},
		{name: "strips quotes", raw: `"quoted title"`, want: "quoted title"},
		{name: "collapses whitespace", raw: "lots   of\tspaces", want: "lots of spaces"},
		{name: "keeps first line only", raw: "First line\nSecond", want: "First line"},
		{name: "empty falls back to default", raw: "   ", want: Default},
		{
			name: "truncates when over 50 runes",
			raw:  "This is a very long title that exceeds fifty runes for sure indeed",
			want: "This is a very long title that exceeds fifty ru...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Sanitize(tt.raw)
			if got != tt.want {
				t.Fatalf("Sanitize(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestBuildPromptIncludesMessage(t *testing.T) {
	got := BuildPrompt("fix the login bug", nil)
	if !strings.Contains(got, "fix the login bug") {
		t.Fatalf("prompt missing user message: %q", got)
	}
	if !strings.Contains(got, "Return a JSON object") {
		t.Fatalf("prompt missing JSON object directive: %q", got)
	}
}

func TestDecodeClaudeExtractsFromStructuredOutput(t *testing.T) {
	payload := []byte(`
{"some":"preamble"}
{"structured_output":{"title":"Refactor worktree rename"}}
`)
	got, err := DecodeClaude(payload)
	if err != nil {
		t.Fatalf("DecodeClaude() error = %v", err)
	}
	if got != "Refactor worktree rename" {
		t.Fatalf("title = %q", got)
	}
}

func TestDecodeClaudeErrorsOnEmptyOutput(t *testing.T) {
	_, err := DecodeClaude([]byte("   \n\t"))
	if err == nil {
		t.Fatal("DecodeClaude(blank) error = nil, want empty-output error")
	}
}

func TestDecodeClaudeErrorsOnMalformedJSON(t *testing.T) {
	_, err := DecodeClaude([]byte("not-json"))
	if err == nil {
		t.Fatal("DecodeClaude(garbage) error = nil, want decode error")
	}
	if !strings.Contains(err.Error(), "decode claude structured output") {
		t.Fatalf("DecodeClaude() error = %v, want decode context", err)
	}
}

func TestRedactErrorReplacesCLIFailure(t *testing.T) {
	err := errors.New("codex CLI failed: exit status 1")
	if got := RedactError(err); got != "provider CLI failed" {
		t.Fatalf("RedactError(%q) = %q, want %q", err, got, "provider CLI failed")
	}
}

func TestRedactErrorPassesNonCLIErrorsThrough(t *testing.T) {
	err := errors.New("decode claude structured output: unexpected EOF")
	got := RedactError(err)
	if got != err.Error() {
		t.Fatalf("RedactError(%q) = %q, want passthrough", err, got)
	}
}
