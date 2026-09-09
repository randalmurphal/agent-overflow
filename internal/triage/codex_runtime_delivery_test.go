package triage

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestAuditDelayedOldAnswerMustNotStopNewTurn(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn-1", "child-1")
	if err := r.Handle(provider.ProviderEvent{Kind: provider.EventSubagentStatus, ThreadID: "t1", ItemID: "spawn-1", Meta: json.RawMessage(`{"agent_path":"child-1","status":"completed"}`), Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	resumeChild(t, r, "t1", "spawn-1", "child-1")
	deliverMailbox(t, r, "t1", "spawn-1", json.RawMessage(`{"agent_path":"/root/reviewer","recipient":"/root","message_type":"FINAL_ANSWER","mailbox_delivery":true,"delivery_id":"item:old-answer","message":"Old turn answer delivered after follow-up started"}`))
	item, _, err := st.GetThreadItem("t1", "spawn-1")
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Active bool `json:"live_background_active"`
	}
	if err := json.Unmarshal([]byte(r.codexAgentRuntimeOrLaunch(item).Meta), &meta); err != nil {
		t.Fatal(err)
	}
	if !meta.Active {
		t.Fatal("delayed old FINAL_ANSWER cleared live_background_active while the new child turn is running")
	}
}

func TestCodexRecipientMessagePersistsEncryptedPlaceholderOnce(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn-1", "child-1")
	e := provider.ProviderEvent{Kind: provider.EventSubagentNotification, ThreadID: "t1", ParentToolUseID: "spawn-1", Meta: json.RawMessage(`{"agent_path":"/root","recipient":"/root/worker","message_type":"NEW_TASK","mailbox_delivery":true,"delivery_id":"item:message-1","encrypted":true}`), Timestamp: time.Now()}
	if err := r.Handle(e); err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(e); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("spawn-1\x00/root\x00item:message-1"))
	id := fmt.Sprintf("agent-message:%x", digest[:16])
	item, found, err := st.GetThreadItem("t1", id)
	if err != nil || !found {
		t.Fatalf("message found=%v err=%v", found, err)
	}
	if item.ParentID != "spawn-1" || item.Role != "user" || item.Summary != "Message content is encrypted by Codex." || !strings.Contains(item.Meta, `"wire_only":true`) {
		t.Fatalf("message=%+v", item)
	}
}

func TestNativeMailboxIdentityDoesNotDependOnCurrentExecution(t *testing.T) {
	delivery := codexSubagentSignalMeta{AgentPath: "/root/worker", Recipient: "/root", MessageType: "FINAL_ANSWER", MailboxDelivery: true, DeliveryID: "item:message-1", Message: "done"}
	if codexMailboxCompletionID("spawn", 0, delivery) != codexMailboxCompletionID("spawn", 5, delivery) {
		t.Fatal("native delivery identity changed across execution")
	}
	other := delivery
	other.DeliveryID = "item:message-2"
	if codexMailboxCompletionID("spawn", 0, delivery) == codexMailboxCompletionID("spawn", 0, other) {
		t.Fatal("distinct native deliveries collapsed")
	}
}

func TestModernFinalAnswerIsDeliveryEvidenceWithEncryptedPlaceholder(t *testing.T) {
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "t1")
	seedOpenTurn(t, r, st, "t1", 0)
	seedCodexSpawnCard(t, r, st, "t1", "spawn-1", "child-1")
	resumeChild(t, r, "t1", "spawn-1", "child-1")
	deliverMailbox(t, r, "t1", "spawn-1", json.RawMessage(`{"agent_path":"/root/worker","recipient":"/root","message_type":"FINAL_ANSWER","mailbox_delivery":true,"delivery_id":"item:final-1","encrypted":true}`))
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.CompletionOf == "spawn-1" {
			t.Fatal("delivery invented an execution completion")
		}
		if item.ToolName != "send_input" {
			continue
		}
		found = true
		if item.ParentID != "" || !strings.Contains(item.Meta, `"messageType":"FINAL_ANSWER"`) {
			t.Fatalf("delivery=%+v", item)
		}
		body, err := st.GetPayloadData("t1", item.PayloadID)
		if err != nil || !strings.Contains(string(body), "Message content is encrypted by Codex.") {
			t.Fatalf("payload=%s err=%v", body, err)
		}
	}
	if !found {
		t.Fatal("answer receipt missing")
	}
	launch, _, err := st.GetThreadItem("t1", "spawn-1")
	if err != nil || !strings.Contains(r.codexAgentRuntimeOrLaunch(launch).Meta, `"live_background_active":true`) {
		t.Fatalf("execution changed: %+v err=%v", launch, err)
	}
}
