package sessionimport

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// golden_test.go — a readable record of what the writer produces.
//
// parity_test.go proves the import agrees with the live router on the
// shapes BOTH can produce. This file covers the rest: multi-turn
// sequences, subagent nesting, Codex's split completions, a
// backgrounded Task's sibling row, an import_unavailable marker, and a
// provider-executed command's row — plus it renders the whole result as
// text, so a shape change shows up as a reviewable diff instead of a
// green test.
//
// Regenerate with:
//
//	go test ./internal/sessionimport -run TestGolden -update

var updateGolden = flag.Bool("update", false, "rewrite the golden files from the current writer output")

func TestGoldenClaudeSession(t *testing.T) {
	runGolden(t, "claude_session.json", "claude", claudeGoldenEvents)
}

func TestGoldenCodexSession(t *testing.T) {
	runGolden(t, "codex_session.json", "codex", codexGoldenEvents)
}

func runGolden(t *testing.T, name, providerName string, build func(threadID string) []importir.Event) {
	t.Helper()
	st := newTestStore(t)
	thread := seedThread(t, st, testThreadID, providerName, t.TempDir())

	events := build(testThreadID)
	batch, warnings, err := NewWriter(st, thread).Build(events)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := st.ApplyImportBatch(thread.ID, batch); err != nil {
		t.Fatalf("apply: %v", err)
	}

	snapshot := goldenSnapshot{
		Rows:     goldenRows(t, st, thread.ID),
		Turns:    readParityTurns(t, st, thread.ID),
		Usage:    batch.Usage,
		Warnings: warnings,
	}
	encoded, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	encoded = append(encoded, '\n')

	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("create testdata: %v", err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(want) != string(encoded) {
		t.Errorf("%s is stale.\n--- want ---\n%s\n--- got ---\n%s", path, want, encoded)
	}
}

type goldenSnapshot struct {
	Rows     []goldenRow            `json:"rows"`
	Turns    []parityTurn           `json:"turns"`
	Usage    []store.UsageLedgerRow `json:"usage"`
	Warnings []importir.Warning     `json:"warnings"`
}

// goldenRow is the reviewable projection: identity, placement, and the
// metas. Payload DATA is deliberately absent — it is the provider's own
// text, and pinning it here would make the file a transcript rather
// than a shape record.
type goldenRow struct {
	TurnIndex    int            `json:"turnIndex"`
	ItemIndex    int            `json:"itemIndex"`
	ID           string         `json:"id"`
	Kind         string         `json:"kind"`
	Role         string         `json:"role"`
	Status       string         `json:"status"`
	Summary      string         `json:"summary"`
	ParentID     string         `json:"parentId,omitempty"`
	CompletionOf string         `json:"completionOf,omitempty"`
	ToolName     string         `json:"toolName,omitempty"`
	IsBackground bool           `json:"isBackground,omitempty"`
	CreatedAt    int64          `json:"createdAt"`
	UpdatedAt    int64          `json:"updatedAt"`
	Meta         any            `json:"meta,omitempty"`
	Payload      *goldenPayload `json:"payload,omitempty"`
	InputPayload *goldenPayload `json:"inputPayload,omitempty"`
}

type goldenPayload struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Meta any    `json:"meta,omitempty"`
}

func goldenRows(t *testing.T, st *store.Store, threadID string) []goldenRow {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	rows := make([]goldenRow, 0, len(items))
	for _, item := range items {
		rows = append(rows, goldenRow{
			TurnIndex:    item.TurnIndex,
			ItemIndex:    item.ItemIndex,
			ID:           item.ID,
			Kind:         item.Kind,
			Role:         item.Role,
			Status:       item.Status,
			Summary:      item.Summary,
			ParentID:     item.ParentID,
			CompletionOf: item.CompletionOf,
			ToolName:     item.ToolName,
			IsBackground: item.IsBackground,
			CreatedAt:    item.CreatedAt,
			UpdatedAt:    item.UpdatedAt,
			Meta:         normalizeMeta(t, item.Meta),
			Payload:      goldenPayloadFor(t, st, item.PayloadID),
			InputPayload: goldenPayloadFor(t, st, item.InputPayloadID),
		})
	}
	return rows
}

func goldenPayloadFor(t *testing.T, st *store.Store, payloadID string) *goldenPayload {
	t.Helper()
	if payloadID == "" {
		return nil
	}
	meta, err := st.GetPayloadMeta(payloadID)
	if err != nil {
		t.Fatalf("payload meta %s: %v", payloadID, err)
	}
	return &goldenPayload{
		ID:   maskUUIDs(meta.ID),
		Kind: meta.Kind,
		Meta: normalizeMeta(t, meta.Meta),
	}
}

