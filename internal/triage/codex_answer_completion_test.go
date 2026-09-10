package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// rootAnswer is the child's FINAL_ANSWER envelope as the wire delivers it to
// the root agent: recipient "/root" and a native delivery id.
func rootAnswer(t *testing.T, deliveryID, message string) json.RawMessage {
	t.Helper()
	meta, err := json.Marshal(map[string]any{
		"agent_path": "/root/child", "status": "completed", "message_type": "FINAL_ANSWER",
		"mailbox_delivery": true, "recipient": "/root", "delivery_id": "item:" + deliveryID, "message": message,
	})
	if err != nil {
		t.Fatalf("marshal answer: %v", err)
	}
	return meta
}

func childTurnStatus(t *testing.T, router *Router, threadID, launchID, childID, turnID, status string) {
	t.Helper()
	meta, err := json.Marshal(map[string]any{"agent_path": childID, "status": status})
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventSubagentStatus, ThreadID: threadID, ItemID: launchID,
		TurnID: turnID, Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("child %s status %s: %v", childID, status, err)
	}
}

func completeParentTurn(t *testing.T, router *Router, threadID string) {
	t.Helper()
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: threadID, TurnID: "turn-0",
		TurnComplete: normalTurnCompleteMeta(),
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("turn complete: %v", err)
	}
}

func payloadPreview(t *testing.T, st *store.Store, threadID, payloadID string) string {
	t.Helper()
	meta, err := st.GetPayloadMeta(threadID, payloadID)
	if err != nil {
		t.Fatalf("payload meta %s: %v", payloadID, err)
	}
	var fields struct {
		Preview string `json:"preview"`
	}
	if err := json.Unmarshal([]byte(meta.Meta), &fields); err != nil {
		t.Fatalf("decode payload meta %s: %v", meta.Meta, err)
	}
	return fields.Preview
}

func deliveryRowsFor(t *testing.T, st *store.Store, threadID string) []store.Item {
	t.Helper()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var out []store.Item
	for _, item := range items {
		if strings.HasPrefix(item.ID, "collab-progress:") {
			out = append(out, item)
		}
	}
	return out
}

func heldSpawn(t *testing.T) (*Router, *store.Store) {
	t.Helper()
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	seedCodexSpawnCard(t, router, st, "t1", "spawn", "child")
	childTurnStatus(t, router, "t1", "spawn", "child", "A", "running")
	childTurnStatus(t, router, "t1", "spawn", "child", "A", "completed")
	if rows := completionRowsFor(t, st, "t1", "spawn"); len(rows) != 0 {
		t.Fatalf("terminal inside an open parent turn wrote before the answer: %+v", rows)
	}
	if live := router.ListLiveCodexAgentTasks("t1"); len(live) != 0 {
		t.Fatalf("a terminal child stays in the tray while its row is held: %+v", live)
	}
	return router, st
}

// The child's FINAL_ANSWER is the completion row's payload and preview,
// and no separate delivery row is minted for it.
func TestCodexCompletionWaitsForTheAnswerAndCarriesIt(t *testing.T) {
	router, st := heldSpawn(t)

	deliverMailbox(t, router, "t1", "spawn", rootAnswer(t, "final-1", "Reviewer verdict: ship it."))

	rows := completionRowsFor(t, st, "t1", "spawn")
	if len(rows) != 1 || rows[0].ID != "complete:spawn:turn:A" || rows[0].Status != statusCompleted {
		t.Fatalf("completion rows after answer: %+v", rows)
	}
	if rows[0].PayloadID == "" {
		t.Fatalf("completion carries no answer payload: %+v", rows[0])
	}
	if got := payloadPreview(t, st, "t1", rows[0].PayloadID); got != "Reviewer verdict: ship it." {
		t.Fatalf("completion preview = %q", got)
	}
	data, err := st.GetPayloadData("t1", rows[0].PayloadID)
	if err != nil || string(data) != "Reviewer verdict: ship it." {
		t.Fatalf("completion payload = %q err=%v", data, err)
	}
	if deliveries := deliveryRowsFor(t, st, "t1"); len(deliveries) != 0 {
		t.Fatalf("answer consumed by the completion still minted delivery rows: %+v", deliveries)
	}
}

