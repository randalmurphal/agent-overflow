package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func TestResumedIdleChildMailboxObservesAppendAndStopsWithSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	deliveries := make(chan provider.ProviderEvent, 4)
	s := newMultiAgentV2RoutingSession(t, func(e provider.ProviderEvent) {
		if e.Kind == provider.EventSubagentNotification {
			deliveries <- e
		}
	})
	s.ctx = ctx
	s.readDone = make(chan struct{})
	s.rolloutTail.path = "resumed-root"
	s.rolloutTail.started = true
	s.registerHistoricalChildOwnership("root-provider-thread", "child", "/root/worker", "spawn")
	path := filepath.Join(t.TempDir(), "rollout-child.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s.registerChildMailboxTail("child", path)
	t.Cleanup(func() { cancel(); s.collabAsyncWG.Wait() })
	deliveredAt := time.Date(2026, 9, 8, 10, 11, 12, 0, time.UTC)
	envelope := func(id string) []byte {
		b, err := json.Marshal(map[string]any{"timestamp": deliveredAt, "type": "response_item", "payload": map[string]any{"id": id, "type": "agent_message", "author": "/root", "recipient": "/root/worker", "content": []map[string]string{{"type": "input_text", "text": "Message Type: MESSAGE\nTask name: /root/worker\nSender: /root\nPayload:\nhello"}}}})
		if err != nil {
			t.Fatal(err)
		}
		return append(b, '\n')
	}
	appendRecord := func(data []byte) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.Write(data)
		closeErr := f.Close()
		if writeErr != nil {
			t.Fatal(writeErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	// The initial pending read covers appends racing watcher registration.
	appendRecord(envelope("first"))
	wait := func() {
		select {
		case e := <-deliveries:
			if e.ParentToolUseID != "spawn" || !e.Timestamp.Equal(deliveredAt) {
				t.Fatalf("scope=%+v", e)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("mailbox append was missed")
		}
	}
	wait()
	// Let the idle reader drain, then verify file notification reactivates it.
	time.Sleep(2 * rolloutSubagentNotificationPollInterval)
	appendRecord(envelope("second"))
	wait()
	cancel()
	done := make(chan struct{})
	go func() { s.collabAsyncWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("mailbox worker outlived session cancellation")
	}
}
