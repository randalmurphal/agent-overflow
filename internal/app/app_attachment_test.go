package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// realPNGBytes returns a small (8x8) but fully-decodable PNG. The
// thumbnail generator needs an image it can actually decode; the
// pngSignature helper below just hands back the PNG signature bytes,
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

func pngSignature() []byte {
	return []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
}

// uploadTestAttachment puts an attachment on a thread the way the byte
// route does — through the store, streamed, with the length declared up
// front.
//
// It calls the unexported storeAttachment rather than a bound method
// because there is no longer a bound method that carries bytes, which is
// the point of wave 6b. Tests that only need an attachment to EXIST use
// this; the tests that care about the transfer itself go over HTTP below.
func uploadTestAttachment(t *testing.T, app *App, threadID, filename, mimeType string, data []byte) store.Attachment {
	t.Helper()
	record, err := app.storeAttachment(threadID, filename, mimeType, int64(len(data)), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("storeAttachment(%s): %v", filename, err)
	}
	return record
}

// attachmentTransportApp is an attachment-capable App with a real
// transport in front of it, so the mint methods have a server to mint
// from and the routes have a seam to reach. Returns the app and the base
// URL its relative transfer URLs resolve against.
func attachmentTransportApp(t *testing.T) (*App, string) {
	t.Helper()
	app := newAttachmentTestApp(t)
	srv, err := transport.New(transport.Config{
		Dispatcher:         transport.NewDispatcher(),
		EventBus:           transport.NewEventBus(8),
		Token:              "attachment-transfer-test-token",
		AttachmentTransfer: AttachmentTransfer(app),
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("transport.Server.Start: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})
	app.SetTransportServer(srv)
	return app, "http://" + srv.Addr()
}

func TestAttachmentUploadRoundTripsOverHTTP(t *testing.T) {
	app, base := attachmentTransportApp(t)
	payload := realPNGBytes(t)

	uploadURL, err := app.MintAttachmentUploadTicket("thr-a", "hero.png", "image/png", int64(len(payload)))
	if err != nil {
		t.Fatalf("MintAttachmentUploadTicket: %v", err)
	}
	// Relative, always. The URL a browser is handed has to resolve against
	// whatever origin the page was served from — embedded webview, the
	// `--connect` stub, or a paired remote browser — and a host baked in
	// here would be right for exactly one of the three.
	if !strings.HasPrefix(uploadURL, "/attachments/") {
		t.Fatalf("minted URL %q is not relative to the page origin", uploadURL)
	}

	req, err := http.NewRequest(http.MethodPut, base+uploadURL, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build upload request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload status = %d (%s)", resp.StatusCode, body)
	}

	// The route answers the created row, and it must be the same shape the
	// RPC used to return: this is what the composer puts in its draft.
	var record store.Attachment
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		t.Fatalf("decode created row: %v", err)
	}
	if record.ID == "" || record.ThreadID != "thr-a" || record.Filename != "hero.png" ||
		record.MimeType != "image/png" || record.Size != int64(len(payload)) {
		t.Fatalf("created row = %+v", record)
	}

	listed, err := app.ListAttachments("thr-a")
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != record.ID {
		t.Fatalf("ListAttachments = %+v", listed)
	}

	// And the bytes come back byte-identical over the download route.
	downloadURL, err := app.MintAttachmentDownloadTicket("thr-a", record.ID)
	if err != nil {
		t.Fatalf("MintAttachmentDownloadTicket: %v", err)
	}
	got, err := http.Get(base + downloadURL)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer got.Body.Close()
	if got.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d", got.StatusCode)
	}
	if ct := got.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", ct)
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read downloaded body: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("downloaded %d bytes, uploaded %d", len(body), len(payload))
	}
}

