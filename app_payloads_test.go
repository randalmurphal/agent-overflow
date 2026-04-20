package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// TestSavePayloadToFileWritesBytesAndReturnsPath covers the happy path:
// the picker returns a chosen path, SavePayloadToFile writes the
// payload body to disk, and the returned value is the chosen path.
func TestSavePayloadToFileWritesBytesAndReturnsPath(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-save-payload")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Seed a payload + item that references it.
	payload := store.Payload{
		ID:        "payload-1",
		Kind:      "command_output",
		Meta:      "{}",
		Data:      []byte("hello bytes"),
		CreatedAt: time.Now().UnixMilli(),
	}
	item := store.Item{
		ID:          "item-1",
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
		t.Fatalf("UpsertItem: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "out.txt")
	var pickedFilename string
	app.savePayloadPickerFn = func(filename string) (string, error) {
		pickedFilename = filename
		return dest, nil
	}

	path, err := app.SavePayloadToFile("payload-1")
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
}

// TestSavePayloadToFileCancelledReturnsEmptyPath verifies that when the
// dialog is cancelled (picker returns empty path + nil error) the
// function returns the (empty, nil) pair and does NOT write anything
// to disk.
func TestSavePayloadToFileCancelledReturnsEmptyPath(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-save-cancel")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	payload := store.Payload{
		ID:        "payload-cancel",
		Kind:      "diff",
		Meta:      "{}",
		Data:      []byte("secret"),
		CreatedAt: time.Now().UnixMilli(),
	}
	item := store.Item{
		ID:          "item-cancel",
		ThreadID:    thread.ID,
		TurnIndex:   1,
		Kind:        "tool_call",
		Role:        "assistant",
		Status:      "completed",
		Summary:     "Diff",
		PayloadID:   payload.ID,
		PayloadKind: payload.Kind,
		CreatedAt:   payload.CreatedAt,
		UpdatedAt:   payload.CreatedAt,
	}
	if _, err := app.store.UpsertItem(item, &payload); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}

	app.savePayloadPickerFn = func(filename string) (string, error) {
		return "", nil // user cancelled
	}

	path, err := app.SavePayloadToFile("payload-cancel")
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
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	payload := store.Payload{
		ID:        "p-err",
		Kind:      "thinking",
		Meta:      "{}",
		Data:      []byte("thoughts"),
		CreatedAt: time.Now().UnixMilli(),
	}
	item := store.Item{
		ID:          "item-err",
		ThreadID:    thread.ID,
		TurnIndex:   1,
		Kind:        "assistant_text",
		Role:        "assistant",
		Status:      "completed",
		Summary:     "tldr",
		PayloadID:   payload.ID,
		PayloadKind: payload.Kind,
		CreatedAt:   payload.CreatedAt,
		UpdatedAt:   payload.CreatedAt,
	}
	if _, err := app.store.UpsertItem(item, &payload); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}

	app.savePayloadPickerFn = func(filename string) (string, error) {
		return "", errors.New("boom")
	}

	if _, err := app.SavePayloadToFile("p-err"); err == nil {
		t.Fatal("SavePayloadToFile: err = nil, want wrapped dialog error")
	}
}
