package threadtitle

import (
	"strings"
	"testing"

	"agent-overflow/internal/providerschema"
	"agent-overflow/internal/store"
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

// TestBuildPromptSnapshot pins the exact wire text. The prompt is the
// feature — a silent edit changes every title the app generates — so
// the test carries the whole string rather than probing for keywords.
func TestBuildPromptSnapshot(t *testing.T) {
	want := `Generate a title that will help the user recognize this thread weeks later.
Return JSON with exactly one key: title.

Before answering, silently reduce the request to:
- Subject: What system, feature, or problem is this really about?
- Outcome: What does the user ultimately want to understand or change?
- Incidental instructions: What only describes how the agent should do the work?

Title the subject and outcome. Discard incidental instructions.

Editorial rules:
- 3-8 words, fewer than 40 characters.
- Use a compact noun phrase or clear action phrase.
- Capture the umbrella goal when the request lists several symptoms or steps.
- Name the product change, not the mock, plan, report, branch, or PR used to produce it.
- Models, subagents, tools, output formats, and monitoring instructions do not belong in the title unless they are themselves the topic.
- For reviews, name what is being reviewed and the relevant concern. Avoid generic titles such as "Review PR 123" when linked or attached context reveals the subject.
- For research, name the question domain rather than the requested research process.
- Do not claim the work is complete.
- Do not copy and truncate the user's message.
- Avoid project names already visible in the UI, quotes, labels, filler, and trailing punctuation.
- Use attached images as primary context for UI issues.
- Do not use tools; derive the title only from the content provided. When a URL or attachment is the only source of the subject, remain accurate about what is known rather than guessing.

User message:
fix the login bug`

	if got := BuildPrompt("fix the login bug", nil); got != want {
		t.Fatalf("BuildPrompt() =\n%s\n\nwant\n%s", got, want)
	}
}

func TestBuildPromptAppendsAttachmentSection(t *testing.T) {
	got := BuildPrompt("look at this", []store.Attachment{
		{Filename: "shot.png", MimeType: "image/png", Size: 12},
	})
	if !strings.HasSuffix(got, "\n\nAttachment metadata:\n- shot.png (image/png, 12 bytes)") {
		t.Fatalf("prompt missing attachment section: %q", got)
	}
	if !strings.Contains(got, "\nUser message:\nlook at this\n") {
		t.Fatalf("prompt lost the user message section: %q", got)
	}
}

// TestBuildRegeneratePromptSnapshot pins the regeneration wire text and
// the quoting of the previous title (a title carrying a quote must not
// break out of the sentence that reports it).
func TestBuildRegeneratePromptSnapshot(t *testing.T) {
	want := `Regenerate the title for an existing thread so the user can recognize it weeks later.
The previous title was "Fix \"flaky\" resume".
Return JSON with exactly one key: title.

Determine the title in this order:
1. Read the USER messages first. Identify the latest explicit durable goal. The original subject remains the subject until the user clearly changes what the thread is about.
2. Use ASSISTANT messages to resolve vague links, unnamed code, and discovered product nouns. Do not promote one assistant finding into the thread subject unless the user adopts it as a new goal.
3. Compare that subject with the previous title. Preserve accurate scope words, especially when earlier content is truncated. Replace the previous title when it is generic, artifact-based, a completion update, or contradicted by the thread.
4. Title the durable subject and desired outcome, not the current workflow state.

Editorial rules:
- 3-8 words, fewer than 40 characters.
- Use a compact noun phrase or clear action phrase.
- Preserve the umbrella subject when later messages focus on one finding, provider, platform, or implementation detail.
- A thread progressing through research, planning, implementation, review, CI, merge, and monitoring has usually not changed subjects.
- Ignore deliverables and operations such as mocks, plans, HTML, branches, PRs, tests, CI, commits, merging, and monitoring unless they are the actual topic.
- Models, subagents, tools, output formats, and monitoring instructions do not belong in the title unless they are themselves the topic.
- Treat final operational follow-ups and assistant completion summaries as weak evidence of subject.
- For reviews, name the reviewed feature or system and its durable concern, not one finding from the review.
- For research, name the question domain rather than the research process.
- Do not claim the work is complete.
- Do not copy and truncate a thread message.
- Avoid project names already visible in the UI, PR numbers, quotes, labels, filler, and trailing punctuation.
- Do not use tools; derive the title only from the content provided. When a URL or attachment is the only source of the subject, remain accurate about what is known rather than guessing.
- Return a meaningfully improved title, not a cosmetic paraphrase of the previous title.

Examples of the distinction:
- A subagent-monitoring review that finds a Codex roster bug remains "Review Subagent Monitoring Risks," not "Codex Roster Bug Review."
- A vague failing-test request later identified as a lazy thread-feed mismatch becomes "Fix Lazy Thread Feed Test," not "Prevent Mobile Feed Regressions."
- A QR-sharing overhaul that ends with CI and merge work remains about QR sharing, not the PR lifecycle.

Thread contents:
USER:
make resume stop flaking`

	got := BuildRegeneratePrompt(`Fix "flaky" resume`, "USER:\nmake resume stop flaking")
	if got != want {
		t.Fatalf("BuildRegeneratePrompt() =\n%s\n\nwant\n%s", got, want)
	}
}

// TestCodexSchemaHasNoMaxLength guards the incident fix: a strict
// schema rejecting a 51-character draft loses the title entirely, so
// length is enforced by the prompt and Sanitize instead.
//
// The schema is also checked against the rules both CLIs enforce, since
// a rejected schema fails the whole run before any work happens. Only
// the Codex constant: ClaudeSchemaJSON deliberately omits
// `additionalProperties: false`, which providerschema requires because
// Codex does — Claude does not, and the two constants exist precisely so
// they can differ.
func TestCodexSchemaHasNoMaxLength(t *testing.T) {
	if strings.Contains(CodexSchemaJSON, "maxLength") {
		t.Fatalf("CodexSchemaJSON must not constrain length: %s", CodexSchemaJSON)
	}
	for _, violation := range providerschema.Validate([]byte(CodexSchemaJSON)) {
		t.Errorf("CodexSchemaJSON violates a provider schema rule: %s", violation.Error())
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

