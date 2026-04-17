package attachment

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
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
	now := time.Now().UnixMilli()
	thread := store.Thread{
		ID:            id,
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
	record, err := attStore.Upload("t1", "chart.jpg", "", pngData(t), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if record.MimeType != "image/jpeg" {
		t.Fatalf("MimeType inferred: got %q want image/jpeg", record.MimeType)
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

	// The disk directory should either be missing or empty.
	threadDir := filepath.Join(attStore.root, "missing-thread")
	entries, err := os.ReadDir(threadDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no orphan files on rollback, got %+v", entries)
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
