package sessionimport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
)

// renderEvents is the golden form: one line per event, carrying every
// field the writer keys a row off.
func renderEvents(events []importir.Event) string {
	lines := make([]string, 0, len(events))
	for _, e := range events {
		lines = append(lines, fmt.Sprintf("%s turn=%d item=%s parent=%s src=%s content=%q",
			e.Kind, e.TurnIndex, e.ItemID, e.ParentToolUseID, e.SourceUUID, e.Content))
	}
	return strings.Join(lines, "\n")
}

func convertFixture(t *testing.T, opts ConvertOptions, lines ...any) ([]importir.Event, []importir.Warning) {
	t.Helper()
	branch := buildChain(t, lines...)
	return Convert(branch.Chain, opts)
}

func eventsOfKind(events []importir.Event, kind provider.EventKind) []importir.Event {
	var out []importir.Event
	for _, e := range events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func decodeMeta(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode meta %s: %v", raw, err)
	}
	return out
}

func TestConvertFullTurn(t *testing.T) {
	events, warnings := convertFixture(t, ConvertOptions{},
		userRow("u1", "", "do it", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{
			map[string]any{"type": "thinking", "thinking": "hmm", "signature": "sig-1"},
			textBlock("working on it"),
			toolUseBlock("toolu_1", "Bash", map[string]any{"command": "ls"}),
		}, "2026-01-01T00:00:01.000Z"),
		toolResultRow("r1", "a1", "toolu_1", "file.txt", "2026-01-01T00:00:02.000Z",
			with("toolUseResult", map[string]any{"stdout": "file.txt", "exit_code": 0})),
		assistantRow("a2", "r1", "msg_2", []any{textBlock("done")}, "2026-01-01T00:00:03.000Z",
			with("message", map[string]any{
				"role": "assistant", "id": "msg_2", "model": "claude-test-1",
				"stop_reason": "end_turn",
				"content":     []any{textBlock("done")},
				"usage": map[string]any{
					"input_tokens": 10, "output_tokens": 20,
					"cache_read_input_tokens": 5, "cache_creation_input_tokens": 1,
				},
			})),
	)
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none", warnings)
	}

	want := strings.Join([]string{
		`turn_start turn=1 item= parent= src=u1 content=""`,
		`user_text turn=1 item=u1 parent= src=u1 content="do it"`,
		`thinking turn=1 item=msg_1#0 parent= src=a1 content="hmm"`,
		`text_delta turn=1 item=msg_1#1 parent= src=a1 content="working on it"`,
		`tool_start turn=1 item=toolu_1 parent= src=a1 content=""`,
		`tool_complete turn=1 item=toolu_1 parent= src=r1 content="file.txt"`,
		`text_delta turn=1 item=msg_2#0 parent= src=a2 content="done"`,
		`turn_complete turn=1 item= parent= src=a2 content=""`,
	}, "\n")
	if got := renderEvents(events); got != want {
		t.Errorf("events mismatch\n got:\n%s\nwant:\n%s", got, want)
	}

	complete := eventsOfKind(events, provider.EventTurnComplete)[0]
	meta, ok := complete.TurnComplete.(*provider.WireTurnCompleteMeta)
	if !ok {
		t.Fatalf("turn complete meta type = %T", complete.TurnComplete)
	}
	if meta.StopReason != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", meta.StopReason)
	}
	if meta.AssistantMessageID != "msg_2" {
		t.Errorf("assistantMessageID = %q, want msg_2", meta.AssistantMessageID)
	}
	if meta.Usage == nil || meta.Usage.InputTokens != 10 || meta.Usage.OutputTokens != 20 {
		t.Fatalf("usage = %+v", meta.Usage)
	}
	if meta.Usage.TotalCostUSD != 0 {
		t.Errorf("transcripts carry no cost, got %v", meta.Usage.TotalCostUSD)
	}
	if len(meta.ModelUsage) != 1 || meta.ModelUsage[0].Model != "claude-test-1" {
		t.Errorf("modelUsage = %+v", meta.ModelUsage)
	}
	if want := parseISOMillis("2026-01-01T00:00:03.000Z"); complete.Timestamp.UnixMilli() != want {
		t.Errorf("turn completed at %d, want the last source row's time %d", complete.Timestamp.UnixMilli(), want)
	}
}

