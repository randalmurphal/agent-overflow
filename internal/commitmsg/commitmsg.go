// Package commitmsg owns the pure prompt-builder, decoder, and
// sanitisers behind the "generate a commit message" provider call.
//
// The App-coupled glue (workspace resolution, settings routing, CLI
// invocation) stays in `internal/app` — this package only knows how
// to assemble the prompt, parse the structured output, and trim the
// model's response into a well-formed subject + body.
package commitmsg

import (
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/textgen"
)

// Timeout bounds how long the caller should wait for ONE provider CLI
// attempt to draft a message. Matches t3-code's 180s — commit-message
// generation is a small, structured JSON request; anything slower than
// three minutes is a misconfiguration the user should see as an error,
// not a hang. It is a PER-ATTEMPT budget: runTextGenWithFallback hands
// each attempt a fresh one, so a fallback to the alternate provider is
// not left with the remainder of a primary that timed out.
const Timeout = 180 * time.Second

// Per-section caps applied at the prompt-construction layer. Mirrors
// t3-code's Prompts.ts (6k for the summary, 40k for the patch). The
// gather layer in internal/git applies larger caps first, so these can
// trim further without re-reading the repo.
const (
	PromptStagedSummaryLimit = 6_000
	PromptStagedPatchLimit   = 40_000
	// PromptCustomStyleLimit caps the user's free-text style
	// instructions — guidance, not payload, so it never crowds out
	// the diff sections.
	PromptCustomStyleLimit = 2_000
)

// Style kinds, mirroring settings.CommitMessageStyle values. The
// settings layer validates the enum; this package treats anything
// unrecognized as StyleConventional so a stale settings file can't
// produce a guidance-free prompt.
const (
	StyleConventional = "conventional"
	StyleCustom       = "custom"
	StyleRepo         = "repo"
)

// RepoStyleSubjectCount is how many recent commit subjects the repo
// style samples as examples. Matches t3-code's source-control writing
// config (git log -n 20 --no-merges).
const RepoStyleSubjectCount = 20

// StyleGuidance describes how a generated message should be phrased.
// Kind selects the strategy; Custom carries the user's instructions
// for StyleCustom, RecentSubjects the sampled history for StyleRepo.
// A kind whose payload is empty (blank instructions, no history yet)
// falls back to the conventional guidance rather than emitting an
// empty section.
type StyleGuidance struct {
	Kind           string
	Custom         string
	RecentSubjects []string
}

// lines renders the guidance as prompt rule lines.
func (g StyleGuidance) lines() []string {
	switch g.Kind {
	case StyleCustom:
		custom := strings.TrimSpace(g.Custom)
		if custom == "" {
			break
		}
		return []string{
			"- follow these commit message style instructions from the user:",
			textgen.LimitPromptSection(custom, PromptCustomStyleLimit),
		}
	case StyleRepo:
		if len(g.RecentSubjects) == 0 {
			break
		}
		out := []string{"- match the style of these recent commit subjects from this repository:"}
		for i, subject := range g.RecentSubjects {
			if i >= RepoStyleSubjectCount {
				break
			}
			out = append(out, "  * "+subject)
		}
		return out
	}
	return []string{
		"- use Conventional Commits format: type(scope): summary" +
			" (types: feat, fix, refactor, docs, test, chore, perf, build, ci)",
	}
}

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
// provider CLI. The base rules and section shape match t3-code's
// Prompts.ts; the style rule appended after them mirrors t3-code's
// source-control writing-style configuration.
func BuildPrompt(summary, patch, branch string, style StyleGuidance) string {
	if strings.TrimSpace(branch) == "" {
		branch = "(detached)"
	}
	lines := []string{
		"You write concise git commit messages.",
		"Return a JSON object with keys: subject, body.",
		"Rules:",
		"- subject must be imperative, <= 72 chars, and no trailing period",
		"- body can be empty string or short bullet points",
		"- capture the primary user-visible or developer-visible change",
	}
	lines = append(lines, style.lines()...)
	lines = append(lines,
		"",
		"Branch: "+branch,
		"",
		"Staged files:",
		textgen.LimitPromptSection(summary, PromptStagedSummaryLimit),
		"",
		"Staged patch:",
		textgen.LimitPromptSection(patch, PromptStagedPatchLimit),
	)
	return strings.Join(lines, "\n")
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
