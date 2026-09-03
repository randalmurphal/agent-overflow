package attachment

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

func newTestStores(t *testing.T) (*Store, *store.Store) {
	t.Helper()
	meta, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { meta.Close() })

	tmpDir := t.TempDir()
	attStore, err := NewStore(Config{RootDir: tmpDir}, meta)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return attStore, meta
}

func seedThread(t *testing.T, meta *store.Store, id string) {
	t.Helper()
	project := testutil.EnsureProject(t, meta, "/tmp")
	now := time.Now().UnixMilli()
	thread := store.Thread{
		ID:            id,
		ProjectID:     project.ID,
		Title:         "Thread",
		Provider:      "claude",
		WorkspacePath: "/tmp",
		Model:         "claude-sonnet",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := meta.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
}

func pngData(t *testing.T) []byte {
	t.Helper()
	// 1x1 PNG (real header).
	return []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89,
	}
}

func jpegData(t *testing.T) []byte {
	t.Helper()
	return []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}
}

// uploadBytes is the tests' spelling of the streaming Upload for a payload
// they already hold in memory. Every test that is not ABOUT the streaming
// contract goes through it, so the length agreement — Upload's whole
// premise — is stated once rather than at forty call sites.
func uploadBytes(s *Store, threadID, filename, mimeType string, data []byte, createdAt int64) (store.Attachment, error) {
	return s.Upload(threadID, filename, mimeType, int64(len(data)), bytes.NewReader(data), createdAt)
}

func gifData(t *testing.T) []byte {
	t.Helper()
	return []byte("GIF89a\x01\x00\x01\x00")
}

// textData is a `file`-kind payload: bytes that are not any image the
// signature detector knows, so an upload accepting them proves the file
// path skipped the signature check entirely.
func textData(t *testing.T, s string) []byte {
	t.Helper()
	return []byte(s)
}

func TestNewStoreRejectsMissingRoot(t *testing.T) {
	meta, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { meta.Close() })
	_, err = NewStore(Config{}, meta)
	if err == nil || !strings.Contains(err.Error(), "root directory") {
		t.Fatalf("expected missing-root error, got %v", err)
	}
}

func TestNewStoreRejectsMissingMeta(t *testing.T) {
	_, err := NewStore(Config{RootDir: t.TempDir()}, nil)
	if err == nil {
		t.Fatal("expected error for nil meta store")
	}
}

func TestNewStoreRejectsNegativeMaxSize(t *testing.T) {
	meta, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { meta.Close() })
	_, err = NewStore(Config{RootDir: t.TempDir(), MaxSize: -1}, meta)
	if err == nil {
		t.Fatal("expected error for negative max size")
	}
}

func TestNewStoreRepairsPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not stable on Windows")
	}
	meta, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { meta.Close() })

	root := filepath.Join(t.TempDir(), "attachments")
	threadDir := filepath.Join(root, "thread-a")
	filePath := filepath.Join(threadDir, "image.png")
	if err := os.MkdirAll(threadDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("png"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := NewStore(Config{RootDir: root}, meta); err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	assertMode(t, root, 0o700)
	assertMode(t, threadDir, 0o700)
	assertMode(t, filePath, 0o600)
}

func TestUploadAndReadRoundTrip(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	record, err := uploadBytes(attStore, "t1", "pic.png", "image/png", pngData(t), 1000)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if record.ID == "" {
		t.Fatal("expected generated id")
	}
	if record.ThreadID != "t1" {
		t.Fatalf("ThreadID: got %q", record.ThreadID)
	}
	if record.Filename != "pic.png" {
		t.Fatalf("Filename: got %q", record.Filename)
	}
	if record.MimeType != "image/png" {
		t.Fatalf("MimeType: got %q", record.MimeType)
	}
	if !strings.HasSuffix(record.RelativePath, ".png") {
		t.Fatalf("RelativePath should end .png, got %q", record.RelativePath)
	}
	if !strings.HasPrefix(record.RelativePath, "t1/") {
		t.Fatalf("RelativePath should be per-thread, got %q", record.RelativePath)
	}
	if record.Size <= 0 {
		t.Fatalf("Size should be >0, got %d", record.Size)
	}

	readBack, bytes, err := attStore.ReadBytes(record.ID)
	if err != nil {
		t.Fatalf("ReadBytes: %v", err)
	}
	if readBack.ID != record.ID {
		t.Fatalf("ID mismatch: got %q want %q", readBack.ID, record.ID)
	}
	if len(bytes) != int(record.Size) {
		t.Fatalf("size mismatch: got %d want %d", len(bytes), record.Size)
	}
	if runtime.GOOS != "windows" {
		_, path, ok, err := attStore.Get(record.ID)
		if err != nil || !ok {
			t.Fatalf("Get: ok=%v err=%v", ok, err)
		}
		assertMode(t, filepath.Dir(path), 0o700)
		assertMode(t, path, 0o600)
	}
}

func TestUploadInfersMimeFromFilename(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	// No mime type provided — should infer from extension.
	record, err := uploadBytes(attStore, "t1", "chart.jpg", "", jpegData(t), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if record.MimeType != "image/jpeg" {
		t.Fatalf("MimeType inferred: got %q want image/jpeg", record.MimeType)
	}
}

func TestUploadRejectsMismatchedImagePayload(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	_, err := uploadBytes(attStore, "t1", "pic.png", "image/png", jpegData(t), 0)
	if err == nil || !strings.Contains(err.Error(), "payload does not match image/png") {
		t.Fatalf("expected payload mismatch error, got %v", err)
	}
}

// A non-image MIME is no longer a rejection: it is the `file` kind, kept
// at face value with no signature check. The bytes here ARE a PNG and the
// row still says application/x-msdownload — declaring an image is what
// makes an image, and this upload declared none.
func TestUploadNonImageMimeBecomesFile(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	record, err := uploadBytes(attStore, "t1", "evil.exe", "application/x-msdownload", pngData(t), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if record.Kind != store.AttachmentKindFile {
		t.Fatalf("Kind: got %q want %q", record.Kind, store.AttachmentKindFile)
	}
	if record.MimeType != "application/x-msdownload" {
		t.Fatalf("MimeType: got %q, declared type should survive verbatim", record.MimeType)
	}
	if want := "t1/" + record.ID + "/evil.exe"; record.RelativePath != want {
		t.Fatalf("RelativePath: got %q want %q", record.RelativePath, want)
	}
}

// The kind rule in one table. The asymmetry is the point: declaring (or
// naming) an image is a PROMISE the bytes must keep, and everything else
// is a file with no promise to keep.
func TestUploadKindRule(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	cases := []struct {
		name     string
		filename string
		mime     string
		data     []byte
		wantKind string
		wantMIME string
		wantErr  string
	}{
		{"declared-image-mime", "a.png", "image/png", pngData(t), store.AttachmentKindImage, "image/png", ""},
		{"image-extension-no-mime", "a.jpg", "", jpegData(t), store.AttachmentKindImage, "image/jpeg", ""},
		{"image-extension-wrong-mime", "a.gif", "text/plain", gifData(t), store.AttachmentKindImage, "image/gif", ""},
		{"declared-image-mime-lying", "a.png", "image/png", jpegData(t), "", "", "does not match image/png"},
		{"image-extension-lying", "a.png", "", jpegData(t), "", "", "does not match image/png"},
		// An image format no provider ingests is a FILE, and is never
		// asked to prove itself — these bytes are a PNG, not a heic.
		{"unignested-image-format", "shot.heic", "image/heic", pngData(t), store.AttachmentKindFile, "image/heic", ""},
		{"svg", "logo.svg", "image/svg+xml", pngData(t), store.AttachmentKindFile, "image/svg+xml", ""},
		{"pdf", "report.pdf", "application/pdf", textData(t, "%PDF-1.7"), store.AttachmentKindFile, "application/pdf", ""},
		{"no-mime-no-extension", "LICENSE", "", textData(t, "MIT"), store.AttachmentKindFile, fallbackFileMIME, ""},
		{"mime-is-normalised", "a.PNG", "  IMAGE/PNG  ", pngData(t), store.AttachmentKindImage, "image/png", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record, err := uploadBytes(attStore, "t1", tc.filename, tc.mime, tc.data, 0)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err: got %v want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Upload: %v", err)
			}
			if record.Kind != tc.wantKind {
				t.Errorf("Kind: got %q want %q", record.Kind, tc.wantKind)
			}
			if record.MimeType != tc.wantMIME {
				t.Errorf("MimeType: got %q want %q", record.MimeType, tc.wantMIME)
			}
		})
	}
}

// A declared MIME is the only unbounded caller-controlled string a `file`
// row stores, so it is the only one that needs a bound.
func TestUploadRejectsOversizedDeclaredMIME(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	_, err := uploadBytes(attStore, "t1", "blob.bin", strings.Repeat("x", MaxDeclaredMIMEBytes+1), textData(t, "hi"), 0)
	if err == nil || !strings.Contains(err.Error(), "declared mime type") {
		t.Fatalf("expected declared-mime bound error, got %v", err)
	}
}

// The kind decides the cap, and the kind is decided first — so a PNG over
// the image cap is refused at 10 MiB instead of sliding under the 50 MiB
// file one.
func TestUploadEnforcesCapPerKind(t *testing.T) {
	meta, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { meta.Close() })
	attStore, err := NewStore(Config{RootDir: t.TempDir(), MaxSize: 64, MaxFileSize: 4096}, meta)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	seedThread(t, meta, "t1")

	oversizeImage := append(pngData(t), make([]byte, 128)...)
	if _, err := uploadBytes(attStore, "t1", "big.png", "image/png", oversizeImage, 0); err == nil ||
		!strings.Contains(err.Error(), "exceeds limit 64") {
		t.Fatalf("image over the image cap: got %v, want limit 64", err)
	}
	// The same byte count as a file: under the file cap, accepted.
	if _, err := uploadBytes(attStore, "t1", "big.bin", "application/octet-stream", oversizeImage, 0); err != nil {
		t.Fatalf("file under the file cap: %v", err)
	}
	big := make([]byte, 8192)
	if _, err := uploadBytes(attStore, "t1", "huge.bin", "application/octet-stream", big, 0); err == nil ||
		!strings.Contains(err.Error(), "exceeds limit 4096") {
		t.Fatalf("file over the file cap: got %v, want limit 4096", err)
	}
}

