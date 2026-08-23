package sessionimport

import (
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/importir"
)

// ConvertSubagentTranscript is the live path's entry point: one known
// sidechain file, one known launch, no parent transcript in play. The
// property that matters is that it produces exactly what a whole-session
// import produces for the same rows — same events, same order, same ids —
// because the two writers land in the same thread.
func TestConvertSubagentTranscriptMatchesTheJoinedImport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, sessionA+".jsonl")
	writeJSONL(t, path,
		userRow("u1", "", "delegate it", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{
			toolUseBlock("toolu_task", "Task", map[string]any{"description": "do work"}),
		}, "2026-01-01T00:00:01.000Z"),
		toolResultRow("r1", "a1", "toolu_task", "agent finished", "2026-01-01T00:00:06.000Z",
			with("toolUseResult", map[string]any{"agentId": "agent7", "status": "completed"})),
	)
	agentPath := filepath.Join(dir, sessionA, subagentsSubdir, "agent-agent7.jsonl")
	writeJSONL(t, agentPath,
		userRow("s1", "", "the task prompt", "2026-01-01T00:00:02.000Z", with("isSidechain", true)),
		assistantRow("s2", "s1", "msg_sub", []any{textBlock("subagent thinking out loud")},
			"2026-01-01T00:00:03.000Z", with("isSidechain", true)),
		assistantRow("s3", "s2", "msg_sub2", []any{
			toolUseBlock("toolu_sub", "Read", map[string]any{"file_path": "/repo/a.go"}),
		}, "2026-01-01T00:00:04.000Z", with("isSidechain", true)),
		toolResultRow("s4", "s3", "toolu_sub", "package main", "2026-01-01T00:00:05.000Z",
			with("isSidechain", true)),
	)

	standalone, err := ConvertSubagentTranscript(agentPath, "toolu_task")
	if err != nil {
		t.Fatalf("ConvertSubagentTranscript: %v", err)
	}
	got := renderEvents(standalone.Events)
	want := strings.Join([]string{
		`user_text turn=0 item=s1 parent=toolu_task src=s1 content="the task prompt"`,
		`text_delta turn=0 item=msg_sub#0 parent=toolu_task src=s2 content="subagent thinking out loud"`,
		`tool_start turn=0 item=toolu_sub parent=toolu_task src=s3 content=""`,
		`tool_complete turn=0 item=toolu_sub parent=toolu_task src=s4 content="package main"`,
	}, "\n")
	if got != want {
		t.Fatalf("standalone events:\ngot:\n%s\nwant:\n%s", got, want)
	}

	// The same rows read as part of the whole session: identical events,
	// modulo the turn index the caller pins (a subagent has no turns of
	// its own — invariant 10).
	joined := loadBranch(t, path, 0, 1).Events
	var nested []string
	for _, evt := range joined {
		if evt.ParentToolUseID != "toolu_task" {
			continue
		}
		nested = append(nested, strings.Replace(
			renderEvents([]importir.Event{evt}), "turn=1", "turn=0", 1))
	}
	if joinedRendered := strings.Join(nested, "\n"); joinedRendered != got {
		t.Fatalf("standalone conversion diverges from the joined import:\nstandalone:\n%s\njoined:\n%s", got, joinedRendered)
	}
}

// A transcript with nothing convertible in it is an empty result, never
// an error: an agent killed before it produced anything still has a file.
//
// "Nothing convertible" means no rows a timeline can render. An agent
// killed after being GIVEN its task still has its prompt, and that row
// is the whole reason a killed agent's card is not blank — so the empty
// case here is a transcript of pure machinery.
func TestConvertSubagentTranscriptEmptyAndInvalidInputs(t *testing.T) {
	dir := t.TempDir()
	promptOnly := filepath.Join(dir, "agent-only-prompt.jsonl")
	writeJSONL(t, promptOnly,
		userRow("s1", "", "the task prompt", "2026-01-01T00:00:02.000Z", with("isSidechain", true)),
	)
	result, err := ConvertSubagentTranscript(promptOnly, "toolu_task")
	if err != nil {
		t.Fatalf("ConvertSubagentTranscript(prompt only): %v", err)
	}
	if got := renderEvents(result.Events); got != `user_text turn=0 item=s1 parent=toolu_task src=s1 content="the task prompt"` {
		t.Fatalf("prompt-only transcript produced:\n%s", got)
	}

	machineryOnly := filepath.Join(dir, "agent-machinery.jsonl")
	writeJSONL(t, machineryOnly,
		userRow("m1", "", "Caveat: this is a caveat", "2026-01-01T00:00:02.000Z",
			with("isSidechain", true), with("isMeta", true)),
	)
	empty, err := ConvertSubagentTranscript(machineryOnly, "toolu_task")
	if err != nil {
		t.Fatalf("ConvertSubagentTranscript(machinery only): %v", err)
	}
	if len(empty.Events) != 0 {
		t.Fatalf("machinery-only transcript produced %d event(s)", len(empty.Events))
	}

	if _, err := ConvertSubagentTranscript(promptOnly, "  "); err == nil {
		t.Fatal("expected an empty launch tool_use id to be refused")
	}
	if _, err := ConvertSubagentTranscript(filepath.Join(dir, "missing.jsonl"), "toolu_task"); err == nil {
		t.Fatal("expected a missing transcript to be an error")
	}
}
