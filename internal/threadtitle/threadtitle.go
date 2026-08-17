// Package threadtitle owns the pure prompt-builders, thread-context
// formatter, decoder, and sanitisers behind the thread title flow
// (first-turn generation and user-triggered regeneration).
//
// The App-coupled glue — workspace resolution, settings routing, image
// attachment plumbing, the store read behind the regeneration context,
// and `store.UpdateTitleIfCurrent` — stays in `app_thread_title.go`.
// This package only knows how to assemble the prompt, parse the
// structured output, and trim the model's response into a well-formed
// title.
//
// The prompt text is adapted from t3-code's `TextGenerationPrompts.ts`
// (INITIAL_THREAD_TITLE_PROMPT / the regeneration prompt), with one
// deliberate divergence: t3's tool-use line is replaced by an explicit
// no-tools rule. AO runs the Claude leg under `--safe-mode` without
// `--dangerously-skip-permissions`, so a tool call would be denied and
// only waste turns.
package threadtitle

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/textgen"
)

// Default is the sentinel a fresh thread starts with and what
// `Sanitize` falls back to when the model returns nothing usable.
// Callers compare-and-swap with this value via
// `store.UpdateTitleIfCurrent` so a manually-renamed thread doesn't get
// clobbered by a late-arriving auto-title.
const Default = "New Thread"

// Timeout bounds how long the caller should wait for ONE provider CLI
// attempt to draft a title. 3 minutes matches t3-code's budget — title
// generation is a small request, but image-attached prompts can take
// longer than the bare-text path. It is a PER-ATTEMPT budget: a
// fallback to the alternate provider starts a fresh one, because a
// primary that burned the whole budget timing out would otherwise leave
// the fallback no time to answer (incident 2026-08-16).
const Timeout = 3 * time.Minute

// MaxRunes caps the generated title at 50 runes (with a 3-rune ellipsis
// suffix when truncated). Keeps the sidebar entry on one line on the
// narrowest supported window width.
const MaxRunes = 50

// CodexSchemaJSON is the JSON schema passed to `codex exec
// --output-schema`. Deliberately carries NO maxLength: length is
// enforced by the prompt's editorial rules and by Sanitize's MaxRunes
// cap, and a strict-schema rejection of a 51-character draft loses the
// title entirely rather than trimming it.
const CodexSchemaJSON = `{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}`

// ClaudeSchemaJSON mirrors the Codex schema for the Claude CLI. Kept as
// a separate constant so the two can diverge if escaping rules change.
const ClaudeSchemaJSON = `{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`

// initialInstructions is the instruction block of the first-turn
// prompt. Everything after it is data: the user's message, then the
// attachment metadata section when there is one.
const initialInstructions = `Generate a title that will help the user recognize this thread weeks later.
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
- Do not use tools; derive the title only from the content provided. When a URL or attachment is the only source of the subject, remain accurate about what is known rather than guessing.`

// regenerateInstructions is the instruction block of the regeneration
// prompt, with %s carrying the quoted previous title. t3's "use
// attached images" rule is deliberately absent: the regeneration path
// passes no images, only the formatted thread context.
const regenerateInstructions = `Regenerate the title for an existing thread so the user can recognize it weeks later.
The previous title was %s.
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
- A QR-sharing overhaul that ends with CI and merge work remains about QR sharing, not the PR lifecycle.`

// BuildPrompt assembles the first-turn instruction sent to the provider
// CLI: the editorial rules, the user's message, and — when the send
// carried attachments — their metadata. Both data sections are budgeted
// through textgen.LimitPromptSection.
func BuildPrompt(message string, attachments []store.Attachment) string {
	prompt := []string{
		initialInstructions,
		"",
		"User message:",
		textgen.LimitPromptSection(message, 8_000),
	}

	if len(attachments) > 0 {
		prompt = append(prompt, "", "Attachment metadata:", textgen.LimitPromptSection(FormatAttachments(attachments), 4_000))
	}

	return strings.Join(prompt, "\n")
}

// BuildRegeneratePrompt assembles the instruction for re-titling a
// thread the user asked to regenerate. threadContents is the output of
// FormatThreadContext — already windowed and budgeted, so it is
// appended verbatim.
func BuildRegeneratePrompt(previousTitle, threadContents string) string {
	return fmt.Sprintf(regenerateInstructions, strconv.Quote(previousTitle)) +
		"\n\nThread contents:\n" + threadContents
}

// FormatAttachments renders attachment metadata for the prompt's
// "Attachment metadata:" section. One bullet per attachment with
// filename, MIME type, and byte size.
func FormatAttachments(attachments []store.Attachment) string {
	lines := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		lines = append(lines, fmt.Sprintf("- %s (%s, %d bytes)", attachment.Filename, attachment.MimeType, attachment.Size))
	}
	return strings.Join(lines, "\n")
}

// DecodeClaude pulls the title out of a Claude JSON response. Wraps
// the generic last-line decoder with the title envelope shape.
func DecodeClaude(stdout []byte) (string, error) {
	payload, err := textgen.DecodeClaudeStructuredLastLine[struct {
		Title string `json:"title"`
	}](stdout)
	if err != nil {
		return "", err
	}
	return payload.Title, nil
}

// Sanitize trims the model's output into a well-formed title: single
// line, no quotes, no surrounding whitespace, internal whitespace
// collapsed, capped at MaxRunes with an ellipsis if truncated. Returns
// the Default sentinel when the model returns nothing usable so the
// compare-and-swap in `UpdateTitleIfCurrent` skips the write.
func Sanitize(raw string) string {
	out := textgen.NormalizeStructuredOutputLine(raw)
	if out == "" {
		return Default
	}
	return textgen.CapRunesWithEllipsis(out, MaxRunes)
}