// The kind, not the MIME, is what makes bytes servable. A `file` never
// leaves this package as bytes or as a thumbnail.
func TestFileKindRefusesByteAccessors(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	record, err := uploadBytes(attStore, "t1", "report.pdf", "application/pdf", textData(t, "%PDF-1.7"), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, _, err := attStore.ReadThreadBytes("t1", record.ID); !errors.Is(err, ErrNotAnImage) {
		t.Errorf("ReadThreadBytes: got %v want ErrNotAnImage", err)
	}
	if _, _, err := attStore.Thumbnail("t1", record.ID); !errors.Is(err, ErrNotAnImage) {
		t.Errorf("Thumbnail: got %v want ErrNotAnImage", err)
	}
	// The path accessor is the file's delivery route and stays open.
	if _, path, err := attStore.PathForThread("t1", record.ID); err != nil || filepath.Base(path) != "report.pdf" {
		t.Errorf("PathForThread: path=%q err=%v", path, err)
	}
}

// A file owns its `<id>` directory, so that is what Delete takes — an
// empty directory per deleted attachment would otherwise accumulate under
// every thread forever.
func TestDeleteRemovesFileDirectory(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	record, err := uploadBytes(attStore, "t1", "report.pdf", "application/pdf", textData(t, "%PDF-1.7"), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	dir := filepath.Join(attStore.root, "t1", record.ID)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("expected file dir: %v", err)
	}
	if err := attStore.Delete("t1", record.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("file dir survived Delete: %v", err)
	}
}

// Deleting is at least as privileged as reading: a foreign thread id is
// refused before anything is removed, so a stale id from a closed composer
// or one sent by any client leaves both the row and the bytes
// intact. Covers both kinds because a file's delete takes a whole
// directory, which is the more destructive of the two.
func TestDeleteRefusesForeignThread(t *testing.T) {
	for _, tc := range []struct {
		name     string
		filename string
		mime     string
		data     []byte
	}{
		{"image", "pic.png", "image/png", pngData(t)},
		{"file", "report.pdf", "application/pdf", textData(t, "%PDF-1.7")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attStore, meta := newTestStores(t)
			seedThread(t, meta, "owner")
			seedThread(t, meta, "intruder")

			record, err := uploadBytes(attStore, "owner", tc.filename, tc.mime, tc.data, 0)
			if err != nil {
				t.Fatalf("Upload: %v", err)
			}
			diskPath := filepath.Join(attStore.root, record.RelativePath)

			err = attStore.Delete("intruder", record.ID)
			if err == nil {
				t.Fatal("expected a foreign-thread delete to be refused")
			}
			if !strings.Contains(err.Error(), "belongs to thread") {
				t.Fatalf("expected an ownership error, got %v", err)
			}

			if _, _, ok, err := attStore.Get(record.ID); err != nil || !ok {
				t.Fatalf("row destroyed by a refused delete: ok=%v err=%v", ok, err)
			}
			if _, err := os.Stat(diskPath); err != nil {
				t.Fatalf("bytes destroyed by a refused delete: %v", err)
			}

			// The owner can still delete it, so the refusal is the check
			// and not a wedge.
			if err := attStore.Delete("owner", record.ID); err != nil {
				t.Fatalf("owner Delete: %v", err)
			}
		})
	}
}

