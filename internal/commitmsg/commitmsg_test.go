package commitmsg

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---- SanitizeSubject ----

func TestSanitizeSubject_StripsQuotesAndPunctuation(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`"Add login flow."`, `Add login flow`},
		{"'Fix typo'", `Fix typo`},
		{"`Refactor auth`", `Refactor auth`},
		{`Add login flow.`, `Add login flow`},
		{`  Leading and trailing  `, `Leading and trailing`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := SanitizeSubject(tc.in); got != tc.want {
				t.Errorf("SanitizeSubject(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeSubject_KeepsOnlyFirstLine(t *testing.T) {
	in := "Add login flow\nExtra body line that should not leak into subject"
	want := "Add login flow"
	if got := SanitizeSubject(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeSubject_CollapsesInternalWhitespace(t *testing.T) {
	in := "Add    login\t flow"
	want := "Add login flow"
	if got := SanitizeSubject(in); got != want {
		t.Errorf("got %q, want %q", got, want)
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

func TestSanitizeSubject_EmptyInputYieldsEmpty(t *testing.T) {
	for _, s := range []string{"", "   ", "\n\n", `""`} {
		if got := SanitizeSubject(s); got != "" {
			t.Errorf("SanitizeSubject(%q) = %q, want empty", s, got)
		}
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

func TestSanitizeBody_CollapsesBlankRuns(t *testing.T) {
	in := "First paragraph.\n\n\n\nSecond paragraph."
	want := "First paragraph.\n\nSecond paragraph."
	if got := SanitizeBody(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeBody_EmptyIsEmpty(t *testing.T) {
	if got := SanitizeBody("   \n\n "); got != "" {
		t.Errorf("expected empty for whitespace-only body, got %q", got)
	}
}

func TestSanitizeBody_PreservesSingleNewlines(t *testing.T) {
	in := "Line one.\nLine two.\n\nParagraph break."
	if got := SanitizeBody(in); got != in {
		t.Errorf("expected no change, got %q", got)
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
	stdout := []byte(`{"session_id":"abc"}
{"structured_output":{"subject":"Add login flow","body":"Supports SSO."}}`)
	subject, body, err := DecodeClaude(stdout)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if subject != "Add login flow" {
		t.Errorf("subject: got %q", subject)
	}
	if body != "Supports SSO." {
		t.Errorf("body: got %q", body)
	}
}

func TestDecodeClaude_EmptyOutputErrors(t *testing.T) {
	if _, _, err := DecodeClaude(nil); err == nil {
		t.Error("expected error for empty output")
	}
	if _, _, err := DecodeClaude([]byte("\n\n\n")); err == nil {
		t.Error("expected error for whitespace-only output")
	}
}

func TestDecodeClaude_MissingSubjectErrors(t *testing.T) {
	stdout := []byte(`{"structured_output":{"body":"orphaned body"}}`)
	if _, _, err := DecodeClaude(stdout); err == nil {
		t.Error("expected error when subject is missing")
	}
}

func TestDecodeClaude_MalformedJSONErrors(t *testing.T) {
	if _, _, err := DecodeClaude([]byte("not json")); err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestDecodeClaude_HandlesMultilineEnvelope(t *testing.T) {
	stdout := []byte(`first log line
{"structured_output":{"subject":"Fix race in readLoop","body":""}}
`)
	subject, body, err := DecodeClaude(stdout)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if subject != "Fix race in readLoop" {
		t.Errorf("subject: got %q", subject)
	}
	if body != "" {
		t.Errorf("body: got %q, want empty", body)
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
