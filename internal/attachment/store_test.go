package attachment

import (
	"encoding/base64"
	"os"
	"path/filepath"
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

func pngData(t *testing.T) string {
	t.Helper()
	// 1x1 PNG (real header).
	payload := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89,
	}
	return base64.StdEncoding.EncodeToString(payload)
}

func jpegData(t *testing.T) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10})
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

func TestUploadAndReadRoundTrip(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	record, err := attStore.Upload("t1", "pic.png", "image/png", pngData(t), 1000)
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
}

func TestUploadInfersMimeFromFilename(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	// No mime type provided — should infer from extension.
	record, err := attStore.Upload("t1", "chart.jpg", "", jpegData(t), 0)
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

	_, err := attStore.Upload("t1", "pic.png", "image/png", jpegData(t), 0)
	if err == nil || !strings.Contains(err.Error(), "payload does not match image/png") {
		t.Fatalf("expected payload mismatch error, got %v", err)
	}
}

func TestUploadRejectsDisallowedMime(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	_, err := attStore.Upload("t1", "evil.exe", "application/x-msdownload", pngData(t), 0)
	if err == nil {
		t.Fatal("expected mime rejection")
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
	encoded := base64.StdEncoding.EncodeToString(big)
	_, err = attStore.Upload("t1", "pic.png", "image/png", encoded, 0)
	if err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
}

func TestUploadRejectsEmptyPayload(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	_, err := attStore.Upload("t1", "pic.png", "image/png", "", 0)
	if err == nil {
		t.Fatal("expected empty payload rejection")
	}
}

func TestUploadRejectsBadBase64(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	_, err := attStore.Upload("t1", "pic.png", "image/png", "!!!not-base64!!!", 0)
	if err == nil {
		t.Fatal("expected base64 decode error")
	}
}

func TestUploadRequiresThreadID(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	_, err := attStore.Upload("   ", "pic.png", "image/png", pngData(t), 0)
	if err == nil {
		t.Fatal("expected thread id required")
	}
}

func TestUploadRollsBackOnMetaFailure(t *testing.T) {
	attStore, _ := newTestStores(t)
	// No thread seeded — insert will fail due to FK.
	_, err := attStore.Upload("missing-thread", "pic.png", "image/png", pngData(t), 0)
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

	_, err := attStore.Upload("missing-thread", "pic.png", "image/png", pngData(t), 0)
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
func TestUploadFilenameFuzz(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	cases := []struct {
		name     string
		filename string
		mime     string
	}{
		{"traversal-in-filename", "../etc/passwd.png", "image/png"},
		{"null-byte", "pic\x00.png", "image/png"},
		{"newline", "pic\n.png", "image/png"},
		{"mixed-case-ext", "PIC.PNG", "image/png"},
		{"double-dot", "..png", ""},
		{"empty-ext", "pic", "image/png"},
		{"only-dot", ".", "image/png"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record, err := attStore.Upload("t1", tc.filename, tc.mime, pngData(t), 0)
			if err != nil {
				// Rejected — acceptable. No orphan files should exist.
				return
			}
			// Accepted — the final on-disk path MUST stay under root.
			absRoot, _ := filepath.Abs(attStore.root)
			absFile, _ := filepath.Abs(filepath.Join(attStore.root, record.RelativePath))
			if !strings.HasPrefix(absFile, absRoot+string(os.PathSeparator)) {
				t.Errorf("accepted filename %q produced path escaping root: %q", tc.filename, absFile)
			}
			// The file must actually exist (not a phantom row).
			if _, err := os.Stat(absFile); err != nil {
				t.Errorf("accepted record missing on disk: %v", err)
			}
		})
	}
}

// TestUploadSuccessfulEndStateHasNoTmpFile documents the tmp-rename
// contract from the success side: after a successful upload, only the
// final file exists (no .tmp sibling). Acts as a post-condition for the
// two-phase write path.
func TestUploadSuccessfulEndStateHasNoTmpFile(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")

	record, err := attStore.Upload("t1", "pic.png", "image/png", pngData(t), 0)
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

	_, err := attStore.Upload("t1", "pic.png", "image/png", pngData(t), 0)
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

	record, err := attStore.Upload("t1", "pic.png", "image/png", pngData(t), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if err := attStore.Delete(record.ID); err != nil {
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

	err := attStore.Delete("never-was-there")
	if err == nil {
		t.Fatal("expected error deleting missing attachment")
	}
}

func TestListReturnsPerThread(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	seedThread(t, meta, "t2")

	if _, err := attStore.Upload("t1", "a.png", "image/png", pngData(t), 1); err != nil {
		t.Fatalf("upload a: %v", err)
	}
	if _, err := attStore.Upload("t1", "b.png", "image/png", pngData(t), 2); err != nil {
		t.Fatalf("upload b: %v", err)
	}
	if _, err := attStore.Upload("t2", "c.png", "image/png", pngData(t), 3); err != nil {
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

	record, err := attStore.Upload(threadID, "evil.png", "image/png", pngData(t), 0)
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