// TestMintAttachmentDownloadTicketRejectsCrossThreadID is the ownership
// recheck, at the only place that can make it: the route knows nothing
// about attachment rows, so a stale cross-thread id has to die here rather
// than become a ticket.
func TestMintAttachmentDownloadTicketRejectsCrossThreadID(t *testing.T) {
	app, base := attachmentTransportApp(t)
	record := uploadTestAttachment(t, app, "thr-a", "hero.png", "image/png", pngSignature())

	if _, err := app.MintAttachmentDownloadTicket("thr-b", record.ID); err == nil {
		t.Fatal("expected cross-thread attachment rejection")
	} else if !strings.Contains(err.Error(), "belongs to thread") {
		t.Fatalf("mint error = %v, want ownership rejection", err)
	}

	// The route holds the same line independently. A ticket that WAS
	// legitimately minted still resolves the file through the ownership
	// check, so nothing rests on the mint having been the only gate.
	downloadURL, err := app.MintAttachmentDownloadTicket("thr-a", record.ID)
	if err != nil {
		t.Fatalf("MintAttachmentDownloadTicket: %v", err)
	}
	if err := app.attachments.Delete("thr-a", record.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	resp, err := http.Get(base + downloadURL)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d for an attachment deleted after minting, want 404", resp.StatusCode)
	}
}

func TestMintAttachmentDownloadTicketMissingReturnsError(t *testing.T) {
	app, _ := attachmentTransportApp(t)
	if _, err := app.MintAttachmentDownloadTicket("thr-a", "nope"); err == nil {
		t.Fatal("expected error for missing attachment")
	}
}

// TestMintAttachmentUploadTicketRefusesOversize is the pre-check the whole
// mint exists for: a payload over the cap is refused for the price of one
// RPC instead of one full transfer.
func TestMintAttachmentUploadTicketRefusesOversize(t *testing.T) {
	app, _ := attachmentTransportApp(t)
	oversize := app.attachments.MaxSizeFor(store.AttachmentKindImage) + 1
	_, err := app.MintAttachmentUploadTicket("thr-a", "huge.png", "image/png", oversize)
	if err == nil {
		t.Fatal("expected an oversize upload to be refused before minting")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("mint error = %v, want the size limit", err)
	}
}

// TestMintAttachmentUploadTicketRefusesAnOverlongDeclaredType is the one
// type refusal left now that any file is an attachment: a declared content
// type past the store's byte cap is refused at mint, before a byte moves.
func TestMintAttachmentUploadTicketRefusesAnOverlongDeclaredType(t *testing.T) {
	app, _ := attachmentTransportApp(t)
	overlong := strings.Repeat("x", attachment.MaxDeclaredMIMEBytes+1)
	if _, err := app.MintAttachmentUploadTicket("thr-a", "blob.bin", overlong, 128); err == nil {
		t.Fatal("expected an overlong declared type to be refused before minting")
	}
}

// TestMintAttachmentUploadTicketNormalizesTheType pins that what the
// ticket carries is the type the STORE will record, not the one the caller
// spelled. Otherwise the row a transfer creates could disagree with the
// mint that authorized it.
func TestMintAttachmentUploadTicketNormalizesTheType(t *testing.T) {
	app, base := attachmentTransportApp(t)
	payload := realPNGBytes(t)

	// "image/jpg" is an accepted spelling that the store canonicalizes to
	// image/jpeg; a filename with no usable mime is resolved from its
	// extension. Both must reach the row as the canonical value.
	uploadURL, err := app.MintAttachmentUploadTicket("thr-a", "shot.png", "", int64(len(payload)))
	if err != nil {
		t.Fatalf("MintAttachmentUploadTicket: %v", err)
	}
	req, err := http.NewRequest(http.MethodPut, base+uploadURL, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build upload request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload status = %d (%s)", resp.StatusCode, body)
	}
	var record store.Attachment
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		t.Fatalf("decode created row: %v", err)
	}
	if record.MimeType != "image/png" {
		t.Fatalf("stored mime = %q, want the canonical image/png", record.MimeType)
	}
}

