package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

func seedPayloadOwner(t *testing.T, app *App, thread store.Thread, payload store.Payload, itemID string) {
	t.Helper()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(%s): %v", thread.ID, err)
	}
	item := store.Item{
		ID:          itemID,
		ThreadID:    thread.ID,
		TurnIndex:   1,
		Kind:        "tool_call",
		Role:        "assistant",
		Status:      "completed",
		Summary:     "Bash: echo hi",
		ToolName:    "Bash",
		PayloadID:   payload.ID,
		PayloadKind: payload.Kind,
		CreatedAt:   payload.CreatedAt,
		UpdatedAt:   payload.CreatedAt,
	}
	if _, err := app.store.UpsertItem(item, &payload); err != nil {
		t.Fatalf("UpsertItem(%s): %v", itemID, err)
	}
}

func TestGetPayloadBindingsRequireOwningThread(t *testing.T) {
	app := newTestAppWithStore(t)
	now := time.Now().UnixMilli()
	owner := testThread("thread-owner")
	other := testThread("thread-other")
	if err := app.store.CreateThread(owner); err != nil {
		t.Fatalf("CreateThread(owner): %v", err)
	}
	if err := app.store.CreateThread(other); err != nil {
		t.Fatalf("CreateThread(other): %v", err)
	}

	payload := store.Payload{
		ID:        "payload-owned",
		Kind:      "command_output",
		Meta:      "{}",
		Data:      []byte("hello bytes"),
		CreatedAt: now,
	}
	if _, err := app.store.UpsertItem(store.Item{
		ID:          "owner-item",
		ThreadID:    owner.ID,
		TurnIndex:   1,
		Kind:        "tool_call",
		Role:        "assistant",
		Status:      "completed",
		Summary:     "Bash: echo hi",
		ToolName:    "Bash",
		PayloadID:   payload.ID,
		PayloadKind: payload.Kind,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, &payload); err != nil {
		t.Fatalf("UpsertItem(owner-item): %v", err)
	}

	got, err := app.GetPayloadData(owner.ID, payload.ID)
	if err != nil {
		t.Fatalf("GetPayloadData(owner): %v", err)
	}
	if got.Data != "hello bytes" {
		t.Fatalf("GetPayloadData(owner) = %q, want hello bytes", got.Data)
	}

	preview, err := app.GetPayloadPreview(owner.ID, payload.ID, 5)
	if err != nil {
		t.Fatalf("GetPayloadPreview(owner): %v", err)
	}
	if preview.Data != "hello" {
		t.Fatalf("GetPayloadPreview(owner) = %q, want hello", preview.Data)
	}

	chunk, err := app.GetPayloadChunk(owner.ID, payload.ID, 5, 64)
	if err != nil {
		t.Fatalf("GetPayloadChunk(owner): %v", err)
	}
	if chunk.Data != " bytes" {
		t.Fatalf("GetPayloadChunk(owner) = %q, want requested raw slice", chunk.Data)
	}

	if _, err := app.GetPayloadData(other.ID, payload.ID); err == nil {
		t.Fatal("GetPayloadData(other): err = nil, want ownership error")
	}
	if _, err := app.GetPayloadPreview(other.ID, payload.ID, 5); err == nil {
		t.Fatal("GetPayloadPreview(other): err = nil, want ownership error")
	}
	if _, err := app.GetPayloadChunk(other.ID, payload.ID, 0, 5); err == nil {
		t.Fatal("GetPayloadChunk(other): err = nil, want ownership error")
	}
}

func TestGetPayloadDataFlushesLiveThinkingBuffer(t *testing.T) {
	app := newTestAppWithStore(t)
	app.triage = triage.NewRouter(app.store, func(string, any) {})
	thread := testThread("thread-live-thinking")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventThinking,
		ThreadID:  thread.ID,
		Content:   "first",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("thinking first: %v", err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventThinking,
		ThreadID:  thread.ID,
		Content:   " second",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("thinking second: %v", err)
	}

	got, err := app.GetPayloadData(thread.ID, "thinking:think:0:1")
	if err != nil {
		t.Fatalf("GetPayloadData: %v", err)
	}
	if got.Data != "first second" {
		t.Fatalf("GetPayloadData = %q, want flushed thinking payload", got.Data)
	}
}

func TestGetPayloadDataIncludesThinkingDeltaBeforeWireEmission(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-live-thinking-wire-order")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	var payloadDuringDelta string
	app.triage = triage.NewRouter(app.store, func(eventName string, data any) {
		if eventName != "provider:item_event" {
			return
		}
		evt, ok := data.(triage.ItemStreamEvent)
		if !ok || evt.Action != "delta" || evt.Kind != "thinking" {
			return
		}
		got, err := app.GetPayloadData(thread.ID, "thinking:think:0:1")
		if err != nil {
			t.Fatalf("GetPayloadData during delta emission: %v", err)
		}
		payloadDuringDelta = got.Data
	})

	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventThinking,
		ThreadID:  thread.ID,
		Content:   "first",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("thinking first: %v", err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventThinking,
		ThreadID:  thread.ID,
		Content:   " second",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("thinking second: %v", err)
	}

	if payloadDuringDelta != "first second" {
		t.Fatalf("GetPayloadData during delta emission = %q, want full visible thinking text", payloadDuringDelta)
	}
}

func TestGetPayloadDataIncludesAssistantTextDeltaBeforeWireEmission(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-live-text-wire-order")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	var payloadDuringDelta string
	app.triage = triage.NewRouter(app.store, func(eventName string, data any) {
		if eventName != "provider:item_event" {
			return
		}
		evt, ok := data.(triage.ItemStreamEvent)
		if !ok || evt.Action != "delta" || evt.Kind != "assistant_text" {
			return
		}
		payloadID := triage.AssistantTextPayloadID(evt.ItemID)
		got, err := app.GetPayloadData(thread.ID, payloadID)
		if err != nil {
			t.Fatalf("GetPayloadData during delta emission: %v", err)
		}
		payloadDuringDelta = got.Data
	})

	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  thread.ID,
		Content:   "first",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text first: %v", err)
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind:      provider.EventTextDelta,
		ThreadID:  thread.ID,
		Content:   " second",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text second: %v", err)
	}

	if payloadDuringDelta != "first second" {
		t.Fatalf("GetPayloadData during delta emission = %q, want full visible assistant text", payloadDuringDelta)
	}
}

