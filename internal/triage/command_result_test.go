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

func TestCommandResult_ForkedAgentResultPreservesSourceAndMarkdownMetadata(t *testing.T) {
	wsRoot := seedPathlinksWorkspace(t, "internal/triage/command_result.go")
	router, st, _ := newTestRouter(t)
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID: "t-result", ProjectID: triageTestProjectID, Title: "fork result",
		Provider: "claude", WorkspacePath: wsRoot, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	router.SetCodeSpanEnricher(func(text string) json.RawMessage {
		return json.RawMessage(`{"hv":"test","blocks":[]}`)
	})
	evt := commandResultEvent("t-result", "synthetic-1", "Found `internal/triage/command_result.go:1`.\n\n```go\npackage triage\n```")
	evt.Meta = json.RawMessage(`{"commandUuid":"cmd-1","agentResult":{"launchId":"claude-command:cmd-1","sourceKind":"skill","sourceName":"code-review"}}`)
	if err := router.Handle(evt); err != nil {
		t.Fatalf("command result: %v", err)
	}

	item, found, err := st.GetThreadItem("t-result", "command-result:synthetic-1")
	if err != nil || !found {
		t.Fatalf("get result: found=%v err=%v", found, err)
	}
	var meta commandResultMeta
	if err := json.Unmarshal([]byte(item.Meta), &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if meta.AgentResult == nil || meta.AgentResult.LaunchID != "claude-command:cmd-1" || meta.AgentResult.SourceKind != "skill" || meta.AgentResult.SourceName != "code-review" {
		t.Fatalf("agent result source = %+v", meta.AgentResult)
	}
	if len(pathRefsFromMeta(t, item.Meta)) == 0 {
		t.Fatalf("agent result has no validated path refs: %s", item.Meta)
	}
	var enriched map[string]json.RawMessage
	if err := json.Unmarshal([]byte(item.Meta), &enriched); err != nil {
		t.Fatalf("decode enriched meta: %v", err)
	}
	if len(enriched[codeSpansMetaKey]) == 0 {
		t.Fatalf("agent result has no persisted code spans: %s", item.Meta)
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

// ---------------------------------------------- suppressed command results
//
// Some provider-executed commands must not leave a transcript row: one Agent
// Overflow issued for its own bookkeeping (`/rename`), and one whose entire
// output restates state AO already renders in its own UI (`/effort`, `/fast`,
// `/model`). The decision is made at SEND time in the provider package and
// arrives here on CommandResultMeta; triage neither makes it nor reads output
// text to infer it.
//
// Both halves are pinned together on purpose: an over-eager rule would swallow
// `/usage`, which is the one thing this row exists to show.

func suppressedCommandResultEvent(threadID, itemID, text string, suppressed bool) provider.ProviderEvent {
	evt := commandResultEvent(threadID, itemID, text)
	meta, err := json.Marshal(provider.CommandResultMeta{
		CommandUUID: "cmd-" + itemID,
		Suppressed:  suppressed,
	})
	if err != nil {
		panic(err)
	}
	evt.Meta = meta
	return evt
}

func TestCommandResult_SuppressedOutputWritesNoRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(suppressedCommandResultEvent(
		"t1", "msg_1", "Set effort level to high (this session only)", true)); err != nil {
		t.Fatalf("command result: %v", err)
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %+v, want none — the effort confirmation is not transcript content", items)
	}
}

// The other half, and the one that keeps the rule honest: an ordinary command
// still writes its row, with or without the meta present at all.
func TestCommandResult_OrdinaryOutputStillWritesItsRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	if err := router.Handle(suppressedCommandResultEvent(
		"t1", "msg_1", "Current session\n  Tokens: 12,345 in / 6,789 out", false)); err != nil {
		t.Fatalf("marked-unsuppressed command result: %v", err)
	}
	// No meta at all: an older CLI emits no lifecycle bracket, so nothing
	// correlates and nothing may be suppressed.
	if err := router.Handle(commandResultEvent("t1", "msg_2", "Context: 42% used")); err != nil {
		t.Fatalf("meta-less command result: %v", err)
	}
	// Undecodable meta is not a suppression signal either: the safe direction
	// is keeping a row the user might want.
	corrupt := commandResultEvent("t1", "msg_3", "Cost: $0.42")
	corrupt.Meta = json.RawMessage(`{"suppressed":`)
	if err := router.Handle(corrupt); err != nil {
		t.Fatalf("corrupt-meta command result: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3 — an unmarked command keeps its row", len(items))
	}
	for _, item := range items {
		if item.Kind != "command_result" {
			t.Fatalf("kind = %q, want command_result", item.Kind)
		}
	}
}

// Suppression must reach the OVERSIZED path too. That branch writes a payload
// before the row, so a check placed only on the inline return would leak the
// bytes into SQLite while showing no row for them.
func TestCommandResult_SuppressedOversizedOutputWritesNoPayload(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	big := strings.Repeat("x", commandResultInlineRunes+500)
	if err := router.Handle(suppressedCommandResultEvent("t1", "msg_1", big, true)); err != nil {
		t.Fatalf("command result: %v", err)
	}
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %+v, want none", items)
	}
	// The oversized branch writes its payload in the same call that writes
	// the row, so no row means no bytes: a payload can only be reached
	// through the item that names it.
	if _, err := st.GetPayloadData("t1", CommandResultItemID(0, "msg_1", 0)); err == nil {
		t.Fatal("a suppressed command left a payload behind")
	}
}
