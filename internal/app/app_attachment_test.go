package app

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/store"
)

// realPNGBytes returns a small (8x8) but fully-decodable PNG. The
// thumbnail generator needs an image it can actually decode; the
// pngBase64 helper above just hands back the PNG signature bytes,
// which works for upload validation but not for image.Decode.
func realPNGBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 32), G: uint8(y * 32), B: 0x80, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

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
		ID:            "thr-a",
		ProjectID:     defaultTestProjectID,
		Title:         "Thread A",
		Provider:      "claude",
		WorkspacePath: "/tmp/work",
		Model:         "claude",
		Mode:          "chat",
		CreatedAt:     time.Now().UnixMilli(),
		UpdatedAt:     time.Now().UnixMilli(),
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

	b64, err := app.GetAttachmentData(record.ThreadID, record.ID)
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
	_, err := app.GetAttachmentData("thr-a", "nope")
	if err == nil {
		t.Fatal("expected error for missing attachment")
	}
}

func TestGetAttachmentDataRejectsCrossThreadID(t *testing.T) {
	app := newAttachmentTestApp(t)

	record, err := app.UploadAttachment("thr-a", "hero.png", "image/png", pngBase64(t))
	if err != nil {
		t.Fatalf("UploadAttachment: %v", err)
	}

	_, err = app.GetAttachmentData("thr-b", record.ID)
	if err == nil {
		t.Fatal("expected cross-thread attachment rejection")
	}
	if !strings.Contains(err.Error(), "belongs to thread") {
		t.Fatalf("GetAttachmentData error = %v, want ownership rejection", err)
	}
}

func TestGetAttachmentThumbnailReturnsThumb(t *testing.T) {
	app := newAttachmentTestApp(t)
	// Use a real (decodable) PNG so the generator can read it.
	pngBytes := realPNGBytes(t)
	record, err := app.UploadAttachment("thr-a", "shot.png", "image/png", base64.StdEncoding.EncodeToString(pngBytes))
	if err != nil {
		t.Fatalf("UploadAttachment: %v", err)
	}
	got, err := app.GetAttachmentThumbnail(record.ThreadID, record.ID)
	if err != nil {
		t.Fatalf("GetAttachmentThumbnail: %v", err)
	}
	if got.MimeType != "image/png" {
		t.Fatalf("thumbnail mime = %q, want image/png", got.MimeType)
	}
	if got.Data == "" {
		t.Fatal("expected non-empty base64 thumbnail")
	}
	// The cached thumbnail should be identical on a second call (no
	// regeneration). Compare the raw decoded bytes for stability across
	// platforms — base64 representation is canonical so this is a tight
	// check.
	again, err := app.GetAttachmentThumbnail(record.ThreadID, record.ID)
	if err != nil {
		t.Fatalf("GetAttachmentThumbnail (cached): %v", err)
	}
	if again.Data != got.Data || again.MimeType != got.MimeType {
		t.Fatal("expected cached thumbnail to round-trip identically")
	}
}

func TestGetAttachmentThumbnailRejectsCrossThread(t *testing.T) {
	app := newAttachmentTestApp(t)
	thread := store.Thread{
		ID:            "thr-b",
		ProjectID:     defaultTestProjectID,
		Title:         "Thread B",
		Provider:      "claude",
		WorkspacePath: "/tmp/work-b",
		Model:         "claude",
		Mode:          "chat",
		CreatedAt:     time.Now().UnixMilli(),
		UpdatedAt:     time.Now().UnixMilli(),
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread thr-b: %v", err)
	}
	pngBytes := realPNGBytes(t)
	record, err := app.UploadAttachment("thr-a", "shot.png", "image/png", base64.StdEncoding.EncodeToString(pngBytes))
	if err != nil {
		t.Fatalf("UploadAttachment: %v", err)
	}
	if _, err := app.GetAttachmentThumbnail("thr-b", record.ID); err == nil {
		t.Fatal("expected cross-thread thumbnail rejection")
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
