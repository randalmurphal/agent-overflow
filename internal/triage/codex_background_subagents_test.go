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

// The delivered/idle axis describes the CURRENT child turn. A resumed child
// that goes terminal again with nothing drained is idle, not delivered.
func TestResumeClearsTheDeliveredStamp(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	seedCodexSpawnCard(t, router, st, "t1", "spawn-1", "child-1")

	deliveredAt := func() int64 {
		t.Helper()
		launch, found, err := st.GetThreadItem("t1", "spawn-1")
		if err != nil || !found {
			t.Fatalf("launch: found=%v err=%v", found, err)
		}
		var parsed struct {
			At int64 `json:"codex_collab_delivered_at"`
		}
		if err := json.Unmarshal([]byte(launch.Meta), &parsed); err != nil {
			t.Fatalf("decode launch meta: %v", err)
		}
		return parsed.At
	}

	deliverMailbox(t, router, "t1", "spawn-1", mailboxDelivery(t, "/root/reviewer", "First answer"))
	if deliveredAt() == 0 {
		t.Fatal("first answer did not stamp delivered")
	}

	resumeChild(t, router, "t1", "spawn-1", "child-1")
	if at := deliveredAt(); at != 0 {
		t.Fatalf("delivered stamp survived the resume: %d", at)
	}

	deliverMailbox(t, router, "t1", "spawn-1", mailboxDelivery(t, "/root/reviewer", "Second answer"))
	if deliveredAt() == 0 {
		t.Fatal("the resumed turn's answer did not stamp delivered")
	}
}

// A PLAINTEXT progress note's body is the only copy of that text — nothing
// else persists it. An encrypted envelope has no body and keeps none.
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

	deliverMailbox(t, router, "t1", "spawn-1", progress("halfway; tests failing in X\nsecond line dropped"))
	deliverMailbox(t, router, "t1", "spawn-1", progress(""))

	interactions := launchInteractions(t, st, "t1", "spawn-1")
	if len(interactions) != 2 {
		t.Fatalf("interactions = %+v, want two progress beats", interactions)
	}
	if interactions[0].Kind != codexCollabInteractionProgress ||
		interactions[0].Text != "halfway; tests failing in X" {
		t.Fatalf("plaintext beat = %+v", interactions[0])
	}
	if interactions[1].Text != "" {
		t.Fatalf("encrypted beat kept a body: %+v", interactions[1])
	}

	long := ""
	for range codexCollabProgressTextRunes + 40 {
		long += "x"
	}
	deliverMailbox(t, router, "t1", "spawn-1", progress(long))
	interactions = launchInteractions(t, st, "t1", "spawn-1")
	body := interactions[len(interactions)-1].Text
	if runes := []rune(body); len(runes) > codexCollabProgressTextRunes+1 {
		t.Fatalf("progress body ran to %d runes", len(runes))
	}
}
