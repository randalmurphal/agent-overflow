package sessionimport

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
)

// joinedSubagentEvents converts rows as agent7's sidechain inside a whole
// session import and returns the events nested under its launch, with the
// turn pinned to 0 as a mirrored projection carries it. The mirror and the
// import write the same thread, so the import is the projector's reference.
func joinedSubagentEvents(t *testing.T, rows ...any) []importir.Event {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, sessionA+".jsonl")
	writeJSONL(t, path,
		userRow("u1", "", "delegate it", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{
			toolUseBlock("toolu_task", "Task", map[string]any{"description": "do work"}),
		}, "2026-01-01T00:00:01.000Z"),
		toolResultRow("r1", "a1", "toolu_task", "agent finished", "2026-01-01T00:00:09.000Z",
			with("toolUseResult", map[string]any{"agentId": "agent7", "status": "completed"})),
	)
	writeJSONL(t, filepath.Join(dir, sessionA, subagentsSubdir, "agent-agent7.jsonl"), rows...)
	var nested []importir.Event
	for _, evt := range loadBranch(t, path, 0, 1).Events {
		if evt.ParentToolUseID != "toolu_task" {
			continue
		}
		evt.TurnIndex = 0
		nested = append(nested, evt)
	}
	return nested
}

// projectInBatches runs rows through one projector, one Append per batch,
// and closes it.
func projectInBatches(t *testing.T, batches ...[]map[string]any) []importir.Event {
	t.Helper()
	projector, err := NewSidechainProjector("toolu_task")
	if err != nil {
		t.Fatalf("NewSidechainProjector: %v", err)
	}
	var events []importir.Event
	for _, batch := range batches {
		entries := make([]json.RawMessage, 0, len(batch))
		for _, row := range batch {
			encoded, marshalErr := json.Marshal(row)
			if marshalErr != nil {
				t.Fatalf("marshal fixture: %v", marshalErr)
			}
			entries = append(entries, encoded)
		}
		result, appendErr := projector.Append(entries)
		if appendErr != nil {
			t.Fatalf("Append: %v", appendErr)
		}
		events = append(events, result.Events...)
	}
	return append(events, projector.Close().Events...)
}