func TestConvertStartsOneTurnPerUserPrompt(t *testing.T) {
	events, _ := convertFixture(t, ConvertOptions{},
		userRow("u1", "", "one", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("first")}, "2026-01-01T00:00:01.000Z"),
		userRow("u2", "a1", "two", "2026-01-01T00:00:02.000Z"),
		assistantRow("a2", "u2", "msg_2", []any{textBlock("second")}, "2026-01-01T00:00:03.000Z"),
	)
	starts := eventsOfKind(events, provider.EventTurnStart)
	completes := eventsOfKind(events, provider.EventTurnComplete)
	if len(starts) != 2 || len(completes) != 2 {
		t.Fatalf("got %d starts / %d completes, want 2 / 2", len(starts), len(completes))
	}
	if starts[0].TurnIndex != 1 || starts[1].TurnIndex != 2 {
		t.Errorf("turn indexes = %d, %d — must be 1-based and monotonic", starts[0].TurnIndex, starts[1].TurnIndex)
	}
	// The first turn must close before the second opens.
	var order []string
	for _, e := range events {
		switch e.Kind {
		case provider.EventTurnStart, provider.EventTurnComplete:
			order = append(order, fmt.Sprintf("%s/%d", e.Kind, e.TurnIndex))
		}
	}
	if got, want := strings.Join(order, " "), "turn_start/1 turn_complete/1 turn_start/2 turn_complete/2"; got != want {
		t.Errorf("turn order = %q, want %q", got, want)
	}
}

func TestConvertUserContentShapes(t *testing.T) {
	cases := []struct {
		name string
		row  map[string]any
		want string
	}{
		{
			"plain string content",
			userRow("u1", "", "typed prompt", "2026-01-01T00:00:00.000Z"),
			"typed prompt",
		},
		{
			"array of text blocks",
			userBlocksRow("u1", "", []any{textBlock("part one "), textBlock("part two")}, "2026-01-01T00:00:00.000Z"),
			"part one part two",
		},
		{
			"image block alongside text",
			userBlocksRow("u1", "", []any{
				map[string]any{"type": "image", "source": map[string]any{"type": "base64"}},
				textBlock("look at this"),
			}, "2026-01-01T00:00:00.000Z"),
			"look at this",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, _ := convertFixture(t, ConvertOptions{}, tc.row)
			texts := eventsOfKind(events, provider.EventUserText)
			if len(texts) != 1 {
				t.Fatalf("got %d user_text events, want 1", len(texts))
			}
			if texts[0].Content != tc.want {
				t.Errorf("content = %q, want %q", texts[0].Content, tc.want)
			}
			if !texts[0].ContentPresent {
				t.Error("ContentPresent must be set — import never emits a partial")
			}
		})
	}
}

func TestConvertSkipsNonPromptUserRows(t *testing.T) {
	cases := []struct {
		name string
		row  map[string]any
	}{
		{"isMeta caveat", userRow("u1", "", "[Image: 100x100]", "2026-01-01T00:00:00.000Z", with("isMeta", true))},
		{"transcript-only injection", userRow("u1", "", "<context>…</context>", "2026-01-01T00:00:00.000Z",
			with("isVisibleInTranscriptOnly", true))},
		{"whitespace only", userRow("u1", "", "   \n ", "2026-01-01T00:00:00.000Z")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, _ := convertFixture(t, ConvertOptions{}, tc.row)
			if len(eventsOfKind(events, provider.EventUserText)) != 0 {
				t.Errorf("row produced a user_text event: %s", renderEvents(events))
			}
		})
	}
}

func TestConvertToolResultCarriesStructuredResult(t *testing.T) {
	cases := []struct {
		name          string
		toolUseResult any
		wantExitCode  bool
	}{
		{
			"object form",
			map[string]any{"stdout": "hi", "stderr": "", "exit_code": 2, "interrupted": false},
			true,
		},
		{
			"bare string form",
			"Async agent launched successfully.",
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, _ := convertFixture(t, ConvertOptions{},
				userRow("u1", "", "run", "2026-01-01T00:00:00.000Z"),
				assistantRow("a1", "u1", "msg_1", []any{
					toolUseBlock("toolu_1", "Bash", map[string]any{"command": "false"}),
				}, "2026-01-01T00:00:01.000Z"),
				toolResultRow("r1", "a1", "toolu_1", "output", "2026-01-01T00:00:02.000Z",
					with("toolUseResult", tc.toolUseResult), with("is_error", false)),
			)
			completions := eventsOfKind(events, provider.EventToolComplete)
			if len(completions) != 1 {
				t.Fatalf("got %d completions, want 1", len(completions))
			}
			meta := decodeMeta(t, completions[0].Meta)
			// The transcript's camelCase sibling must reach the writer under
			// the wire's snake_case key.
			if _, ok := meta["tool_use_result"]; !ok {
				t.Errorf("meta has no tool_use_result: %v", meta)
			}
			if _, ok := meta["tool_result"]; !ok {
				t.Errorf("meta has no tool_result echo: %v", meta)
			}
			code, hasCode := meta["exit_code"]
			if hasCode != tc.wantExitCode {
				t.Errorf("exit_code present = %v, want %v", hasCode, tc.wantExitCode)
			}
			if tc.wantExitCode && code != float64(2) {
				t.Errorf("exit_code = %v, want 2", code)
			}
		})
	}
}