// CopyToThread is the clone primitive the cross-thread draft path uses. It
// copies bytes on disk under the same tmp-then-rename invariant, preserves
// the kind (rather than re-deriving one), and keeps the ownership boundary
// so a stale cross-thread id cannot pull another thread's file.
func TestCopyToThreadClonesBothKinds(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	seedThread(t, meta, "t2")

	image, err := uploadBytes(attStore, "t1", "pic.png", "image/png", pngData(t), 0)
	if err != nil {
		t.Fatalf("Upload image: %v", err)
	}
	file, err := uploadBytes(attStore, "t1", "report.pdf", "application/pdf", textData(t, "%PDF-1.7"), 0)
	if err != nil {
		t.Fatalf("Upload file: %v", err)
	}

	for _, source := range []store.Attachment{image, file} {
		clone, err := attStore.CopyToThread("t1", "t2", source.ID, 7)
		if err != nil {
			t.Fatalf("CopyToThread(%s): %v", source.Kind, err)
		}
		if clone.ID == source.ID {
			t.Fatalf("clone reused the source id")
		}
		if clone.ThreadID != "t2" || clone.Kind != source.Kind ||
			clone.Filename != source.Filename || clone.MimeType != source.MimeType || clone.Size != source.Size {
			t.Fatalf("clone did not preserve the row: %+v vs %+v", clone, source)
		}
		if clone.CreatedAt != 7 {
			t.Fatalf("CreatedAt: got %d want 7", clone.CreatedAt)
		}
		_, clonePath, ok, err := attStore.Get(clone.ID)
		if err != nil || !ok {
			t.Fatalf("Get clone: ok=%v err=%v", ok, err)
		}
		_, sourcePath, _, _ := attStore.Get(source.ID)
		got, err := os.ReadFile(clonePath)
		if err != nil {
			t.Fatalf("read clone: %v", err)
		}
		want, err := os.ReadFile(sourcePath)
		if err != nil {
			t.Fatalf("read source: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("clone bytes differ from source")
		}
		if filepath.Base(clonePath) != filepath.Base(sourcePath) && source.Kind == store.AttachmentKindFile {
			t.Fatalf("file clone lost its name: %q", clonePath)
		}
	}

	// Ownership: the source thread is a parameter precisely so this fails.
	if _, err := attStore.CopyToThread("t2", "t2", image.ID, 0); err == nil {
		t.Fatal("expected cross-thread clone of a t1 attachment to be refused")
	}
}

func TestPromptLineFormat(t *testing.T) {
	line := PromptLine(store.Attachment{
		Filename: "report.pdf",
		MimeType: "application/pdf",
		Size:     1258291,
	}, "/home/u/.config/agent-overflow/attachments/th/id/report.pdf")
	want := `[Attached file "report.pdf" (application/pdf, 1.2 MB) is saved at: /home/u/.config/agent-overflow/attachments/th/id/report.pdf]`
	if line != want {
		t.Fatalf("PromptLine:\n got %s\nwant %s", line, want)
	}
}

// Mirrors formatAttachmentSize in frontend/src/lib/types/attachment.ts.
func TestFormatSize(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024*1024 - 1, "1024.0 KB"},
		{1024 * 1024, "1.0 MB"},
		{52428800, "50.0 MB"},
	}
	for _, tc := range cases {
		if got := FormatSize(tc.bytes); got != tc.want {
			t.Errorf("FormatSize(%d): got %q want %q", tc.bytes, got, tc.want)
		}
	}
}

