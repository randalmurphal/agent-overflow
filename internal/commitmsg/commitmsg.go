// Package commitmsg owns the pure prompt-builder, decoder, and
// sanitisers behind the "generate a commit message" provider call.
//
// The App-coupled glue (workspace resolution, settings routing, CLI
// invocation) stays in the main package — this package only knows how
// to assemble the prompt, parse the structured output, and trim the
// model's response into a well-formed subject + body.
package commitmsg

import (
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/textgen"
)

// Timeout bounds how long the caller should wait for a provider CLI to
// draft a message. Matches t3-code's 180s — commit-message generation
// is a small, structured JSON request; anything slower than three
// minutes is a misconfiguration the user should see as an error, not a
// hang.
const Timeout = 180 * time.Second

// Per-section caps applied at the prompt-construction layer. Mirrors
// t3-code's Prompts.ts (6k for the summary, 40k for the patch). The
// gather layer in internal/git applies larger caps first, so these can
// trim further without re-reading the repo.
const (
	PromptStagedSummaryLimit = 6_000
	PromptStagedPatchLimit   = 40_000
)

// CodexSchemaJSON is the JSON schema passed to `codex exec
// --output-schema`. Matches t3-code's BuildPrompt output schema:
// required subject+body, no extra keys.
const CodexSchemaJSON = `{` +
	`"type":"object",` +
	`"properties":{` +
	`"subject":{"type":"string"},` +
	`"body":{"type":"string"}` +
	`},` +
	`"required":["subject","body"],` +
	`"additionalProperties":false` +
	`}`

// ClaudeSchemaJSON mirrors the Codex schema but is inlined separately
// because the Claude CLI escapes the schema slightly differently on
// the command line. Keeping them as distinct constants means a future
// divergence (e.g. adding a `branch` key for the branch-name flow)
// won't require re-parsing the one shared string.
const ClaudeSchemaJSON = `{"type":"object","properties":{"subject":{"type":"string"},"body":{"type":"string"}},"required":["subject"]}`

// BuildPrompt assembles the natural-language instruction sent to the
// provider CLI. Matches t3-code's Prompts.ts line-for-line so the two
// apps produce identical output shape for identical input.
func BuildPrompt(summary, patch, branch string) string {
	if strings.TrimSpace(branch) == "" {
		branch = "(detached)"
	}
	return strings.Join([]string{
		"You write concise git commit messages.",
		"Return a JSON object with keys: subject, body.",
		"Rules:",
		"- subject must be imperative, <= 72 chars, and no trailing period",
		"- body can be empty string or short bullet points",
		"- capture the primary user-visible or developer-visible change",
		"",
		"Branch: " + branch,
		"",
		"Staged files:",
		textgen.LimitPromptSection(summary, PromptStagedSummaryLimit),
		"",
		"Staged patch:",
		textgen.LimitPromptSection(patch, PromptStagedPatchLimit),
	}, "\n")
}

// DecodeClaude pulls the structured output out of a Claude JSON
// response. Wraps the generic last-line decoder with the commit
// envelope shape and the subject-required validation.
func DecodeClaude(stdout []byte) (subject, body string, err error) {
	payload, err := textgen.DecodeClaudeStructuredLastLine[struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}](stdout)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(payload.Subject) == "" {
		return "", "", fmt.Errorf("claude returned an empty subject")
	}
	return payload.Subject, payload.Body, nil
}

// SanitizeSubject trims the model's output into a well-formed subject
// line: single line, stripped quotes, no trailing period, capped at 72
// characters. Returns an empty string when the model returns nothing
// usable so the dialog never opens with a blank subject — the user
// can overwrite it.
func SanitizeSubject(raw string) string {
	out := strings.TrimSuffix(textgen.NormalizeStructuredOutputLine(raw), ".")
	if out == "" {
		return ""
	}
	return textgen.CapRunesWithEllipsis(out, 72)
}

// SanitizeBody trims the model's body output without imposing a
// character limit — git has no body length limit and the user can
// always edit. Whitespace normalization: collapse runs of 3+ newlines
// down to 2, strip leading/trailing whitespace.
func SanitizeBody(raw string) string {
	out := strings.TrimSpace(raw)
	if out == "" {
		return ""
	}
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return out
}