// claudeGoldenEvents is a two-turn transcript: a subagent-launching turn
// whose Task is backgrounded and observed later, a provider-executed
// slash command, and a second turn whose tool output the session file no
// longer holds.
func claudeGoldenEvents(threadID string) []importir.Event {
	return importEvents([]provider.ProviderEvent{
		{Kind: provider.EventUserText, ThreadID: threadID,
			Content:   "Audit the parser.",
			Meta:      json.RawMessage(`{"provider_item_id":"user-1","parent_uuid":"root"}`),
			Timestamp: at(0)},
		{Kind: provider.EventThinking, ThreadID: threadID, ItemID: "reasoning-1",
			Content: "Delegate the sweep.", Timestamp: at(1)},
		{Kind: provider.EventToolStart, ThreadID: threadID,
			ItemID: "toolu_task_1", ItemType: "Task",
			Meta: json.RawMessage(
				`{"toolName":"Task","is_background":true,"input":{"description":"sweep parser"}}`),
			Timestamp: at(2)},
		{Kind: provider.EventUserText, ThreadID: threadID,
			ItemID: "child-prompt-1", ParentToolUseID: "toolu_task_1",
			Content:   "Sweep every parser file.",
			Meta:      json.RawMessage(`{"provider_item_id":"child-prompt-1"}`),
			Timestamp: at(3)},
		{Kind: provider.EventTextDelta, ThreadID: threadID,
			ItemID: "child-msg-1", ParentToolUseID: "toolu_task_1",
			Content: "Found two gaps.", Timestamp: at(4)},
		{Kind: provider.EventToolComplete, ThreadID: threadID,
			ItemID: "toolu_task_1", Content: "queued", Timestamp: at(5)},
		{Kind: provider.EventBackgroundTaskTerminal, ThreadID: threadID,
			ItemID: "toolu_task_1", Content: "Sweep finished.", Timestamp: at(6)},
		{Kind: provider.EventCommandResult, ThreadID: threadID,
			ItemID: "msg_cmd_1", Content: "Total cost: $0.42", Timestamp: at(7)},
		{Kind: provider.EventTurnComplete, ThreadID: threadID, Timestamp: at(8),
			TurnComplete: &provider.WireTurnCompleteMeta{
				StopReason:         "end_turn",
				AssistantMessageID: "msg_1",
				ModelUsage: []provider.ModelTokenUsage{{
					Model:      "claude-sonnet-4-5",
					TokenUsage: provider.TokenUsage{InputTokens: 900, OutputTokens: 120},
				}},
			}},

		{Kind: provider.EventUserText, ThreadID: threadID,
			Content:   "Show me the second gap.",
			Meta:      json.RawMessage(`{"provider_item_id":"user-2","parent_uuid":"user-1"}`),
			Timestamp: at(20)},
		{Kind: provider.EventToolStart, ThreadID: threadID,
			ItemID: "toolu_bash_2", ItemType: "Bash",
			Meta:      json.RawMessage(`{"toolName":"Bash","input":{"command":"rg parser -n"}}`),
			Timestamp: at(21)},
		{Kind: provider.EventToolComplete, ThreadID: threadID,
			ItemID:    "toolu_bash_2",
			Meta:      json.RawMessage(`{"exit_code":0,"import_unavailable":"tool-output-gc"}`),
			Timestamp: at(22)},
		{Kind: provider.EventNotification, ThreadID: threadID,
			ItemID: "notice-1", ItemType: "session_died",
			Content: "Session ended.", Timestamp: at(23)},
	})
}

// codexGoldenEvents exercises the Codex-shaped differences: an explicit
// turn boundary carrying the wire turn id, a file_change tool result,
// and the wait_agent completion that gets its own sibling row.
func codexGoldenEvents(threadID string) []importir.Event {
	return importEvents([]provider.ProviderEvent{
		{Kind: provider.EventTurnStart, ThreadID: threadID,
			TurnID: "turn_abc", Timestamp: at(0)},
		{Kind: provider.EventUserText, ThreadID: threadID,
			Content:   "Spawn a reviewer.",
			Meta:      json.RawMessage(`{"provider_item_id":"item_user_1"}`),
			Timestamp: at(1)},
		{Kind: provider.EventToolStart, ThreadID: threadID,
			ItemID: "item_spawn_1", ItemType: "spawn_agent",
			Meta:      json.RawMessage(`{"toolName":"spawn_agent","input":{"prompt":"review"}}`),
			Timestamp: at(2)},
		{Kind: provider.EventToolStart, ThreadID: threadID,
			ItemID: "item_wait_1", ItemType: "wait_agent",
			Meta:      json.RawMessage(`{"toolName":"wait_agent","input":{"agent_id":"a1"}}`),
			Timestamp: at(3)},
		{Kind: provider.EventToolComplete, ThreadID: threadID,
			ItemID: "item_wait_1", Content: "Reviewer approved.",
			Meta:      json.RawMessage(`{"toolName":"wait_agent"}`),
			Timestamp: at(4)},
		{Kind: provider.EventCompactBoundary, ThreadID: threadID,
			ItemID: "item_compact_1", Timestamp: at(5),
			Meta: json.RawMessage(`{"trigger":"auto"}`)},
		{Kind: provider.EventError, ThreadID: threadID,
			Content: "stream disconnected", Timestamp: at(6)},
		{Kind: provider.EventTurnComplete, ThreadID: threadID,
			TurnID: "turn_abc", Timestamp: at(7),
			TurnComplete: &provider.WireTurnCompleteMeta{
				StopReason: "end_turn",
				ModelUsage: []provider.ModelTokenUsage{{
					Model:      "gpt-5-codex",
					TokenUsage: provider.TokenUsage{InputTokens: 400, OutputTokens: 60, ReasoningOutputTokens: 40},
				}},
			}},
	})
}