func TestUploadRejectsOversize(t *testing.T) {
	meta, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { meta.Close() })
	attStore, err := NewStore(Config{RootDir: t.TempDir(), MaxSize: 64}, meta)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	seedThread(t, meta, "t1")

	big := make([]byte, 128)
	_, err = uploadBytes(attStore, "t1", "pic.png", "image/png", big, 0)
	if err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

// TestUploadCapsABodyThatOverruns pins the bound INSIDE Upload rather than
// at its caller: a body that keeps going past the store's own limit is cut
// off and refused, so a caller that forgot its own cap still cannot make
// this store write past MaxSize.
func TestUploadCapsABodyThatOverruns(t *testing.T) {
	meta, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { meta.Close() })
	root := t.TempDir()
	attStore, err := NewStore(Config{RootDir: root, MaxSize: 64}, meta)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	seedThread(t, meta, "t1")

	// Declares a size the store accepts, then hands over ten times that
	// many bytes.
	payload := append(pngData(t), make([]byte, 640)...)
	_, err = attStore.Upload("t1", "pic.png", "image/png", 32, bytes.NewReader(payload), 0)
	if err == nil || !strings.Contains(err.Error(), "declared") {
		t.Fatalf("expected a length disagreement, got %v", err)
	}
	assertNoStoredBytes(t, attStore, meta, "t1")
}

func TestUploadRejectsEmptyPayload(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	_, err := uploadBytes(attStore, "t1", "pic.png", "image/png", nil, 0)
	if err == nil {
		t.Fatal("expected empty payload rejection")
	}
}

// TestUploadRejectsShortBody pins the length agreement in the direction a
// dropped connection produces: the declared size is what the metadata row
// records, so a body that stops early must fail rather than land a row
// whose Size is a lie about the file beside it.
func TestUploadRejectsShortBody(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	payload := pngData(t)
	_, err := attStore.Upload("t1", "pic.png", "image/png", int64(len(payload))+8, bytes.NewReader(payload), 0)
	if err == nil || !strings.Contains(err.Error(), "declared") {
		t.Fatalf("expected a length disagreement, got %v", err)
	}
	assertNoStoredBytes(t, attStore, meta, "t1")
}

// TestUploadRejectsMidStreamFailure covers the failure a network body has
// and an in-memory one does not: the reader errors part-way, after the
// signature already validated and bytes are already on disk.
func TestUploadRejectsMidStreamFailure(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	// Longer than the copy buffer, so the signature peek is satisfied from
	// bytes already read and the failure lands during the copy — after the
	// staging file exists and has content in it, which is the state the
	// deferred cleanup is for.
	prefix := append(pngData(t), make([]byte, 2*copyBufferSize)...)
	body := io.MultiReader(bytes.NewReader(prefix), errReader{})
	_, err := attStore.Upload("t1", "pic.png", "image/png", int64(len(prefix))+1024, body, 0)
	if err == nil || !strings.Contains(err.Error(), "write payload") {
		t.Fatalf("expected the read failure to surface, got %v", err)
	}
	assertNoStoredBytes(t, attStore, meta, "t1")
}

