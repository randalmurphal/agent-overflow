// Package threadtitle owns the pure prompt-builder, decoder, and
// sanitisers behind the auto-generated thread title flow.
//
// The App-coupled glue — workspace resolution, settings routing, image
// attachment plumbing, and `store.UpdateTitleIfCurrent` — stays in
// `app_thread_title.go`. This package only knows how to assemble the
// prompt, parse the structured output, and trim the model's response
// into a well-formed title.
package threadtitle

import (
	"fmt"
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

// Timeout bounds how long the caller should wait for a provider CLI to
// draft a title. 3 minutes matches t3-code's budget — title generation
// is a small request, but image-attached prompts can take longer than
// the bare-text path.
const Timeout = 3 * time.Minute

// MaxRunes caps the generated title at 50 runes (with a 3-rune ellipsis
// suffix when truncated). Keeps the sidebar entry on one line on the
// narrowest supported window width.
const MaxRunes = 50

// CodexSchemaJSON is the JSON schema passed to `codex exec
// --output-schema`. Requires a 50-char title; matches t3-code line for
// line so both apps produce comparable output.
const CodexSchemaJSON = `{"type":"object","properties":{"title":{"type":"string","maxLength":50}},"required":["title"],"additionalProperties":false}`

// ClaudeSchemaJSON mirrors the Codex schema for the Claude CLI. Kept as
// a separate constant so the two can diverge if escaping rules change.
const ClaudeSchemaJSON = `{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`

// BuildPrompt assembles the natural-language instruction sent to the
// provider CLI. Mirrors t3-code's Prompts.ts.
func BuildPrompt(message string, attachments []store.Attachment) string {
	prompt := []string{
		"You write concise thread titles for coding conversations.",
		"Return a JSON object with key: title.",
		"Rules:",
		"- Title should summarize the user's request, not restate it verbatim.",
		"- Keep it short and specific (3-8 words).",
		"- Avoid quotes, filler, prefixes, and trailing punctuation.",
		"- If images are attached, use them as primary context for visual/UI issues.",
		"",
		"User message:",
		textgen.LimitPromptSection(message, 8_000),
	}

	if len(attachments) > 0 {
		prompt = append(prompt, "", "Attachment metadata:", textgen.LimitPromptSection(FormatAttachments(attachments), 4_000))
	}

	return strings.Join(prompt, "\n")
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

// RedactError takes a raw error from the title-generation flow and
// turns CLI-failure messages into a stable opaque string the log can
// emit without leaking the user's prompt or environment. Non-CLI
// errors pass through.
func RedactError(err error) string {
	message := err.Error()
	if strings.Contains(message, "CLI failed") {
		return "provider CLI failed"
	}
	return message
}
