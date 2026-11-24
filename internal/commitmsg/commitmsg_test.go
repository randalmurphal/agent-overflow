package commitmsg

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---- SanitizeSubject ----

func TestSanitizeSubject(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"double quotes and period", `"Add login flow."`, `Add login flow`},
		{"single quotes", "'Fix typo'", `Fix typo`},
		{"backticks", "`Refactor auth`", `Refactor auth`},
		{"trailing period", `Add login flow.`, `Add login flow`},
		{"surrounding spaces", `  Leading and trailing  `, `Leading and trailing`},
		{"keeps only first line", "Add login flow\nExtra body line that should not leak into subject", "Add login flow"},
		{"collapses internal whitespace", "Add    login\t flow", "Add login flow"},
		{"empty", "", ""},
		{"blank", "   ", ""},
		{"newlines only", "\n\n", ""},
		{"empty quotes", `""`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeSubject(tc.in); got != tc.want {
				t.Errorf("SanitizeSubject(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeSubject_TruncatesWithEllipsis(t *testing.T) {
	in := strings.Repeat("x", 100)
	got := SanitizeSubject(in)
	if len([]rune(got)) > 72 {
		t.Errorf("expected <= 72 runes, got %d", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected ellipsis suffix, got %q", got)
	}
}

func TestSanitizeSubject_UnicodeAtBoundary(t *testing.T) {
	in := strings.Repeat("日", 80)
	got := SanitizeSubject(in)
	if len([]rune(got)) > 72 {
		t.Errorf("rune length: got %d, want <=72", len([]rune(got)))
	}
}

// ---- SanitizeBody ----

func TestSanitizeBody(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"collapses blank runs", "First paragraph.\n\n\n\nSecond paragraph.", "First paragraph.\n\nSecond paragraph."},
		{"whitespace only is empty", "   \n\n ", ""},
		{"preserves single newlines", "Line one.\nLine two.\n\nParagraph break.", "Line one.\nLine two.\n\nParagraph break."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeBody(tc.in); got != tc.want {
				t.Errorf("SanitizeBody(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ---- BuildPrompt ----

func TestBuildPrompt_IncludesBranchAndSections(t *testing.T) {
	prompt := BuildPrompt("A\tREADME\n", "+++ b/README\n+new line\n", "main", StyleGuidance{})
	for _, needle := range []string{
		"You write concise git commit messages.",
		"Return a JSON object with keys: subject, body.",
		"subject must be imperative",
		"Branch: main",
		"Staged files:",
		"A\tREADME",
		"Staged patch:",
		"+new line",
	} {
		if !strings.Contains(prompt, needle) {
			t.Errorf("prompt missing %q; got:\n%s", needle, prompt)
		}
	}
}

func TestBuildPrompt_DetachedHEADRendersSentinel(t *testing.T) {
	prompt := BuildPrompt("A\tx", "+++", "", StyleGuidance{})
	if !strings.Contains(prompt, "Branch: (detached)") {
		t.Errorf("expected '(detached)' sentinel; got:\n%s", prompt)
	}
}

// ---- StyleGuidance ----

func TestBuildPrompt_DefaultStyleIsConventional(t *testing.T) {
	for _, g := range []StyleGuidance{
		{},
		{Kind: StyleConventional},
		{Kind: "bogus-from-stale-settings"},
		{Kind: StyleCustom, Custom: "   "},
		{Kind: StyleRepo},
	} {
		prompt := BuildPrompt("A\tx", "+++", "main", g)
		if !strings.Contains(prompt, "Conventional Commits") {
			t.Errorf("guidance %+v: expected conventional fallback; got:\n%s", g, prompt)
		}
	}
}

func TestBuildPrompt_CustomStyleEmbedsInstructions(t *testing.T) {
	g := StyleGuidance{Kind: StyleCustom, Custom: "All subjects start with an emoji."}
	prompt := BuildPrompt("A\tx", "+++", "main", g)
	if !strings.Contains(prompt, "All subjects start with an emoji.") {
		t.Errorf("custom instructions missing; got:\n%s", prompt)
	}
	if strings.Contains(prompt, "Conventional Commits") {
		t.Errorf("custom style must replace the conventional rule; got:\n%s", prompt)
	}
}

func TestBuildPrompt_CustomStyleIsCapped(t *testing.T) {
	g := StyleGuidance{Kind: StyleCustom, Custom: strings.Repeat("x", PromptCustomStyleLimit*2)}
	prompt := BuildPrompt("A\tx", "+++", "main", g)
	if !strings.Contains(prompt, "[truncated]") {
		t.Errorf("oversized instructions not truncated; prompt length %d", len(prompt))
	}
}

func TestBuildPrompt_RepoStyleListsSubjects(t *testing.T) {
	g := StyleGuidance{
		Kind:           StyleRepo,
		RecentSubjects: []string{"feat(auth): add SSO", "fix: stop leaking handles"},
	}
	prompt := BuildPrompt("A\tx", "+++", "main", g)
	for _, needle := range []string{
		"recent commit subjects",
		"  * feat(auth): add SSO",
		"  * fix: stop leaking handles",
	} {
		if !strings.Contains(prompt, needle) {
			t.Errorf("prompt missing %q; got:\n%s", needle, prompt)
		}
	}
}

func TestBuildPrompt_RepoStyleCapsSubjectCount(t *testing.T) {
	subjects := make([]string, RepoStyleSubjectCount+10)
	for i := range subjects {
		subjects[i] = "subject-" + strings.Repeat("x", i%5)
	}
	prompt := BuildPrompt("A\tx", "+++", "main", StyleGuidance{Kind: StyleRepo, RecentSubjects: subjects})
	if got := strings.Count(prompt, "  * "); got != RepoStyleSubjectCount {
		t.Errorf("embedded %d subjects, want %d", got, RepoStyleSubjectCount)
	}
}

// ---- DecodeClaude ----

func TestDecodeClaude_ExtractsStructuredOutput(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, subject, body string
	}{
		{"after a session line", `{"session_id":"abc"}
{"structured_output":{"subject":"Add login flow","body":"Supports SSO."}}`, "Add login flow", "Supports SSO."},
		{"multiline envelope", `first log line
{"structured_output":{"subject":"Fix race in readLoop","body":""}}
`, "Fix race in readLoop", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subject, body, err := DecodeClaude([]byte(tc.stdout))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if subject != tc.subject {
				t.Errorf("subject: got %q, want %q", subject, tc.subject)
			}
			if body != tc.body {
				t.Errorf("body: got %q, want %q", body, tc.body)
			}
		})
	}
}

func TestDecodeClaude_RejectsWhatItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout []byte
	}{
		{"empty output", nil},
		{"whitespace-only output", []byte("\n\n\n")},
		{"missing subject", []byte(`{"structured_output":{"body":"orphaned body"}}`)},
		{"malformed JSON", []byte("not json")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := DecodeClaude(tc.stdout); err == nil {
				t.Errorf("DecodeClaude(%q) returned no error", tc.stdout)
			}
		})
	}
}

// ---- CodexSchemaJSON ----

func TestCodexSchemaJSON_IsValid(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(CodexSchemaJSON), &parsed); err != nil {
		t.Fatalf("CodexSchemaJSON invalid: %v", err)
	}
	if parsed["type"] != "object" {
		t.Errorf("type = %v, want object", parsed["type"])
	}
	props, ok := parsed["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or wrong shape: %v", parsed["properties"])
	}
	if _, ok := props["subject"]; !ok {
		t.Errorf("properties.subject missing")
	}
	if _, ok := props["body"]; !ok {
		t.Errorf("properties.body missing")
	}
}