// errReader fails every read, standing in for a connection that dropped
// mid-transfer.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("body went away") }

// assertNoStoredBytes checks that a refused upload left neither a metadata
// row nor a file — including the .tmp staging file, which is the one a
// streaming write can leave behind and a single os.WriteFile could not.
func assertNoStoredBytes(t *testing.T, attStore *Store, meta *store.Store, threadID string) {
	t.Helper()
	list, err := meta.ListAttachments(threadID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected no metadata rows after a refused upload, got %d", len(list))
	}
	entries, err := os.ReadDir(filepath.Join(attStore.root, threadID))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no files after a refused upload, got %+v", entries)
	}
}

// TestOpenThreadStreamsStoredBytes is the read half: the handle hands back
// exactly what Upload stored, and refuses a cross-thread id on the same
// terms ReadThreadBytes does.
func TestOpenThreadStreamsStoredBytes(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	seedThread(t, meta, "t2")

	payload := pngData(t)
	record, err := uploadBytes(attStore, "t1", "pic.png", "image/png", payload, 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	content, err := attStore.OpenThread("t1", record.ID)
	if err != nil {
		t.Fatalf("OpenThread: %v", err)
	}
	defer content.File.Close()
	if content.Record.ID != record.ID {
		t.Fatalf("Record.ID = %q, want %q", content.Record.ID, record.ID)
	}
	if content.ModTime.IsZero() {
		t.Fatal("ModTime is zero; a conditional request would have nothing to compare")
	}
	got, err := io.ReadAll(content.File)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("streamed %d bytes, stored %d", len(got), len(payload))
	}

	if _, err := attStore.OpenThread("t2", record.ID); err == nil {
		t.Fatal("expected a cross-thread id to be refused")
	}
}

func TestUploadRequiresThreadID(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	_, err := uploadBytes(attStore, "   ", "pic.png", "image/png", pngData(t), 0)
	if err == nil {
		t.Fatal("expected thread id required")
	}
}

func TestUploadRollsBackOnMetaFailure(t *testing.T) {
	attStore, _ := newTestStores(t)
	// No thread seeded — insert will fail due to FK.
	_, err := uploadBytes(attStore, "missing-thread", "pic.png", "image/png", pngData(t), 0)
	if err == nil {
		t.Fatal("expected FK failure")
	}

	// After A9: no final file, no tmp file — the tmp was removed during
	// rollback so no .tmp orphan leaks either.
	threadDir := filepath.Join(attStore.root, "missing-thread")
	entries, err := os.ReadDir(threadDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no orphan files on rollback, got %+v", entries)
	}
}