func TestConvertMarksBackgroundToolResults(t *testing.T) {
	cases := []struct {
		name          string
		toolUseResult map[string]any
		want          bool
	}{
		{"backgroundTaskId", map[string]any{"backgroundTaskId": "task_1"}, true},
		{"async agent launch", map[string]any{"isAsync": true, "status": "async_launched", "agentId": "abc"}, true},
		{"monitor launch", map[string]any{"taskId": "task_2", "persistent": true, "timeoutMs": 1000}, true},
		{"inline agent completion", map[string]any{"agentId": "abc", "status": "completed"}, false},
		{"task list ack", map[string]any{"taskId": "task_3", "success": true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, _ := convertFixture(t, ConvertOptions{},
				userRow("u1", "", "run", "2026-01-01T00:00:00.000Z"),
				assistantRow("a1", "u1", "msg_1", []any{
					toolUseBlock("toolu_1", "Bash", map[string]any{"command": "sleep 100"}),
				}, "2026-01-01T00:00:01.000Z"),
				toolResultRow("r1", "a1", "toolu_1", "started", "2026-01-01T00:00:02.000Z",
					with("toolUseResult", tc.toolUseResult)),
			)
			meta := decodeMeta(t, eventsOfKind(events, provider.EventToolComplete)[0].Meta)
			_, got := meta["is_background"]
			if got != tc.want {
				t.Errorf("is_background present = %v, want %v (meta %v)", got, tc.want, meta)
			}
		})
	}
}

func TestConvertToolStartMetaMirrorsLiveParser(t *testing.T) {
	events, _ := convertFixture(t, ConvertOptions{},
		userRow("u1", "", "run", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{
			toolUseBlock("toolu_1", "Bash", map[string]any{"command": "sleep 1", "run_in_background": true}),
			toolUseBlock("toolu_2", "mcp__github__list_issues", map[string]any{"repo": "x"}),
		}, "2026-01-01T00:00:01.000Z"),
	)
	starts := eventsOfKind(events, provider.EventToolStart)
	if len(starts) != 2 {
		t.Fatalf("got %d tool_start events, want 2", len(starts))
	}

	bash := decodeMeta(t, starts[0].Meta)
	if bash["toolName"] != "Bash" {
		t.Errorf("toolName = %v, want Bash", bash["toolName"])
	}
	if bash["is_background"] != true {
		t.Errorf("is_background = %v, want true", bash["is_background"])
	}
	if bash["assistant_message_id"] != "msg_1" {
		t.Errorf("assistant_message_id = %v, want msg_1", bash["assistant_message_id"])
	}
	if _, ok := bash["input"]; !ok {
		t.Errorf("meta has no input (the summary preview would go blank): %v", bash)
	}
	if starts[0].ItemType != "Bash" {
		t.Errorf("itemType = %q, want the raw tool name", starts[0].ItemType)
	}

	mcp := decodeMeta(t, starts[1].Meta)
	if mcp["toolName"] != "MCP/list_issues" {
		t.Errorf("mcp toolName = %v, want MCP/list_issues", mcp["toolName"])
	}
	pair, _ := mcp["mcp"].(map[string]any)
	if pair["server"] != "github" || pair["tool"] != "list_issues" {
		t.Errorf("mcp pair = %v", pair)
	}
}