// TestSavePayloadToFileWritesBytesAndReturnsPath covers the happy path:
// the picker returns a chosen path, SavePayloadToFile writes the
// payload body to disk, and the returned value is the chosen path.
func TestSavePayloadToFileWritesBytesAndReturnsPath(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-save-payload")

	// Seed a payload + item that references it.
	payload := store.Payload{
		ID:        "payload-1",
		Kind:      "command_output",
		Meta:      "{}",
		Data:      []byte("hello bytes"),
		CreatedAt: time.Now().UnixMilli(),
	}
	seedPayloadOwner(t, app, thread, payload, "item-1")

	dest := filepath.Join(t.TempDir(), "out.txt")
	var pickedFilename string
	app.savePayloadPickerFn = func(filename string) (string, error) {
		pickedFilename = filename
		return dest, nil
	}

	path, err := app.SavePayloadToFile(thread.ID, "payload-1")
	if err != nil {
		t.Fatalf("SavePayloadToFile: %v", err)
	}
	if path != dest {
		t.Errorf("returned path = %q, want %q", path, dest)
	}
	data, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("read dest: %v", readErr)
	}
	if string(data) != "hello bytes" {
		t.Errorf("file contents = %q, want %q", string(data), "hello bytes")
	}
	// The filename suggestion should come from the tool name when
	// present (the dialog UI uses it as the default save name).
	if pickedFilename != "Bash.txt" {
		t.Errorf("picker filename = %q, want Bash.txt", pickedFilename)
	}

	// Saved payloads land at 0o600 — the file may carry tool output or
	// provider responses the user wouldn't want world-readable on a
	// shared host. Pin the mode so a future regression that silently
	// loosens it is caught immediately.
	info, statErr := os.Stat(dest)
	if statErr != nil {
		t.Fatalf("stat dest: %v", statErr)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 0o600 (saved payloads are user-private)", got)
	}
}

// TestSavePayloadToFileCancelledReturnsEmptyPath verifies that when the
// dialog is cancelled (picker returns empty path + nil error) the
// function returns the (empty, nil) pair and does NOT write anything
// to disk.
func TestSavePayloadToFileCancelledReturnsEmptyPath(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-save-cancel")

	payload := store.Payload{
		ID:        "payload-cancel",
		Kind:      "diff",
		Meta:      "{}",
		Data:      []byte("secret"),
		CreatedAt: time.Now().UnixMilli(),
	}
	seedPayloadOwner(t, app, thread, payload, "item-cancel")

	app.savePayloadPickerFn = func(filename string) (string, error) {
		return "", nil // user cancelled
	}

	path, err := app.SavePayloadToFile(thread.ID, "payload-cancel")
	if err != nil {
		t.Fatalf("SavePayloadToFile on cancel: %v", err)
	}
	if path != "" {
		t.Errorf("path on cancel = %q, want empty", path)
	}
}

// TestSavePayloadToFileDialogErrorSurfaces checks that a dialog error
// is wrapped and surfaced to the caller rather than silently
// swallowed.
func TestSavePayloadToFileDialogErrorSurfaces(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-save-err")

	payload := store.Payload{
		ID:        "p-err",
		Kind:      "thinking",
		Meta:      "{}",
		Data:      []byte("thoughts"),
		CreatedAt: time.Now().UnixMilli(),
	}
	seedPayloadOwner(t, app, thread, payload, "item-err")

	app.savePayloadPickerFn = func(filename string) (string, error) {
		return "", errors.New("boom")
	}

	if _, err := app.SavePayloadToFile(thread.ID, "p-err"); err == nil {
		t.Fatal("SavePayloadToFile: err = nil, want wrapped dialog error")
	}
}

func TestSavePayloadToFileRejectsPayloadFromOtherThread(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-owner")
	otherThread := testThread("thread-other")
	payload := store.Payload{
		ID:        "payload-private",
		Kind:      "thinking",
		Meta:      "{}",
		Data:      []byte("private"),
		CreatedAt: time.Now().UnixMilli(),
	}
	seedPayloadOwner(t, app, thread, payload, "item-private")
	if err := app.store.CreateThread(otherThread); err != nil {
		t.Fatalf("create other thread: %v", err)
	}
	app.savePayloadPickerFn = func(filename string) (string, error) {
		t.Fatalf("picker should not open for payload outside owning thread")
		return "", nil
	}

	if _, err := app.SavePayloadToFile(otherThread.ID, payload.ID); err == nil {
		t.Fatal("SavePayloadToFile wrong thread: err = nil, want ownership error")
	}
}