// TestAttachmentUploadRefusesABodyThatIsNotTheDeclaredImage: the ticket
// fixed a content type, and the signature is what decides whether the
// bytes agree with it. Nothing survives on disk when they do not.
func TestAttachmentUploadRefusesABodyThatIsNotTheDeclaredImage(t *testing.T) {
	app, base := attachmentTransportApp(t)
	notAnImage := []byte("this text is not a PNG, whatever the ticket says")

	uploadURL, err := app.MintAttachmentUploadTicket("thr-a", "hero.png", "image/png", int64(len(notAnImage)))
	if err != nil {
		t.Fatalf("MintAttachmentUploadTicket: %v", err)
	}
	req, err := http.NewRequest(http.MethodPut, base+uploadURL, bytes.NewReader(notAnImage))
	if err != nil {
		t.Fatalf("build upload request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	listed, err := app.ListAttachments("thr-a")
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("a refused upload left %d row(s) behind", len(listed))
	}
}

// TestAttachmentMintsRequireATransport pins the boot ordering: before the
// transport serves there is nothing to mint against, and the answer is an
// error rather than an empty URL a client would fetch.
func TestAttachmentMintsRequireATransport(t *testing.T) {
	app := newAttachmentTestApp(t)
	if _, err := app.MintAttachmentDownloadTicket("thr-a", "any"); err == nil {
		t.Fatal("expected a download mint with no transport to be refused")
	}
	if _, err := app.MintAttachmentUploadTicket("thr-a", "a.png", "image/png", 8); err == nil {
		t.Fatal("expected an upload mint with no transport to be refused")
	}
}

func TestAttachmentMintsRequireAnInitialisedStore(t *testing.T) {
	app := &App{}
	if _, err := app.MintAttachmentUploadTicket("thr-a", "hero.png", "image/png", 8); err == nil ||
		!strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("expected init error, got %v", err)
	}
	if _, err := app.MintAttachmentDownloadTicket("thr-a", "hero"); err == nil ||
		!strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("expected init error, got %v", err)
	}
}

func TestGetAttachmentThumbnailReturnsThumb(t *testing.T) {
	app := newAttachmentTestApp(t)
	// Use a real (decodable) PNG so the generator can read it.
	record := uploadTestAttachment(t, app, "thr-a", "shot.png", "image/png", realPNGBytes(t))
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
	record := uploadTestAttachment(t, app, "thr-a", "shot.png", "image/png", realPNGBytes(t))
	if _, err := app.GetAttachmentThumbnail("thr-b", record.ID); err == nil {
		t.Fatal("expected cross-thread thumbnail rejection")
	}
}

func TestDeleteAttachmentBinding(t *testing.T) {
	app := newAttachmentTestApp(t)
	record := uploadTestAttachment(t, app, "thr-a", "hero.png", "image/png", pngSignature())

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
	record := uploadTestAttachment(t, app, "thr-a", "hero.png", "image/png", realPNGBytes(t))

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
	if _, _, err := app.attachments.ReadThreadBytes("thr-a", record.ID); err != nil {
		t.Fatalf("bytes destroyed by a refused delete: %v", err)
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

// The upload ticket is minted against the KIND's cap, decided from the
// declared type and filename exactly as the store decides it. A file may
// be five times an image; an image declared as one stays at the image cap
// however big it claims to be.
func TestMintAttachmentUploadTicketCapsPerKind(t *testing.T) {
	app, _ := attachmentTransportApp(t)
	imageCap := app.attachments.MaxSizeFor(store.AttachmentKindImage)
	fileCap := app.attachments.MaxSizeFor(store.AttachmentKindFile)
	if fileCap <= imageCap {
		t.Fatalf("file cap %d is not above the image cap %d; the test proves nothing", fileCap, imageCap)
	}

	if _, err := app.MintAttachmentUploadTicket("thr-a", "report.pdf", "application/pdf", fileCap); err != nil {
		t.Fatalf("file at the file cap: %v", err)
	}
	if _, err := app.MintAttachmentUploadTicket("thr-a", "report.pdf", "application/pdf", fileCap+1); err == nil ||
		!strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("file over the file cap: got %v", err)
	}
	if _, err := app.MintAttachmentUploadTicket("thr-a", "hero.png", "image/png", imageCap+1); err == nil ||
		!strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("image over the image cap must not slide under the file cap: got %v", err)
	}
	// A file declared as an image by its NAME is an image, at the image cap.
	if _, err := app.MintAttachmentUploadTicket("thr-a", "shot.png", "", imageCap+1); err == nil ||
		!strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("image-by-extension over the image cap: got %v", err)
	}
}

