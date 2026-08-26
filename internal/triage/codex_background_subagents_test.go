package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

func mailboxDelivery(t *testing.T, agentPath, message string) json.RawMessage {
	t.Helper()
	meta, err := json.Marshal(map[string]any{
		"agent_path":       agentPath,
		"status":           "completed",
		"message_type":     "FINAL_ANSWER",
		"mailbox_delivery": true,
		"delivery_id":      "content:" + message,
		"message":          message,
	})
	if err != nil {
		t.Fatalf("marshal mailbox delivery: %v", err)
	}
	return meta
}

func resumeChild(t *testing.T, router *Router, threadID, launchID, childID string) {
	t.Helper()
	meta, err := json.Marshal(map[string]any{"agent_path": childID, "status": "running"})
	if err != nil {
		t.Fatalf("marshal resume: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: threadID, ItemID: launchID,
		Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("child resume: %v", err)
	}
}

func deliverMailbox(t *testing.T, router *Router, threadID, launchID string, meta json.RawMessage) {
	t.Helper()
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentNotification, ThreadID: threadID, ItemID: launchID,
		Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("mailbox delivery: %v", err)
	}
}

func completionRowsFor(t *testing.T, st *store.Store, threadID, launchID string) []store.Item {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var out []store.Item
	for _, item := range items {
		if item.CompletionOf == launchID {
			out = append(out, item)
		}
	}
	return out
}

// A child woken by `followup_task` can legitimately answer IDENTICALLY twice.
// Content hashing alone collapsed the second answer onto the first row; the
// per-child resume generation is what separates them. The SAME delivery seen
// on both carriers (live raw stream + rollout tail) must still land once.
func TestIdenticalMailboxAnswersAcrossAResumeGetTwoRows(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	seedCodexSpawnCard(t, router, st, "t1", "spawn-1", "child-1")

	first := mailboxDelivery(t, "/root/reviewer", "Done.")
	deliverMailbox(t, router, "t1", "spawn-1", first)
	// The same record arriving on the second carrier.
	deliverMailbox(t, router, "t1", "spawn-1", first)
	if rows := completionRowsFor(t, st, "t1", "spawn-1"); len(rows) != 1 {
		t.Fatalf("one delivery on two carriers produced %d rows", len(rows))
	}

	resumeChild(t, router, "t1", "spawn-1", "child-1")
	deliverMailbox(t, router, "t1", "spawn-1", mailboxDelivery(t, "/root/reviewer", "Done."))

	rows := completionRowsFor(t, st, "t1", "spawn-1")
	if len(rows) != 2 {
		t.Fatalf("an identical answer after a resume produced %d rows, want 2", len(rows))
	}
	if rows[0].ID == rows[1].ID {
		t.Fatalf("both answers landed on %s", rows[0].ID)
	}
}

// Progress is a chronological activity row. The launch's visible content stays
// the spawn event, and the body is bounded before it reaches the timeline.
func TestMailboxProgressStoresABoundedBody(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	seedCodexSpawnCard(t, router, st, "t1", "spawn-1", "child-1")

	progress := func(message string) json.RawMessage {
		meta, err := json.Marshal(map[string]any{
			"agent_path":       "/root/reviewer",
			"status":           "running",
			"message_type":     "MESSAGE",
			"mailbox_delivery": true,
			"delivery_id":      "content:" + message,
			"message":          message,
		})
		if err != nil {
			t.Fatalf("marshal progress: %v", err)
		}
		return meta
	}

	launchBefore, found, err := st.GetThreadItem("t1", "spawn-1")
	if err != nil || !found {
		t.Fatalf("launch before progress: found=%v err=%v", found, err)
	}
	deliverMailbox(t, router, "t1", "spawn-1", progress("halfway; tests failing in X\nsecond line dropped"))

	long := ""
	for range codexCollabProgressTextRunes + 40 {
		long += "x"
	}
	deliverMailbox(t, router, "t1", "spawn-1", progress(long))

	launchAfter, found, err := st.GetThreadItem("t1", "spawn-1")
	if err != nil || !found {
		t.Fatalf("launch after progress: found=%v err=%v", found, err)
	}
	if launchAfter.Meta != launchBefore.Meta || launchAfter.UpdatedAt != launchBefore.UpdatedAt {
		t.Fatalf("progress mutated launch: before=%+v after=%+v", launchBefore, launchAfter)
	}

	var rows []store.Item
	for _, item := range findItemsByKind(t, st, "t1", itemKindToolCall) {
		if item.ToolName == "send_input" {
			rows = append(rows, item)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("progress rows = %+v, want two standalone rows", rows)
	}
	if rows[0].ParentID != "" || !json.Valid([]byte(rows[0].Meta)) ||
		!containsJSONField(rows[0].Meta, "message", "halfway; tests failing in X") {
		t.Fatalf("plaintext progress row = %+v", rows[0])
	}
	var latestMeta struct {
		Input struct {
			Message string `json:"message"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(rows[1].Meta), &latestMeta); err != nil {
		t.Fatalf("decode latest progress: %v", err)
	}
	if runes := []rune(latestMeta.Input.Message); len(runes) > codexCollabProgressTextRunes+1 {
		t.Fatalf("progress body ran to %d runes", len(runes))
	}
}

func containsJSONField(raw, key, want string) bool {
	var value map[string]any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return false
	}
	input, _ := value["input"].(map[string]any)
	got, _ := input[key].(string)
	return got == want
}
