package aocli

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/workflow/memory"
)

// `agent-overflow memory …`.

func TestMemoryAddSendsTheDraftAndNothingElse(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentAddMemory", map[string]any{
		"itemId": "lane", "rootId": "root", "kind": "warning", "wave": 2,
		"path": "/data/workflow-memory/root/notes.ndjson",
	})
	code, stdout, stderr := runCLI([]string{
		"memory", "add", "--kind", "warning", "the mock server drops the first frame",
		"--file", "internal/a.go", "--file", "internal/b.go",
	}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "recorded=warning") || !strings.Contains(stdout, "wave=2") ||
		!strings.Contains(stdout, "campaign=root") {
		t.Fatalf("output = %q", stdout)
	}
	calls := backend.recorded("WorkflowAgentAddMemory")
	if len(calls) != 1 {
		t.Fatalf("calls = %#v", calls)
	}
	var sent map[string]any
	if err := json.Unmarshal(calls[0].Params[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent["kind"] != "warning" || sent["text"] != "the mock server drops the first frame" {
		t.Fatalf("sent = %#v", sent)
	}
	files, _ := sent["files"].([]any)
	if len(files) != 2 || files[1] != "internal/b.go" {
		t.Fatalf("sent files = %#v", sent["files"])
	}
	// The wire shape has no provenance slot at all: the app stamps it, and a
	// CLI that could send one is a CLI that could be told to lie.
	for _, forbidden := range []string{"provenance", "at", "wave", "runId", "phaseId"} {
		if _, present := sent[forbidden]; present {
			t.Fatalf("the add wire carries %q: %#v", forbidden, sent)
		}
	}
}

// The kind is checked before the round trip: a typo is a usage error the caller
// fixes on the spot, and the usage string already answered the question.
func TestMemoryAddRefusesAKindOutsideTheVocabulary(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentAddMemory", map[string]any{"rootId": "root"})
	for _, kind := range []string{"", "insight", "Warning", "ruling"} {
		args := []string{"memory", "add", "a note"}
		if kind != "" {
			args = append(args, "--kind", kind)
		}
		code, _, stderr := runCLI(args, backend.env())
		if code != exitError {
			t.Fatalf("kind %q exit = %d", kind, code)
		}
		if !strings.Contains(stderr, memory.KindList()) {
			t.Fatalf("kind %q stderr does not name the vocabulary: %q", kind, stderr)
		}
	}
	if calls := backend.recorded("WorkflowAgentAddMemory"); len(calls) != 0 {
		t.Fatalf("a refused kind still reached the backend: %#v", calls)
	}
	// A note with no text is the same class of mistake.
	code, _, stderr := runCLI([]string{"memory", "add", "--kind", "warning"}, backend.env())
	if code != exitError || !strings.Contains(stderr, "exactly one note") {
		t.Fatalf("empty note exit = %d, stderr = %q", code, stderr)
	}
}

// `list` checks the vocabulary locally too. Shipping a typo and learning the
// kinds from a wire refusal is the round trip the usage string already answered
// — and the surface is one claim, not two: a caller who learned it on `add`
// would not expect `list` to answer differently.
func TestMemoryListRefusesAKindOutsideTheVocabularyWithoutARoundTrip(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentListMemory", map[string]any{"rootId": "root", "path": "/log.ndjson"})
	for _, kind := range []string{"insight", "Warning", "ruling"} {
		code, _, stderr := runCLI([]string{"memory", "list", "--kind", kind}, backend.env())
		if code != exitError {
			t.Fatalf("kind %q exit = %d", kind, code)
		}
		if !strings.Contains(stderr, memory.KindList()) {
			t.Fatalf("kind %q stderr does not name the vocabulary: %q", kind, stderr)
		}
	}
	if calls := backend.recorded("WorkflowAgentListMemory"); len(calls) != 0 {
		t.Fatalf("a refused kind still reached the backend: %#v", calls)
	}
	// An EMPTY --kind is not a kind: it is the unfiltered read, and it must stay
	// legal or `memory list` itself would be refused.
	if code, _, stderr := runCLI([]string{"memory", "list", "--kind", "  "}, backend.env()); code != exitOK {
		t.Fatalf("an unfiltered read was refused: exit = %d, stderr = %q", code, stderr)
	}
}

func TestMemoryListRendersProvenanceAndSaysWhatItRead(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentListMemory", map[string]any{
		"itemId": "lane", "rootId": "root", "path": "/data/workflow-memory/root/notes.ndjson",
		"total": 3, "skipped": 1,
		"notes": []any{
			map[string]any{
				"kind": "handoff", "text": "the integration branch is at abc123",
				"at": 10, "provenance": map[string]any{
					"runId": "root", "phaseId": "plan", "attempt": 1, "wave": 0},
			},
			map[string]any{
				"kind": "warning", "text": "gofmt rewrites the generated file",
				"files": []any{"internal/a.go"}, "at": 20,
				"provenance": map[string]any{
					"runId": "lane", "phaseId": "review", "unitId": "lens-a", "attempt": 2, "wave": 2},
			},
		},
	})
	code, stdout, stderr := runCLI([]string{"memory", "list"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "campaign=root notes=2") || !strings.Contains(stdout, "unreadable-lines=1") {
		t.Fatalf("header = %q", stdout)
	}
	if !strings.Contains(stdout, "log=/data/workflow-memory/root/notes.ndjson") {
		t.Fatalf("output does not name the log: %q", stdout)
	}
	if !strings.Contains(stdout, "[wave 0 ") || !strings.Contains(stdout, `review/lens-a.2`) {
		t.Fatalf("output lost its provenance: %q", stdout)
	}
	if !strings.Contains(stdout, "internal/a.go") {
		t.Fatalf("output lost its cited paths: %q", stdout)
	}
	// Model-authored prose is quoted, exactly as run inspect's output values are.
	if strings.Contains(stdout, "gofmt rewrites the generated file\n") {
		t.Fatalf("a note was printed unquoted: %q", stdout)
	}
}