func TestCodexEncryptedAnswerCompletionCarriesThePlaceholder(t *testing.T) {
	router, st := heldSpawn(t)

	meta, err := json.Marshal(map[string]any{
		"agent_path": "/root/child", "status": "completed", "message_type": "FINAL_ANSWER",
		"mailbox_delivery": true, "recipient": "/root", "delivery_id": "item:final-1", "encrypted": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	deliverMailbox(t, router, "t1", "spawn", meta)

	rows := completionRowsFor(t, st, "t1", "spawn")
	if len(rows) != 1 || rows[0].PayloadID == "" {
		t.Fatalf("completion rows after encrypted answer: %+v", rows)
	}
	if got := payloadPreview(t, st, "t1", rows[0].PayloadID); got != codexEncryptedMessagePlaceholder {
		t.Fatalf("encrypted preview = %q", got)
	}
}

// The parent turn ending is the last moment the envelope can land inside
// it; the row is written answerless there, and an answer sampled in a
// later turn is the delivery row it already was.
func TestCodexHeldCompletionIsWrittenAnswerlessWhenTheParentTurnEnds(t *testing.T) {
	router, st := heldSpawn(t)

	completeParentTurn(t, router, "t1")
	rows := completionRowsFor(t, st, "t1", "spawn")
	if len(rows) != 1 || rows[0].ID != "complete:spawn:turn:A" || rows[0].PayloadID != "" {
		t.Fatalf("completion rows after parent turn end: %+v", rows)
	}

	seedOpenTurn(t, router, st, "t1", 1)
	deliverMailbox(t, router, "t1", "spawn", rootAnswer(t, "final-late", "Late verdict."))
	if after := completionRowsFor(t, st, "t1", "spawn"); len(after) != 1 || after[0].PayloadID != "" {
		t.Fatalf("late answer altered or duplicated the completion: %+v", after)
	}
	if deliveries := deliveryRowsFor(t, st, "t1"); len(deliveries) != 1 {
		t.Fatalf("late answer must land as its own delivery row: %+v", deliveries)
	}
}

func TestCodexNewExecutionWritesThePreviousHeldCompletionFirst(t *testing.T) {
	router, st := heldSpawn(t)

	childTurnStatus(t, router, "t1", "spawn", "child", "B", "running")
	rows := completionRowsFor(t, st, "t1", "spawn")
	if len(rows) != 1 || rows[0].ID != "complete:spawn:turn:A" || rows[0].PayloadID != "" {
		t.Fatalf("first execution after the second started: %+v", rows)
	}

	childTurnStatus(t, router, "t1", "spawn", "child", "B", "completed")
	deliverMailbox(t, router, "t1", "spawn", rootAnswer(t, "final-2", "Second verdict."))
	rows = completionRowsFor(t, st, "t1", "spawn")
	if len(rows) != 2 || rows[1].ID != "complete:spawn:turn:B" {
		t.Fatalf("completion rows after second execution: %+v", rows)
	}
	if got := payloadPreview(t, st, "t1", rows[1].PayloadID); got != "Second verdict." {
		t.Fatalf("second completion preview = %q", got)
	}
	if rows[0].PayloadID != "" {
		t.Fatalf("second answer reached the first completion: %+v", rows[0])
	}
}

// Codex renders no envelope for an interrupted child, so nothing is waited for.
func TestCodexInterruptedExecutionCompletesImmediately(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	seedCodexSpawnCard(t, router, st, "t1", "spawn", "child")
	childTurnStatus(t, router, "t1", "spawn", "child", "A", "running")
	childTurnStatus(t, router, "t1", "spawn", "child", "A", "interrupted")

	rows := completionRowsFor(t, st, "t1", "spawn")
	if len(rows) != 1 || rows[0].Status != statusKilled {
		t.Fatalf("interrupted completion rows: %+v", rows)
	}
}

func TestCodexTerminalAfterTheParentTurnCompletesImmediately(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	seedCodexSpawnCard(t, router, st, "t1", "spawn", "child")
	childTurnStatus(t, router, "t1", "spawn", "child", "A", "running")
	completeParentTurn(t, router, "t1")

	childTurnStatus(t, router, "t1", "spawn", "child", "A", "completed")
	rows := completionRowsFor(t, st, "t1", "spawn")
	if len(rows) != 1 || rows[0].ID != "complete:spawn:turn:A" {
		t.Fatalf("completion rows for a terminal between turns: %+v", rows)
	}
}

func TestCodexCleanupWritesHeldCompletionsAnswerless(t *testing.T) {
	router, st := heldSpawn(t)

	router.CleanupThread("t1")

	rows := completionRowsFor(t, st, "t1", "spawn")
	if len(rows) != 1 || rows[0].ID != "complete:spawn:turn:A" || rows[0].Status != statusCompleted {
		t.Fatalf("completion rows after cleanup: %+v", rows)
	}
}