// The mirror delivers a sidechain in arbitrary batches. Split or whole, the
// projection is exactly what a whole-session import produces for the same
// rows: same events, same order, same ids.
func TestSidechainProjectorMatchesTheJoinedImportAcrossMirrorBatches(t *testing.T) {
	fixture := []map[string]any{
		userRow("s1", "", "the task prompt", "2026-01-01T00:00:02.000Z", with("isSidechain", true)),
		assistantRow("s2", "s1", "msg_sub", []any{textBlock("first block")},
			"2026-01-01T00:00:03.000Z", with("isSidechain", true)),
		assistantRow("s3", "s2", "msg_sub", []any{textBlock("second block")},
			"2026-01-01T00:00:04.000Z", with("isSidechain", true)),
		assistantRow("s4", "s3", "msg_tool", []any{
			toolUseBlock("toolu_sub", "Read", map[string]any{"file_path": "/repo/a.go"}),
		}, "2026-01-01T00:00:05.000Z", with("isSidechain", true)),
		toolResultRow("s5", "s4", "toolu_sub", "package main", "2026-01-01T00:00:06.000Z",
			with("isSidechain", true)),
	}
	lines := make([]any, len(fixture))
	split := make([][]map[string]any, len(fixture))
	for i := range fixture {
		lines[i] = fixture[i]
		split[i] = []map[string]any{fixture[i]}
	}
	want := renderEvents(joinedSubagentEvents(t, lines...))
	if want != strings.Join([]string{
		`user_text turn=0 item=s1 parent=toolu_task src=s1 content="the task prompt"`,
		`text_delta turn=0 item=msg_sub#0 parent=toolu_task src=s2 content="first block"`,
		`text_delta turn=0 item=msg_sub#1 parent=toolu_task src=s3 content="second block"`,
		`tool_start turn=0 item=toolu_sub parent=toolu_task src=s4 content=""`,
		`tool_complete turn=0 item=toolu_sub parent=toolu_task src=s5 content="package main"`,
	}, "\n") {
		t.Fatalf("joined import reference changed:\n%s", want)
	}

	streamed := projectInBatches(t, split...)
	if got := renderEvents(streamed); got != want {
		t.Fatalf("one-row batches diverged from the joined import:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if got := renderEvents(projectInBatches(t, fixture)); got != want {
		t.Fatalf("one batch diverged from the joined import:\ngot:\n%s\nwant:\n%s", got, want)
	}
	var promptMeta map[string]any
	if err := json.Unmarshal(streamed[0].Meta, &promptMeta); err != nil {
		t.Fatalf("opening prompt meta: %v", err)
	}
	if promptMeta[provider.MetaSubagentOpeningPromptKey] != true {
		t.Fatalf("opening prompt meta = %s, want %s=true", streamed[0].Meta, provider.MetaSubagentOpeningPromptKey)
	}

	projector, err := NewSidechainProjector("toolu_task")
	if err != nil {
		t.Fatalf("NewSidechainProjector: %v", err)
	}
	projector.Close()
	if result := projector.Close(); len(result.Events) != 0 || len(result.Warnings) != 0 {
		t.Fatalf("second Close returned %+v", result)
	}
	if _, err := projector.Append(nil); err == nil {
		t.Fatal("Append after Close succeeded")
	}
}

func TestSidechainProjectorFoldsCompactSummaryAcrossMirrorBatches(t *testing.T) {
	boundary := map[string]any{
		"type": "system", "subtype": "compact_boundary", "uuid": "compact-1",
		"content": "Conversation compacted", "timestamp": "2026-01-01T00:00:01.000Z",
	}
	summary := userRow("summary-1", "compact-1", "kept facts", "2026-01-01T00:00:02.000Z",
		with("isCompactSummary", true), with("isVisibleInTranscriptOnly", true))

	projector, err := NewSidechainProjector("toolu_task")
	if err != nil {
		t.Fatalf("NewSidechainProjector: %v", err)
	}
	var streamed []importir.Event
	for _, entry := range []map[string]any{boundary, summary} {
		encoded, marshalErr := json.Marshal(entry)
		if marshalErr != nil {
			t.Fatalf("marshal fixture: %v", marshalErr)
		}
		result, appendErr := projector.Append([]json.RawMessage{encoded})
		if appendErr != nil {
			t.Fatalf("Append: %v", appendErr)
		}
		if len(result.Events) > 1 {
			t.Fatalf("Append returned %d events, want at most the folded boundary", len(result.Events))
		}
		streamed = append(streamed, result.Events...)
	}
	result := projector.Close()
	streamed = append(streamed, result.Events...)
	if len(result.Events) != 0 {
		t.Fatalf("Close emitted an extra summary event: %s", renderEvents(result.Events))
	}
	whole := projectInBatches(t, []map[string]any{boundary, summary})
	if got, want := renderEvents(streamed), renderEvents(whole); got != want {
		t.Fatalf("split compact projection diverged from one batch:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if len(streamed) != 1 || !strings.Contains(string(streamed[0].Meta), "kept facts") {
		t.Fatalf("compact projection did not fold summary: %+v", streamed)
	}
}

func TestSidechainProjectorRejectsMalformedBatchWithoutAdvancingState(t *testing.T) {
	projector, err := NewSidechainProjector("toolu_task")
	if err != nil {
		t.Fatalf("NewSidechainProjector: %v", err)
	}
	valid, err := json.Marshal(userRow(
		"s1", "", "the task prompt", "2026-01-01T00:00:02.000Z", with("isSidechain", true),
	))
	if err != nil {
		t.Fatalf("marshal valid row: %v", err)
	}
	if result, appendErr := projector.Append([]json.RawMessage{valid, json.RawMessage(`{"type":`)}); appendErr == nil {
		t.Fatal("malformed batch succeeded")
	} else if len(result.Events) != 0 || len(result.Warnings) != 0 {
		t.Fatalf("malformed batch returned partial projection: %+v", result)
	}

	result, err := projector.Append([]json.RawMessage{valid})
	if err != nil {
		t.Fatalf("valid retry after malformed batch: %v", err)
	}
	if got := renderEvents(result.Events); got != `user_text turn=0 item=s1 parent=toolu_task src=s1 content="the task prompt"` {
		t.Fatalf("valid retry after malformed batch produced:\n%s", got)
	}
}

func TestSidechainProjectorUsesMirrorArrivalForTimestampLessRows(t *testing.T) {
	projector, err := NewSidechainProjector("toolu_task")
	if err != nil {
		t.Fatalf("NewSidechainProjector: %v", err)
	}
	encoded, err := json.Marshal(userRow(
		"s1", "", "prompt", "", with("isSidechain", true),
	))
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	receivedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rows, err := DecodeSidechainRows([]json.RawMessage{encoded}, receivedAt)
	if err != nil {
		t.Fatalf("DecodeSidechainRows: %v", err)
	}
	if got, want := rows[0].Timestamp, receivedAt.UnixMilli(); got != want {
		t.Fatalf("Timestamp = %d, want arrival %d", got, want)
	}
	result, err := projector.AppendRows(rows)
	if err != nil {
		t.Fatalf("AppendRows: %v", err)
	}
	if len(result.Events) != 1 || !result.Events[0].Timestamp.Equal(receivedAt) {
		t.Fatalf("events = %+v, want one event at arrival time", result.Events)
	}
}

// "Nothing convertible" means no rows a timeline can render. An agent
// killed after being GIVEN its task still has its prompt, and that row is
// the whole reason a killed agent's card is not blank, so the empty case
// is a sidechain of pure machinery.
func TestSidechainProjectorEmptyAndInvalidInputs(t *testing.T) {
	promptOnly := projectInBatches(t, []map[string]any{
		userRow("s1", "", "the task prompt", "2026-01-01T00:00:02.000Z", with("isSidechain", true)),
	})
	if got := renderEvents(promptOnly); got != `user_text turn=0 item=s1 parent=toolu_task src=s1 content="the task prompt"` {
		t.Fatalf("prompt-only sidechain produced:\n%s", got)
	}

	machineryOnly := projectInBatches(t, []map[string]any{
		userRow("m1", "", "Caveat: this is a caveat", "2026-01-01T00:00:02.000Z",
			with("isSidechain", true), with("isMeta", true)),
	})
	if len(machineryOnly) != 0 {
		t.Fatalf("machinery-only sidechain produced %d event(s)", len(machineryOnly))
	}

	if _, err := NewSidechainProjector("  "); err == nil {
		t.Fatal("expected an empty launch tool_use id to be refused")
	}
}

// A sidechain file is trusted provider history: the whole-session import
// streams it without a per-line display ceiling, so a tool result larger
// than the root session's line limit arrives intact under its launch.
func TestSubagentImportStreamsLargeRecords(t *testing.T) {
	largeOutput := strings.Repeat("x", 17<<20)
	events := joinedSubagentEvents(t,
		userRow("s1", "", "the task prompt", "2026-01-01T00:00:02.000Z", with("isSidechain", true)),
		assistantRow("s2", "s1", "msg_tool", []any{
			toolUseBlock("toolu_large", "Bash", map[string]any{"command": "cat big"}),
		}, "2026-01-01T00:00:03.000Z", with("isSidechain", true)),
		toolResultRow("s3", "s2", "toolu_large", largeOutput, "2026-01-01T00:00:04.000Z",
			with("isSidechain", true)),
		assistantRow("s4", "s3", "msg_final", []any{textBlock("final answer")},
			"2026-01-01T00:00:05.000Z", with("isSidechain", true)),
	)
	var largeResult, finalAnswer bool
	for _, event := range events {
		if event.Kind == provider.EventToolComplete && event.ItemID == "toolu_large" && len(event.Content) == len(largeOutput) {
			largeResult = true
		}
		if event.Kind == provider.EventTextDelta && event.Content == "final answer" {
			finalAnswer = true
		}
	}
	if !largeResult || !finalAnswer {
		t.Fatalf("large sidechain lost rows: result=%v final=%v", largeResult, finalAnswer)
	}
}