func TestMemoryListFiltersAndSaysSoAndPrintsAnEmptyLog(t *testing.T) {
	backend := newFakeBackend(t)
	backend.reply("WorkflowAgentListMemory", map[string]any{
		"rootId": "root", "path": "/log.ndjson", "total": 9, "notes": []any{},
	})
	code, stdout, stderr := runCLI([]string{"memory", "list", "--kind", "handoff"}, backend.env())
	if code != exitOK {
		t.Fatalf("exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "of=9 kind=handoff") {
		t.Fatalf("a filtered read does not state what it read from: %q", stdout)
	}
	// A blank answer reads as a command that did not work.
	if !strings.Contains(stdout, "No notes recorded yet.") {
		t.Fatalf("empty log printed nothing: %q", stdout)
	}
	calls := backend.recorded("WorkflowAgentListMemory")
	var sent map[string]any
	if err := json.Unmarshal(calls[0].Params[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent["kind"] != "handoff" {
		t.Fatalf("sent = %#v", sent)
	}
	if code, _, _ := runCLI([]string{"memory", "list", "a-positional"}, backend.env()); code != exitError {
		t.Fatalf("a stray positional was accepted: exit = %d", code)
	}
}

func TestMemoryVerbIsInTheCommandTreeAndItsUsage(t *testing.T) {
	if !IsCommand("memory") {
		t.Fatal("memory is not a top-level command")
	}
	if !strings.Contains(Usage(), "memory") {
		t.Fatal("the root usage does not name the memory command")
	}
	code, stdout, _ := runCLI([]string{"memory", "help"}, noEnv)
	if code != exitOK || !strings.Contains(stdout, "add") || !strings.Contains(stdout, "list") {
		t.Fatalf("memory help exit = %d, output = %q", code, stdout)
	}
	// Every kind is documented where the caller is already looking.
	code, stdout, _ = runCLI([]string{"memory", "add", "--help"}, noEnv)
	if code != exitOK {
		t.Fatalf("memory add --help exit = %d", code)
	}
	for _, kind := range memory.Kinds {
		if !strings.Contains(stdout, kind) {
			t.Fatalf("memory add usage does not document kind %q: %q", kind, stdout)
		}
	}
	// It needs a session, like every other execution command.
	if code, _, stderr := runCLI([]string{"memory", "list"}, noEnv); code != exitError ||
		!strings.Contains(stderr, "session") {
		t.Fatalf("outside a session: exit = %d, stderr = %q", code, stderr)
	}
}
