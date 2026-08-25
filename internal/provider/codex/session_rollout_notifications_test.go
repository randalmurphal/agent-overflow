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

func TestRolloutSubagentNotificationLineEmitsEvent(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		childParentByThread: map[string]string{
			"child-done": "call-collab-1",
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	line := rolloutUserSubagentNotificationLine(t, "child-done", map[string]any{
		"completed": "detached child finished",
	})
	if !s.emitSubagentNotificationsFromRolloutLine(line) {
		t.Fatal("rollout notification line was not consumed")
	}

	if len(events) != 1 {
		t.Fatalf("events = %+v, want one EventSubagentNotification", events)
	}
	if events[0].Kind != provider.EventSubagentNotification {
		t.Fatalf("event kind = %q, want %q", events[0].Kind, provider.EventSubagentNotification)
	}
	if events[0].ItemID != "call-collab-1" {
		t.Fatalf("ItemID = %q, want call-collab-1", events[0].ItemID)
	}

	var meta map[string]any
	if err := json.Unmarshal(events[0].Meta, &meta); err != nil {
		t.Fatalf("meta unmarshal: %v", err)
	}
	if meta["agent_path"] != "child-done" {
		t.Errorf("meta.agent_path = %v, want child-done", meta["agent_path"])
	}
	if meta["status"] != "completed" {
		t.Errorf("meta.status = %v, want completed", meta["status"])
	}
	if meta["message"] != "detached child finished" {
		t.Errorf("meta.message = %v, want detached child finished", meta["message"])
	}
}

func TestRolloutSubagentNotificationLineEmitsWithoutProviderMapping(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}

	if !s.emitSubagentNotificationsFromRolloutLine(rolloutUserSubagentNotificationLine(t, "child-resumed", map[string]any{
		"completed": "detached child finished after resume",
	})) {
		t.Fatal("rollout notification line was not consumed")
	}

	if len(events) != 1 {
		t.Fatalf("events = %+v, want one EventSubagentNotification", events)
	}
	if events[0].Kind != provider.EventSubagentNotification {
		t.Fatalf("event kind = %q, want %q", events[0].Kind, provider.EventSubagentNotification)
	}
	if events[0].ItemID != "" {
		t.Fatalf("ItemID = %q, want empty so triage can resolve persisted launch", events[0].ItemID)
	}
}

func TestRolloutAndRawSubagentNotificationDedupes(t *testing.T) {
	var events []provider.ProviderEvent
	s := &Session{
		threadID: "parent-thread",
		pending:  make(map[int64]chan json.RawMessage),
		childParentByThread: map[string]string{
			"child-done": "call-collab-1",
		},
		onEvent: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	}
	s.setRootThreadID("parent-provider-thread")

	rawLine := rawUserSubagentNotificationLineForThread(t, "parent-provider-thread", map[string]any{
		"agent_path": "child-done",
		"status": map[string]any{
			"completed": "detached child finished",
		},
	})
	s.dispatchLine(rawLine)
	s.emitSubagentNotificationsFromRolloutLine(rolloutUserSubagentNotificationLine(t, "child-done", map[string]any{
		"completed": "detached child finished",
	}))

	var notificationCount int
	for _, evt := range events {
		if evt.Kind == provider.EventSubagentNotification {
			notificationCount++
		}
	}
	if notificationCount != 1 {
		t.Fatalf("EventSubagentNotification count = %d, want 1; events=%+v", notificationCount, events)
	}
}

