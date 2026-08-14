package triage

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
)

const localCommandFixture = "../../docs/references/fixtures/claude/local_command_20260803.ndjson"

func commandResultEvent(threadID, itemID, text string) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind:           provider.EventCommandResult,
		ThreadID:       threadID,
		ItemID:         itemID,
		Content:        text,
		ContentPresent: true,
		Timestamp:      time.Now(),
	}
}

func TestCommandResult_PersistsItsOwnKind(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	const output = "Current session\n  Tokens: 12,345 in / 6,789 out"
	if err := router.Handle(commandResultEvent("t1", "msg_1", output)); err != nil {
		t.Fatalf("command result: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	item := items[0]
	if item.Kind != "command_result" {
		t.Fatalf("kind = %q, want command_result — it must never render as an assistant bubble", item.Kind)
	}
	if item.Role != "system" {
		t.Fatalf("role = %q, want system", item.Role)
	}
	if item.Status != "completed" {
		t.Fatalf("status = %q, want completed", item.Status)
	}
	if item.Summary != output {
		t.Fatalf("summary = %q, want the full output inline", item.Summary)
	}
	if item.PayloadID != "" {
		t.Fatalf("payload id = %q, want none for output under the inline bound", item.PayloadID)
	}

	var meta commandResultMeta
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta.Kind != "command_result" || meta.Preview != output || meta.Truncated {
		t.Fatalf("meta = %+v", meta)
	}
}

// A repeated envelope (replay, reconnect) must upsert the same row rather than
// stack duplicates — the provider message id is what makes that possible.
func TestCommandResult_IsIdempotentOnProviderID(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	for i := 0; i < 3; i++ {
		if err := router.Handle(commandResultEvent("t1", "msg_1", "output")); err != nil {
			t.Fatalf("command result %d: %v", i, err)
		}
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
}

func TestCommandResult_OversizedOutputMovesToAPayload(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	output := strings.Repeat("x", commandResultInlineRunes+500)
	if err := router.Handle(commandResultEvent("t1", "msg_1", output)); err != nil {
		t.Fatalf("command result: %v", err)
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	item := items[0]
	if item.PayloadID == "" {
		t.Fatal("oversized output must land in a payload")
	}
	if len([]rune(item.Summary)) > commandResultInlineRunes+len("...") {
		t.Fatalf("summary is %d runes, want the bounded preview", len([]rune(item.Summary)))
	}
	var meta commandResultMeta
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if !meta.Truncated || meta.TotalBytes != len(output) {
		t.Fatalf("meta = %+v", meta)
	}
	data, err := st.GetPayloadData(item.ThreadID, item.PayloadID)
	if err != nil {
		t.Fatalf("get payload: %v", err)
	}
	if string(data) != output {
		t.Fatalf("payload holds %d bytes, want the full %d", len(data), len(output))
	}
}

func TestCommandResult_EmptyOutputPersistsNothing(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(commandResultEvent("t1", "msg_1", "   \n ")); err != nil {
		t.Fatalf("command result: %v", err)
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %+v, want none", items)
	}
}

// TestCommandResult_FullWireSequenceProducesExactlyOneRow replays the probe's
// whole envelope sequence through the real Claude parser and the router. The
// regression it guards is `result.result` — the trailing result envelope
// repeats the command's output verbatim, and it must not become a second row.
// The `<command-name>` metadata echo must not become a user bubble either.
func TestCommandResult_FullWireSequenceProducesExactlyOneRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	parser := &claude.Parser{}
	for _, line := range triageFixtureLines(t, localCommandFixture) {
		events, err := parser.ParseLine("t1", line)
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", line, err)
		}
		for _, evt := range events {
			if err := router.Handle(evt); err != nil {
				t.Fatalf("handle %s: %v", evt.Kind, err)
			}
		}
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	byKind := map[string]int{}
	for _, item := range items {
		byKind[item.Kind]++
	}
	if byKind["command_result"] != 1 {
		t.Fatalf("command_result rows = %d, want 1 (all rows: %v)", byKind["command_result"], byKind)
	}
	if byKind["user_text"] != 0 {
		t.Fatalf("user_text rows = %d, want 0 — the <command-name> echo must stay off the timeline", byKind["user_text"])
	}
	if byKind["assistant_text"] != 0 {
		t.Fatalf("assistant_text rows = %d, want 0 — command output is not model output", byKind["assistant_text"])
	}
}

func triageFixtureLines(t *testing.T, path string) [][]byte {
	t.Helper()
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatalf("open fixture %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()

	var lines [][]byte
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lines = append(lines, []byte(line))
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan fixture %s: %v", path, err)
	}
	return lines
}

// TestCommandEcho_ConsumesPendingSendAndTurnIndexingSurvives replays the
// 2026-08-04 incident shape end to end. A provider slash command's replay
// echo (`<command-name>` triple, carrying the send's client-minted uuid)
// used to be suppressed at the parser as injected content, so the send's
// pending-send entry was never consumed. The stranded entry then poisoned
// resolveTurnIndexOnStart for every later send in the session: the next
// init peeked the stale head, reopened the SETTLED turn's index, the new
// response's rows sorted above the newer user message, and the reset
// id-scope counters overwrote rows the settled turn had persisted.
//
// The echo now reaches triage flagged `command_echo`: a matched echo
// consumes its entry and stamps the optimistic row (typed text kept, XML
// never shown), and the following send opens the NEXT turn index.
func TestCommandEcho_ConsumesPendingSendAndTurnIndexingSurvives(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	persistUser := func(id string, turnIndex int) {
		t.Helper()
		now := time.Now().UnixMilli()
		if err := router.PersistItem(store.Item{
			ID:        id,
			ThreadID:  "t1",
			TurnIndex: turnIndex,
			Kind:      "user_text",
			Role:      "user",
			Status:    "completed",
			Summary:   "typed text for " + id,
			CreatedAt: now,
			UpdatedAt: now,
		}, nil); err != nil {
			t.Fatalf("persist %s: %v", id, err)
		}
	}
	commandEchoMeta, err := json.Marshal(map[string]any{
		"provider_item_id": "uuid-cmd",
		"parent_uuid":      "",
		"command_echo":     true,
	})
	if err != nil {
		t.Fatalf("marshal echo meta: %v", err)
	}

	// Send 1: the slash command. App-side: optimistic row + pending entry.
	persistUser("user:0", 0)
	router.RegisterPendingSendExpecting("t1", "user:0", 0, "uuid-cmd")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventInit, ThreadID: "t1", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("init for command send: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventUserText,
		ThreadID:  "t1",
		Content:   "<command-name>/skill</command-name>\n<command-message>skill</command-message>\n<command-args>go again</command-args>",
		Meta:      commandEchoMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("command echo: %v", err)
	}
	if router.HasPendingSendForThread("t1") {
		t.Fatal("command echo must consume the pending-send entry — a stranded entry poisons every later turn's index")
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("settle command turn: %v", err)
	}

	// Send 2: an ordinary follow-up message.
	persistUser("user:1", 1)
	router.RegisterPendingSendExpecting("t1", "user:1", 1, "uuid-followup")
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventInit, ThreadID: "t1", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("init for follow-up send: %v", err)
	}
	if got := router.OpenTurnIndex("t1"); got != 1 {
		t.Fatalf("follow-up opened turn %d, want 1 — reopening the settled turn re-sorts its rows above the new user message", got)
	}

	// The typed text stays authoritative; the XML never becomes a row.
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, item := range items {
		if strings.Contains(item.Summary, "<command-name>") {
			t.Fatalf("command XML leaked into row %s: %q", item.ID, item.Summary)
		}
		if item.ID == "user:0" && item.Summary != "typed text for user:0" {
			t.Fatalf("user:0 summary = %q, want the typed text preserved", item.Summary)
		}
	}
}
