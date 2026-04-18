package main

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/store"
)

func newAttachmentTestApp(t *testing.T) *App {
	t.Helper()

	app := newTestAppWithStore(t)
	rootDir := filepath.Join(t.TempDir(), "attachments")
	attStore, err := attachment.NewStore(attachment.Config{RootDir: rootDir}, app.store)
	if err != nil {
		t.Fatalf("attachment.NewStore: %v", err)
	}
	app.attachments = attStore

	thread := store.Thread{
		ID:              "thr-a",
		ProjectID:     defaultTestProjectID,
		Title:           "Thread A",
		Provider:        "claude",
		WorkspacePath:   "/tmp/work",
		Model:           "claude",
		Mode: "chat",
		CreatedAt:       time.Now().UnixMilli(),
		UpdatedAt:       time.Now().UnixMilli(),
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return app
}

func pngBase64(t *testing.T) string {
	t.Helper()
	payload := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	return base64.StdEncoding.EncodeToString(payload)
}

func TestUploadAttachmentBindingRoundTrip(t *testing.T) {
	app := newAttachmentTestApp(t)

	record, err := app.UploadAttachment("thr-a", "hero.png", "image/png", pngBase64(t))
	if err != nil {
		t.Fatalf("UploadAttachment: %v", err)
	}
	if record.ID == "" || record.ThreadID != "thr-a" {
		t.Fatalf("unexpected record: %+v", record)
	}

	got, err := app.ListAttachments("thr-a")
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(got) != 1 || got[0].ID != record.ID {
		t.Fatalf("ListAttachments result: %+v", got)
	}
}

func TestGetAttachmentDataReturnsBase64(t *testing.T) {
	app := newAttachmentTestApp(t)

	record, err := app.UploadAttachment("thr-a", "hero.png", "image/png", pngBase64(t))
	if err != nil {
		t.Fatalf("UploadAttachment: %v", err)
	}

	b64, err := app.GetAttachmentData(record.ID)
	if err != nil {
		t.Fatalf("GetAttachmentData: %v", err)
	}
	if b64 == "" {
		t.Fatal("expected base64 payload")
	}
	if b64 != pngBase64(t) {
		t.Fatalf("payload mismatch: got %q want %q", b64, pngBase64(t))
	}
}

func TestGetAttachmentDataMissingReturnsError(t *testing.T) {
	app := newAttachmentTestApp(t)
	_, err := app.GetAttachmentData("nope")
	if err == nil {
		t.Fatal("expected error for missing attachment")
	}
}

func TestDeleteAttachmentBinding(t *testing.T) {
	app := newAttachmentTestApp(t)

	record, err := app.UploadAttachment("thr-a", "hero.png", "image/png", pngBase64(t))
	if err != nil {
		t.Fatalf("UploadAttachment: %v", err)
	}

	if err := app.DeleteAttachment(record.ID); err != nil {
		t.Fatalf("DeleteAttachment: %v", err)
	}

	list, err := app.ListAttachments("thr-a")
	if err != nil {
		t.Fatalf("ListAttachments after delete: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %+v", list)
	}
}

func TestUploadAttachmentRequiresInitialisedStore(t *testing.T) {
	app := &App{}
	_, err := app.UploadAttachment("thr-a", "hero.png", "image/png", pngBase64(t))
	if err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("expected init error, got %v", err)
	}
}

func TestListAttachmentsEmptyIsNonNil(t *testing.T) {
	app := newAttachmentTestApp(t)
	list, err := app.ListAttachments("thr-a")
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if list == nil {
		t.Fatal("expected non-nil empty slice")
	}
}