func TestConvertMarksGarbageCollectedToolOutput(t *testing.T) {
	sessionDir := t.TempDir()
	persisted := "<persisted-output>\nOutput too large (2.1 MB). Full output saved to: " +
		filepath.Join(sessionDir, toolResultsSubdir, "toolu_1.txt") +
		"\n\nPreview (first 2 KB):\nfirst lines…\n...\n</persisted-output>"

	build := func(content string) []importir.Event {
		events, _ := convertFixture(t, ConvertOptions{SessionDir: sessionDir},
			userRow("u1", "", "run", "2026-01-01T00:00:00.000Z"),
			assistantRow("a1", "u1", "msg_1", []any{
				toolUseBlock("toolu_1", "Bash", map[string]any{"command": "cat huge"}),
			}, "2026-01-01T00:00:01.000Z"),
			toolResultRow("r1", "a1", "toolu_1", content, "2026-01-01T00:00:02.000Z"),
		)
		return eventsOfKind(events, provider.EventToolComplete)
	}

	t.Run("externalised file still present", func(t *testing.T) {
		dir := filepath.Join(sessionDir, toolResultsSubdir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "toolu_1.txt"), []byte("full output"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })

		meta := decodeMeta(t, build(persisted)[0].Meta)
		if _, marked := meta[MetaImportUnavailableKey]; marked {
			t.Errorf("output is on disk but was marked unavailable: %v", meta)
		}
	})

	t.Run("externalised file garbage collected", func(t *testing.T) {
		meta := decodeMeta(t, build(persisted)[0].Meta)
		if meta[MetaImportUnavailableKey] != MetaImportUnavailableToolOutputGC {
			t.Errorf("%s = %v, want %q", MetaImportUnavailableKey,
				meta[MetaImportUnavailableKey], MetaImportUnavailableToolOutputGC)
		}
	})

	t.Run("content cleared in place", func(t *testing.T) {
		meta := decodeMeta(t, build(toolResultClearedMessage)[0].Meta)
		if meta[MetaImportUnavailableKey] != MetaImportUnavailableToolOutputGC {
			t.Errorf("%s = %v, want %q", MetaImportUnavailableKey,
				meta[MetaImportUnavailableKey], MetaImportUnavailableToolOutputGC)
		}
	})

	t.Run("ordinary output is never marked", func(t *testing.T) {
		meta := decodeMeta(t, build("small output")[0].Meta)
		if _, marked := meta[MetaImportUnavailableKey]; marked {
			t.Errorf("ordinary output marked unavailable: %v", meta)
		}
	})
}

func TestConvertCompactionEmitsOneDividerWithSummary(t *testing.T) {
	events, _ := convertFixture(t, ConvertOptions{},
		userRow("u1", "", "before", "2026-01-01T00:00:00.000Z"),
		assistantRow("a1", "u1", "msg_1", []any{textBlock("long")}, "2026-01-01T00:00:01.000Z"),
		map[string]any{
			"type": "system", "subtype": "compact_boundary",
			"uuid": "b1", "parentUuid": nil, "logicalParentUuid": "a1",
			"isSidechain": false, "content": "Conversation compacted",
			"timestamp": "2026-01-01T00:00:02.000Z",
			"compactMetadata": map[string]any{
				"trigger": "auto", "preTokens": 340000, "durationMs": 1200,
				"preservedSegment": map[string]any{"head": strings.Repeat("h", 500)},
			},
		},
		userRow("s1", "b1", "Summary: everything so far.", "2026-01-01T00:00:03.000Z",
			with("isCompactSummary", true), with("isVisibleInTranscriptOnly", true)),
		userRow("u2", "s1", "carry on", "2026-01-01T00:00:04.000Z"),
	)

	boundaries := eventsOfKind(events, provider.EventCompactBoundary)
	if len(boundaries) != 1 {
		t.Fatalf("got %d compaction events, want exactly 1: %s", len(boundaries), renderEvents(events))
	}
	if boundaries[0].ItemID != "b1" {
		t.Errorf("compaction itemID = %q, want the boundary row uuid", boundaries[0].ItemID)
	}
	meta := decodeMeta(t, boundaries[0].Meta)
	if meta["summary"] != "Summary: everything so far." {
		t.Errorf("summary = %v", meta["summary"])
	}
	if meta["trigger"] != "auto" {
		t.Errorf("trigger = %v, want auto", meta["trigger"])
	}
	if _, leaked := meta["preservedSegment"]; leaked {
		t.Errorf("preservedSegment must stay out of items.meta: %v", meta)
	}
	// The summary row must not also become a user_text row.
	for _, e := range eventsOfKind(events, provider.EventUserText) {
		if strings.HasPrefix(e.Content, "Summary:") {
			t.Errorf("compaction summary leaked as a user message: %s", renderEvents(events))
		}
	}
}

func TestConvertStandaloneCompactSummary(t *testing.T) {
	events, _ := convertFixture(t, ConvertOptions{},
		userRow("u1", "", "before", "2026-01-01T00:00:00.000Z"),
		userRow("s1", "u1", "Summary of prior work.", "2026-01-01T00:00:01.000Z",
			with("isCompactSummary", true)),
	)
	boundaries := eventsOfKind(events, provider.EventCompactBoundary)
	if len(boundaries) != 1 {
		t.Fatalf("got %d compaction events, want 1: %s", len(boundaries), renderEvents(events))
	}
	meta := decodeMeta(t, boundaries[0].Meta)
	if meta["summary"] != "Summary of prior work." {
		t.Errorf("summary = %v", meta["summary"])
	}
}
