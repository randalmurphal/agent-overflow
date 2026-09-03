package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
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

	if err := app.DeleteAttachment("thr-a", record.ID); err != nil {
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

// The bound method is the wire surface, so the ownership check has to
// hold THERE, not just one layer down: any client holding a token can
// call it with any id it can guess or has gone stale in a closed
// composer.
func TestDeleteAttachmentRefusesAnotherThreadsAttachment(t *testing.T) {
	app := newAttachmentTestApp(t)

	record, err := app.UploadAttachment("thr-a", "hero.png", "image/png", pngBase64(t))
	if err != nil {
		t.Fatalf("UploadAttachment: %v", err)
	}

	if err := app.DeleteAttachment("thr-b", record.ID); err == nil {
		t.Fatal("expected a foreign-thread delete to be refused")
	}

	// Row and bytes both survive the refusal.
	list, err := app.ListAttachments("thr-a")
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("row destroyed by a refused delete: %+v", list)
	}
	if _, err := app.GetAttachmentData("thr-a", record.ID); err != nil {
		t.Fatalf("bytes destroyed by a refused delete: %v", err)
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

// The bound byte accessors are image-only, and the refusal comes off the
// ROW rather than off a MIME allowlist — the attachments directory now
// holds arbitrary bytes, so "safe to serve" is a property of the kind.
// Both go through the store's ReadThreadBytes / Thumbnail, which is why
// neither method needs its own check.
func TestUploadAttachmentAcceptsFileKindButNeverServesIt(t *testing.T) {
	app := newAttachmentTestApp(t)

	record, err := app.UploadAttachment("thr-a", "report.pdf", "application/pdf",
		base64.StdEncoding.EncodeToString([]byte("%PDF-1.7\n")))
	if err != nil {
		t.Fatalf("UploadAttachment: %v", err)
	}
	if record.Kind != store.AttachmentKindFile {
		t.Fatalf("Kind: got %q want %q", record.Kind, store.AttachmentKindFile)
	}
	if record.MimeType != "application/pdf" {
		t.Fatalf("MimeType: got %q", record.MimeType)
	}

	if _, err := app.GetAttachmentData("thr-a", record.ID); !errors.Is(err, attachment.ErrNotAnImage) {
		t.Errorf("GetAttachmentData: got %v want ErrNotAnImage", err)
	}
	if _, err := app.GetAttachmentThumbnail("thr-a", record.ID); !errors.Is(err, attachment.ErrNotAnImage) {
		t.Errorf("GetAttachmentThumbnail: got %v want ErrNotAnImage", err)
	}

	// Deleting a file takes its `<id>` directory with it.
	if err := app.DeleteAttachment("thr-a", record.ID); err != nil {
		t.Fatalf("DeleteAttachment: %v", err)
	}
	if _, err := app.GetAttachmentData("thr-a", record.ID); err == nil {
		t.Fatal("expected the deleted attachment to be gone")
	}
}

// A mixed turn is the shape the whole feature turns on: the `[Image #N]`
// markers are numbered over the IMAGE subset, so a file sitting between
// two images must not consume a number, must not enter the provider
// slice, and must arrive as a prompt line on providerContent — never on
// the persisted content.
func TestResolveUserMessageEnvelopeMixedAttachmentTurn(t *testing.T) {
	app := newAttachmentTestApp(t)

	first, err := app.UploadAttachment("thr-a", "one.png", "image/png", pngBase64(t))
	if err != nil {
		t.Fatalf("upload one.png: %v", err)
	}
	file, err := app.UploadAttachment("thr-a", "report.pdf", "application/pdf",
		base64.StdEncoding.EncodeToString([]byte("%PDF-1.7\n")))
	if err != nil {
		t.Fatalf("upload report.pdf: %v", err)
	}
	second, err := app.UploadAttachment("thr-a", "two.png", "image/png", pngBase64(t))
	if err != nil {
		t.Fatalf("upload two.png: %v", err)
	}

	content := "look at [Image #1] and [Image #2]"
	resolved, err := app.resolveUserMessageEnvelope("thr-a", content, userMessageInputs{
		attachmentIDs: []string{first.ID, file.ID, second.ID},
	})
	if err != nil {
		t.Fatalf("resolveUserMessageEnvelope: %v", err)
	}

	// The provider slice is the image subset, in order, with the file gone.
	if len(resolved.providerAttachments) != 2 {
		t.Fatalf("providerAttachments = %d, want 2 (images only)", len(resolved.providerAttachments))
	}
	if resolved.providerAttachments[0].ID != first.ID || resolved.providerAttachments[1].ID != second.ID {
		t.Fatalf("provider slice lost its [Image #N] binding: %+v", resolved.providerAttachments)
	}

	// Every attachment is still on the row.
	if len(resolved.persistedAttachments) != 3 {
		t.Fatalf("persistedAttachments = %d, want 3", len(resolved.persistedAttachments))
	}
	var meta userMessageMeta
	if err := json.Unmarshal([]byte(resolved.userMessageMeta), &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	wantKinds := []string{store.AttachmentKindImage, store.AttachmentKindFile, store.AttachmentKindImage}
	if len(meta.Attachments) != 3 {
		t.Fatalf("meta attachments = %d, want 3", len(meta.Attachments))
	}
	for i, want := range wantKinds {
		if meta.Attachments[i].Kind != want {
			t.Errorf("meta attachment %d kind = %q, want %q", i, meta.Attachments[i].Kind, want)
		}
	}

	// The line rides providerContent only.
	if resolved.content != content {
		t.Fatalf("persisted content changed: %q", resolved.content)
	}
	_, path, err := app.attachments.PathForThread("thr-a", file.ID)
	if err != nil {
		t.Fatalf("PathForThread: %v", err)
	}
	wantLine := attachment.PromptLine(file, path)
	if resolved.providerContent != content+"\n\n"+wantLine {
		t.Fatalf("providerContent =\n%q\nwant\n%q", resolved.providerContent, content+"\n\n"+wantLine)
	}
	if strings.Contains(wantLine, "[Image #") {
		t.Fatal("the file line must carry no image marker")
	}
}

// The cap is on the union of both kinds: an attachment costs a slot
// whichever way it is delivered.
func TestResolveSendMessageAttachmentsCapsBothKindsTogether(t *testing.T) {
	app := newAttachmentTestApp(t)

	ids := make([]string, 0, attachment.DefaultMaxCount+1)
	for i := range attachment.DefaultMaxCount + 1 {
		mime, data := "image/png", pngBase64(t)
		if i%2 == 1 {
			mime, data = "application/pdf", base64.StdEncoding.EncodeToString([]byte("%PDF-1.7\n"))
		}
		record, err := app.UploadAttachment("thr-a", "a", mime, data)
		if err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
		ids = append(ids, record.ID)
	}
	if _, err := app.resolveSendMessageAttachments("thr-a", ids); err == nil ||
		!strings.Contains(err.Error(), "too many attachments") {
		t.Fatalf("expected the union cap to fire, got %v", err)
	}
}
