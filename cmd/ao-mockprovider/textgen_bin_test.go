package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/commitmsg"
	"agent-overflow/internal/textgen"
	"agent-overflow/internal/threadtitle"
)

// The one-shot text-generation mode is driven through the REAL argv builders
// (textgen.RunClaude / textgen.RunCodex) and its answer is read back by the
// REAL decoders the app uses. Nothing here restates the wire shape: if the mock
// and the production reader ever disagree about where the structured answer
// lives, these tests are what says so.
//
// The binary is the one TestMain built; textgen.ExecCLI is the production
// executor, so the argv, the environment rule, the stdin pipe, and the exit-code
// handling are all the real ones.

const textGenTimeout = 30 * time.Second

// workflowDigestSchema is a representative strict closed object in the
// shape app_workflow_digest.go sends to BOTH providers. It is deliberately
// NOT a mirror of that main-package constant (unimportable from here, and a
// copy would go stale silently): the mock generates its answer FROM whatever
// schema arrives, so the property under test — the Claude path accepts a
// strict closed object, not only the loose Claude-specific ones — holds for
// the production schema by construction, whatever its fields become.
const workflowDigestSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "whatHappened": {"type": "string"},
    "whatItNeeds": {"type": "string"}
  },
  "required": ["whatHappened", "whatItNeeds"]
}`

func textGenConfig() textgen.Config {
	return textgen.Config{
		Binary: mockBin,
		Model:  "mock-model",
		Exec:   textgen.ExecCLI,
	}
}

// TestClaudeTextGenThreadTitle is the regression this whole mode exists for:
// before it, `claude -p` fell through to the NDJSON session adapter, which
// answers a bare prompt with nothing, and every harness thread's generated
// title failed as "claude returned empty output".
func TestClaudeTextGenThreadTitle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), textGenTimeout)
	defer cancel()

	stdout, err := textgen.RunClaude(
		ctx, textGenConfig(), t.TempDir(), threadtitle.ClaudeSchemaJSON,
		nil, "Summarize this thread.", threadtitle.Timeout,
	)
	if err != nil {
		t.Fatalf("RunClaude: %v", err)
	}
	title, err := threadtitle.DecodeClaude(stdout)
	if err != nil {
		t.Fatalf("threadtitle.DecodeClaude(%q): %v", stdout, err)
	}
	if strings.TrimSpace(title) == "" {
		t.Fatalf("decoded an empty title from %q", stdout)
	}
}

// TestClaudeTextGenCommitMessage covers the second Claude-only schema. Its
// `required` lists only `subject` (an empty body is legal), which the
// two-provider union in providerschema.Validate would reject — the mock must
// judge it by Claude's rules alone or every real commit-message run would die
// at spawn.
func TestClaudeTextGenCommitMessage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), textGenTimeout)
	defer cancel()

	// The real caller appends this flag; carrying it here keeps the sniff
	// honest about argv it will actually see.
	extra := []string{"--dangerously-skip-permissions"}
	stdout, err := textgen.RunClaude(
		ctx, textGenConfig(), t.TempDir(), commitmsg.ClaudeSchemaJSON,
		extra, "Write a commit message.", commitmsg.Timeout,
	)
	if err != nil {
		t.Fatalf("RunClaude: %v", err)
	}
	subject, body, err := commitmsg.DecodeClaude(stdout)
	if err != nil {
		t.Fatalf("commitmsg.DecodeClaude(%q): %v", stdout, err)
	}
	if strings.TrimSpace(subject) == "" {
		t.Fatalf("decoded an empty subject from %q", stdout)
	}
	if strings.TrimSpace(body) == "" {
		t.Fatalf("decoded an empty body from %q", stdout)
	}
}

// TestClaudeTextGenWorkflowDigest runs the shared strict schema through the
// Claude path and the same generic decoder app_workflow_digest.go uses.
func TestClaudeTextGenWorkflowDigest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), textGenTimeout)
	defer cancel()

	stdout, err := textgen.RunClaude(
		ctx, textGenConfig(), t.TempDir(), workflowDigestSchema,
		nil, "Digest this run.", textGenTimeout,
	)
	if err != nil {
		t.Fatalf("RunClaude: %v", err)
	}
	digest, err := textgen.DecodeClaudeStructuredLastLine[struct {
		WhatHappened string `json:"whatHappened"`
		WhatItNeeds  string `json:"whatItNeeds"`
	}](stdout)
	if err != nil {
		t.Fatalf("DecodeClaudeStructuredLastLine(%q): %v", stdout, err)
	}
	if digest.WhatHappened == "" || digest.WhatItNeeds == "" {
		t.Fatalf("digest fields empty: %+v (stdout %q)", digest, stdout)
	}
}

// TestClaudeTextGenRejectsAnIllegalSchema pins the strictness doctrine on the
// new path: an unknown keyword is a Claude strict-mode rejection, and the mock
// must fail the run the way the real CLI does rather than answer anyway.
func TestClaudeTextGenRejectsAnIllegalSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), textGenTimeout)
	defer cancel()

	illegal := `{"type":"object","properties":{"title":{"type":"string","multiline":true}},"required":["title"]}`
	_, err := textgen.RunClaude(
		ctx, textGenConfig(), t.TempDir(), illegal,
		nil, "Summarize this thread.", textGenTimeout,
	)
	if err == nil {
		t.Fatal("expected the mock to refuse a schema outside the shared vocabulary")
	}
	if !strings.Contains(err.Error(), "multiline") {
		t.Fatalf("rejection should name the broken keyword, got %v", err)
	}
}

// TestCodexTextGenCommitMessage covers the argv shape that used to be sniffed
// as Claude entirely: `codex exec` carries no `app-server` marker, so before the
// dedicated branch the Codex textgen spawn ran the Claude NDJSON adapter and
// never wrote the output file at all.
func TestCodexTextGenCommitMessage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), textGenTimeout)
	defer cancel()

	raw, err := textgen.RunCodex(
		ctx, textGenConfig(), t.TempDir(), commitmsg.CodexSchemaJSON,
		nil, "Write a commit message.", commitmsg.Timeout,
	)
	if err != nil {
		t.Fatalf("RunCodex: %v", err)
	}
	// The app's own decode (app_commit_message.go): plain JSON out of the
	// --output-last-message file, no envelope.
	var parsed struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode codex structured output %q: %v", raw, err)
	}
	if strings.TrimSpace(parsed.Subject) == "" {
		t.Fatalf("codex answered no subject: %q", raw)
	}
}

// TestCodexTextGenThreadTitle covers the second Codex schema and, with the
// commit-message case above, both closed-object schemas the app sends.
func TestCodexTextGenThreadTitle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), textGenTimeout)
	defer cancel()

	raw, err := textgen.RunCodex(
		ctx, textGenConfig(), t.TempDir(), threadtitle.CodexSchemaJSON,
		nil, "Summarize this thread.", threadtitle.Timeout,
	)
	if err != nil {
		t.Fatalf("RunCodex: %v", err)
	}
	var parsed struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode codex structured output %q: %v", raw, err)
	}
	if threadtitle.Sanitize(parsed.Title) == "" {
		t.Fatalf("codex answered no title: %q", raw)
	}
}

// TestCodexTextGenRejectsAnOpenObject pins the OTHER half of the schema
// doctrine: the Codex app-server is the strict half of the union, so an object
// without `additionalProperties: false` must fail here even though the Claude
// path accepts it.
func TestCodexTextGenRejectsAnOpenObject(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), textGenTimeout)
	defer cancel()

	_, err := textgen.RunCodex(
		ctx, textGenConfig(), t.TempDir(), threadtitle.ClaudeSchemaJSON,
		nil, "Summarize this thread.", threadtitle.Timeout,
	)
	if err == nil {
		t.Fatal("expected the mock to refuse an open object on the codex path")
	}
	if !strings.Contains(err.Error(), "additionalProperties") {
		t.Fatalf("rejection should name the broken rule, got %v", err)
	}
}

// TestTextGenInvocationSniffsAreDisjoint guards the discrimination itself: the
// probe, a streaming session, and the two one-shot shapes all reach main() as
// bare argv, and a sniff that overlapped would silently route a session into a
// one-shot answer.
func TestTextGenInvocationSniffsAreDisjoint(t *testing.T) {
	session := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
	probe := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--max-turns", "0"}
	claudeOneShot := []string{"-p", "--output-format", "json", "--json-schema", "{}", "--safe-mode"}
	codexOneShot := []string{"exec", "--ephemeral", "--ignore-user-config", "-s", "read-only", "-"}
	codexSession := []string{"app-server"}

	for _, tc := range []struct {
		name                  string
		args                  []string
		claudeGen, codexGen   bool
		wantProbeInvocationOK bool
	}{
		{"claude session", session, false, false, false},
		{"claude probe", probe, false, false, true},
		{"claude one-shot", claudeOneShot, true, false, false},
		{"codex one-shot", codexOneShot, false, true, false},
		{"codex app-server", codexSession, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isClaudeTextGenInvocation(tc.args); got != tc.claudeGen {
				t.Errorf("isClaudeTextGenInvocation = %v, want %v", got, tc.claudeGen)
			}
			if got := isCodexTextGenInvocation(tc.args); got != tc.codexGen {
				t.Errorf("isCodexTextGenInvocation = %v, want %v", got, tc.codexGen)
			}
			if got := isClaudeProbeInvocation(tc.args); got != tc.wantProbeInvocationOK {
				t.Errorf("isClaudeProbeInvocation = %v, want %v", got, tc.wantProbeInvocationOK)
			}
		})
	}
}