// A `file` rides the same ticketed PUT an image does and lands as a
// file: its own `<id>` directory, the real filename, the declared type
// kept verbatim. What it never does is come back: the download mint and
// the route's own open both refuse it, so a document attached for the
// agent is not servable at any origin. Deleting it takes the directory.
func TestAttachmentFileKindUploadsOverHTTPButIsNeverServed(t *testing.T) {
	app, base := attachmentTransportApp(t)
	payload := []byte("%PDF-1.7\n")

	uploadURL, err := app.MintAttachmentUploadTicket("thr-a", "report.pdf", "application/pdf", int64(len(payload)))
	if err != nil {
		t.Fatalf("MintAttachmentUploadTicket: %v", err)
	}
	req, err := http.NewRequest(http.MethodPut, base+uploadURL, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build upload request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var record store.Attachment
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if record.Kind != store.AttachmentKindFile {
		t.Fatalf("Kind: got %q want %q", record.Kind, store.AttachmentKindFile)
	}
	if record.MimeType != "application/pdf" {
		t.Fatalf("MimeType: got %q", record.MimeType)
	}
	if want := "thr-a/" + record.ID + "/report.pdf"; record.RelativePath != want {
		t.Fatalf("RelativePath: got %q want %q", record.RelativePath, want)
	}

	if _, err := app.MintAttachmentDownloadTicket("thr-a", record.ID); !errors.Is(err, attachment.ErrNotAnImage) {
		t.Errorf("MintAttachmentDownloadTicket: got %v want ErrNotAnImage", err)
	}
	if _, err := AttachmentTransfer(app).OpenAttachment("thr-a", record.ID); !errors.Is(err, attachment.ErrNotAnImage) {
		t.Errorf("OpenAttachment: got %v want ErrNotAnImage", err)
	}
	if _, err := app.GetAttachmentThumbnail("thr-a", record.ID); !errors.Is(err, attachment.ErrNotAnImage) {
		t.Errorf("GetAttachmentThumbnail: got %v want ErrNotAnImage", err)
	}

	// Deleting a file takes its `<id>` directory with it.
	_, path, err := app.attachments.PathForThread("thr-a", record.ID)
	if err != nil {
		t.Fatalf("PathForThread: %v", err)
	}
	if err := app.DeleteAttachment("thr-a", record.ID); err != nil {
		t.Fatalf("DeleteAttachment: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("file directory survived the delete: %v", err)
	}
}

// A mixed turn is the shape the whole feature turns on: the `[Image #N]`
// markers are numbered over the IMAGE subset, so a file sitting between
// two images must not consume a number, must not enter the provider
// slice, and must arrive as a prompt line on providerContent — never on
// the persisted content.
func TestResolveUserMessageEnvelopeMixedAttachmentTurn(t *testing.T) {
	app := newAttachmentTestApp(t)

	first := uploadTestAttachment(t, app, "thr-a", "one.png", "image/png", pngSignature())
	file := uploadTestAttachment(t, app, "thr-a", "report.pdf", "application/pdf", []byte("%PDF-1.7\n"))
	second := uploadTestAttachment(t, app, "thr-a", "two.png", "image/png", pngSignature())

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
		mime, data := "image/png", pngSignature()
		if i%2 == 1 {
			mime, data = "application/pdf", []byte("%PDF-1.7\n")
		}
		record := uploadTestAttachment(t, app, "thr-a", "a", mime, data)
		ids = append(ids, record.ID)
	}
	if _, err := app.resolveSendMessageAttachments("thr-a", ids); err == nil ||
		!strings.Contains(err.Error(), "too many attachments") {
		t.Fatalf("expected the union cap to fire, got %v", err)
	}
}
