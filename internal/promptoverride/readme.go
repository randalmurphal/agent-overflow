package promptoverride

import (
	"fmt"
	"strings"

	"agent-overflow/internal/settings"
)

// ReadmeMarkdown is the reference the app writes to
// <configDir>/prompts/README.md at every boot, so a user can point an agent
// at the directory and the agent can edit a prompt correctly. Generated from
// Placeholders and the limits, so it cannot drift from what the app does.
func ReadmeMarkdown() string {
	var b strings.Builder
	b.WriteString(`# System prompt override files

Agent Overflow writes this file at every start. Edits to it are replaced.

This directory holds the text of the system prompt overrides configured in
Agent Overflow's Settings, one file per override.

## Files and Settings

- Each override's text is the file ` + "`<id>.md`" + `, where ` + "`<id>`" + ` is the
  override's id. The file holds the prompt and nothing else.
- Settings holds the rest of each override: whether it is enabled and which
  models it applies to. Adding an override in Settings creates its empty
  file here; removing it deletes the file. Edit the text here.
- Claude and Claude TUI sessions share one list of overrides; Codex has its
  own. A session uses the first enabled override in its provider's list
  whose models include the session's model, in place of the provider's
  built-in system prompt.
- An empty file, or one over a limit below, is not applied, and Settings
  says so. A ` + "`.md`" + ` file no override names is ignored.

## Placeholders

These are replaced with their value when a session starts. Any other
` + "`{{...}}`" + ` text is left exactly as written.

| Placeholder | Meaning | Providers |
|---|---|---|
`)
	for _, placeholder := range Placeholders {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n",
			placeholder.Token, placeholder.Meaning, strings.Join(placeholder.Providers, ", "))
	}
	fmt.Fprintf(&b, `
A placeholder renders empty for a provider not listed.

## Limits

- One file: %d bytes of text, not counting leading and trailing whitespace.
- One provider's files together: %d bytes.
- The prompt after placeholders are replaced: %d bytes. A session whose
  prompt renders larger does not start.

## When an edit applies

A change applies when a thread's session next starts. A running session
keeps the prompt it started with, and its thread shows a "system prompt
changed" banner with a Restart button. Dismissed or ignored, the change
applies the next time that session restarts.
`, settings.MaxPromptOverrideLen, settings.MaxPromptOverridesTotalLen, MaxRenderedPromptBytes)
	return b.String()
}