func TestWatchRolloutSubagentNotificationsEmitsSplitLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-2026-06-16T00-01-18-parent-provider-thread.jsonl")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatalf("write empty rollout: %v", err)
	}

	events := make(chan provider.ProviderEvent, 1)
	s := &Session{
		threadID: "parent-thread",
		readDone: make(chan struct{}),
		onEvent: func(evt provider.ProviderEvent) {
			events <- evt
		},
	}
	s.setRootThreadID("parent-provider-thread")
	path, offset, err := prepareRolloutSubagentNotificationObserver(path, "parent-provider-thread")
	if err != nil {
		t.Fatalf("prepare rollout observer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.watchRolloutSubagentNotifications(ctx, path, offset)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("rollout watcher did not exit after cancel")
		}
	})

	line := append(rolloutUserSubagentNotificationLine(t, "child-resumed", "completed"), '\n')
	split := len(line) / 2
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := file.Write(line[:split]); err != nil {
		file.Close()
		t.Fatalf("append first half: %v", err)
	}
	select {
	case evt := <-events:
		t.Fatalf("watcher emitted before newline: %+v", evt)
	case <-time.After(rolloutSubagentNotificationPollInterval * 2):
	}
	if _, err := file.Write(line[split:]); err != nil {
		file.Close()
		t.Fatalf("append second half: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close rollout: %v", err)
	}

	select {
	case evt := <-events:
		if evt.Kind != provider.EventSubagentNotification {
			t.Fatalf("event kind = %q, want %q", evt.Kind, provider.EventSubagentNotification)
		}
		if evt.ItemID != "" {
			t.Fatalf("ItemID = %q, want empty for persisted triage resolution", evt.ItemID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for rollout notification event")
	}
}

func TestReadRolloutAppendStartsAfterExistingHistory(t *testing.T) {
	const threadID = "0199c0de-dead-beef-cafe-000000000001"
	path := filepath.Join(t.TempDir(), "rollout-2026-08-24T00-00-00-"+threadID+".jsonl")
	historical := append(rolloutUserSubagentNotificationLine(t, "child-old", "completed"), '\n')
	if err := os.WriteFile(path, historical, 0644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	// The production entry point is the observer's own preparation step: the
	// start offset IS the file's current size, so everything already on disk
	// is history and only appends are read.
	resolved, offset, err := prepareRolloutSubagentNotificationObserver(path, threadID)
	if err != nil {
		t.Fatalf("prepareRolloutSubagentNotificationObserver: %v", err)
	}
	if resolved != path {
		t.Fatalf("resolved path = %q, want %q", resolved, path)
	}
	if offset != int64(len(historical)) {
		t.Fatalf("offset = %d, want %d", offset, len(historical))
	}

	fresh := append(rolloutUserSubagentNotificationLine(t, "child-fresh", "completed"), '\n')
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := file.Write(fresh); err != nil {
		file.Close()
		t.Fatalf("append rollout: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close rollout: %v", err)
	}

	chunk, _, err := readRolloutAppend(path, offset)
	if err != nil {
		t.Fatalf("readRolloutAppend: %v", err)
	}
	if string(chunk) != string(fresh) {
		t.Fatalf("chunk = %q, want only fresh line %q", string(chunk), string(fresh))
	}
}

func TestPrepareRolloutSubagentNotificationObserverValidatesPath(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "rollout-2026-06-16T00-01-18-parent-provider-thread.jsonl")
	if err := os.WriteFile(valid, []byte("history\n"), 0644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	path, offset, err := prepareRolloutSubagentNotificationObserver(valid, "parent-provider-thread")
	if err != nil {
		t.Fatalf("valid rollout rejected: %v", err)
	}
	if path != filepath.Clean(valid) {
		t.Fatalf("path = %q, want %q", path, filepath.Clean(valid))
	}
	if offset != int64(len("history\n")) {
		t.Fatalf("offset = %d, want history length", offset)
	}

	mismatch := filepath.Join(dir, "rollout-2026-06-16T00-01-18-other-thread.jsonl")
	if err := os.WriteFile(mismatch, nil, 0644); err != nil {
		t.Fatalf("write mismatch rollout: %v", err)
	}
	if _, _, err := prepareRolloutSubagentNotificationObserver(mismatch, "parent-provider-thread"); err == nil {
		t.Fatal("expected mismatched thread id path to be rejected")
	}

	symlink := filepath.Join(dir, "rollout-2026-06-16T00-01-18-parent-provider-thread-link.jsonl")
	if err := os.Symlink(valid, symlink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, _, err := prepareRolloutSubagentNotificationObserver(symlink, "parent-provider-thread"); err == nil {
		t.Fatal("expected symlink rollout path to be rejected")
	}
}