// TestUploadNoTmpLeakOnMetaFailure confirms the tmp-rename contract: if
// the DB insert fails, the .tmp staging file is removed. Before A9 no
// tmp file existed at all; the new code relies on removing it cleanly.
func TestUploadNoTmpLeakOnMetaFailure(t *testing.T) {
	attStore, _ := newTestStores(t)

	_, err := uploadBytes(attStore, "missing-thread", "pic.png", "image/png", pngData(t), 0)
	if err == nil {
		t.Fatal("expected FK failure")
	}

	// Walk the attachment root looking for any .tmp file.
	err = filepath.Walk(attStore.root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".tmp") {
			t.Errorf("tmp file leaked: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestUploadFilenameFuzz exercises the filename-validation surface with
// adversarial inputs: path traversal, embedded null bytes, newlines, and
// mixed-case extensions. Every case must either be rejected or produce
// a record whose final on-disk path stays under the root.
// TestUploadFilenameFuzz walks awkward filenames through BOTH kinds. The
// file kind is the one that matters most: an image's filename is discarded
// in favour of `<id><ext>`, while a file's is sanitized and written to disk
// verbatim, so it is the only path where a caller's string becomes a path
// segment.
func TestUploadFilenameFuzz(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	filenames := []string{
		"../etc/passwd.png",
		"../../../../../../etc/passwd",
		"..",
		"...",
		"pic\x00.png",
		"pic\n.png",
		"PIC.PNG",
		"..png",
		"pic",
		".",
		".ssh/authorized_keys",
		`C:\Windows\System32\drivers\etc\hosts`,
		"stream.txt:$DATA",
		"trailing. ",
		"  spaced  .pdf",
		strings.Repeat("é", 400) + ".pdf",
		"\xff\xfe broken utf8.bin",
	}
	kinds := []struct {
		name string
		mime string
		data []byte
	}{
		{"image", "image/png", pngData(t)},
		{"file", "application/octet-stream", textData(t, "payload")},
	}
	absRoot, err := filepath.Abs(attStore.root)
	if err != nil {
		t.Fatalf("abs root: %v", err)
	}
	for _, kind := range kinds {
		for _, filename := range filenames {
			t.Run(kind.name+"/"+strconv.Quote(filename), func(t *testing.T) {
				record, err := uploadBytes(attStore, "t1", filename, kind.mime, kind.data, 0)
				if err != nil {
					// Rejected — acceptable. No orphan files should exist.
					return
				}
				// Accepted — the final on-disk path MUST stay under root.
				absFile, _ := filepath.Abs(filepath.Join(attStore.root, record.RelativePath))
				if !strings.HasPrefix(absFile, absRoot+string(os.PathSeparator)) {
					t.Fatalf("accepted filename %q produced path escaping root: %q", filename, absFile)
				}
				// The stored path must be exactly the layout for its kind:
				// containment alone would still pass for a name that walked
				// into a SIBLING thread's directory.
				want := "t1/" + record.ID + filepath.Ext(record.RelativePath)
				if record.Kind == store.AttachmentKindFile {
					want = "t1/" + record.ID + "/" + filepath.Base(record.RelativePath)
				}
				if record.RelativePath != want {
					t.Fatalf("RelativePath %q is not the %s layout (%q)", record.RelativePath, record.Kind, want)
				}
				if strings.Contains(record.RelativePath, "..") {
					t.Fatalf("RelativePath kept a '..' segment: %q", record.RelativePath)
				}
				if base := filepath.Base(record.RelativePath); len(base) > maxFilenameBytes {
					t.Fatalf("on-disk name is %d bytes, over the %d cap: %q", len(base), maxFilenameBytes, base)
				}
				// The file must actually exist (not a phantom row).
				if _, err := os.Stat(absFile); err != nil {
					t.Fatalf("accepted record missing on disk: %v", err)
				}
			})
		}
	}
}

// TestUploadSuccessfulEndStateHasNoTmpFile documents the tmp-rename
// contract from the success side: after a successful upload, only the
// final file exists (no .tmp sibling). Acts as a post-condition for the
// two-phase write path.
func TestUploadSuccessfulEndStateHasNoTmpFile(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	record, err := uploadBytes(attStore, "t1", "pic.png", "image/png", pngData(t), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// The final file must exist.
	finalPath := filepath.Join(attStore.root, record.RelativePath)
	if _, err := os.Stat(finalPath); err != nil {
		t.Errorf("final file missing after successful upload: %v", err)
	}
	// No .tmp sibling.
	if _, err := os.Stat(finalPath + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp file leaked after successful upload: %v", err)
	}
}

// TestPathForThreadReturnsOwnedPath proves the path-only accessor returns the
// resolved absolute path for an attachment owned by the thread, and refuses both
// a cross-thread id (the ownership boundary it shares with ReadThreadBytes) and
// a missing id — so a stale id can't reference another thread's file by path.
func TestPathForThreadReturnsOwnedPath(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	seedThread(t, meta, "t2")

	record, err := uploadBytes(attStore, "t1", "pic.png", "image/png", pngData(t), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Owner thread: returns the record and the resolved absolute path, which
	// must point at the real on-disk file.
	gotRecord, path, err := attStore.PathForThread("t1", record.ID)
	if err != nil {
		t.Fatalf("PathForThread(owner): %v", err)
	}
	if gotRecord.ID != record.ID {
		t.Errorf("record id = %q, want %q", gotRecord.ID, record.ID)
	}
	if want := filepath.Join(attStore.root, record.RelativePath); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("path %q is not absolute", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("returned path does not exist on disk: %v", err)
	}

	// Wrong thread: the ownership check refuses it and no path leaks.
	if _, p, err := attStore.PathForThread("t2", record.ID); err == nil {
		t.Errorf("PathForThread for a cross-thread id should error, got path %q", p)
	}

	// Missing id: not found, not a panic.
	if _, _, err := attStore.PathForThread("t1", "does-not-exist"); err == nil {
		t.Error("PathForThread for a missing id should error")
	}
}

// TestUploadFileWriteFailureLeavesNoRow injects a file-write failure by
// making the attachment root directory read-only and verifying no DB
// row is inserted if the tmp write fails.
func TestUploadFileWriteFailureLeavesNoRow(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	// Create the per-thread dir and lock it so the tmp WriteFile fails.
	threadDir := filepath.Join(attStore.root, "t1")
	if err := os.MkdirAll(threadDir, 0o555); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(threadDir, 0o755)
	})

	_, err := uploadBytes(attStore, "t1", "pic.png", "image/png", pngData(t), 0)
	if err == nil {
		t.Fatal("expected upload to fail when dir is read-only")
	}

	list, err := meta.ListAttachments("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("no row should be written when file write fails, got %d", len(list))
	}
}

func TestDeleteRemovesDiskAndRow(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	record, err := uploadBytes(attStore, "t1", "pic.png", "image/png", pngData(t), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if err := attStore.Delete("t1", record.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, _, ok, err := attStore.Get(record.ID)
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if ok {
		t.Fatal("expected not-found after delete")
	}
	diskPath := filepath.Join(attStore.root, record.RelativePath)
	if _, err := os.Stat(diskPath); !os.IsNotExist(err) {
		t.Fatalf("expected disk file to be gone, stat=%v", err)
	}
}

func TestDeleteMissingSurfacesError(t *testing.T) {
	attStore, _ := newTestStores(t)

	err := attStore.Delete("t1", "never-was-there")
	if err == nil {
		t.Fatal("expected error deleting missing attachment")
	}
}

func TestListReturnsPerThread(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	seedThread(t, meta, "t2")

	if _, err := uploadBytes(attStore, "t1", "a.png", "image/png", pngData(t), 1); err != nil {
		t.Fatalf("upload a: %v", err)
	}
	if _, err := uploadBytes(attStore, "t1", "b.png", "image/png", pngData(t), 2); err != nil {
		t.Fatalf("upload b: %v", err)
	}
	if _, err := uploadBytes(attStore, "t2", "c.png", "image/png", pngData(t), 3); err != nil {
		t.Fatalf("upload c: %v", err)
	}

	list, err := attStore.List("t1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 attachments, got %d", len(list))
	}
}

func TestResolveAbsoluteRejectsTraversal(t *testing.T) {
	attStore, _ := newTestStores(t)
	if _, err := attStore.resolveAbsolute("../etc/passwd"); err == nil {
		t.Fatal("expected rejection of traversal path")
	}
	if _, err := attStore.resolveAbsolute("/etc/passwd"); err == nil {
		t.Fatal("expected rejection of absolute path")
	}
	if _, err := attStore.resolveAbsolute(""); err == nil {
		t.Fatal("expected rejection of empty path")
	}
}

func TestUploadSanitisesThreadIDToPreventEscape(t *testing.T) {
	attStore, meta := newTestStores(t)

	// Seed a thread whose literal id contains path separators — it can
	// exist in the DB, but on disk the id must be sanitised.
	threadID := "../../evil"
	seedThread(t, meta, threadID)

	record, err := uploadBytes(attStore, threadID, "evil.png", "image/png", pngData(t), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if strings.Contains(record.RelativePath, "..") {
		t.Fatalf("relative path must not contain '..': %q", record.RelativePath)
	}
	// The file must live inside the configured root.
	absPath := filepath.Join(attStore.root, record.RelativePath)
	if !strings.HasPrefix(absPath, attStore.root) {
		t.Fatalf("path %q escaped root %q", absPath, attStore.root)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode %s = %o, want %o", path, got, want)
	}
}
