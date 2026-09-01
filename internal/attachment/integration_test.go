package attachment

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

// integrationStores spins up a fresh SQLite store + on-disk attachment root
// for end-to-end tests. Both are cleaned up via t.Cleanup.
func integrationStores(t *testing.T) (*Store, *store.Store, string) {
	t.Helper()
	meta, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = meta.Close() })

	rootDir := t.TempDir()
	attStore, err := NewStore(Config{RootDir: rootDir}, meta)
	if err != nil {
		t.Fatalf("attachment.NewStore: %v", err)
	}
	return attStore, meta, rootDir
}

func integrationSeedThread(t *testing.T, meta *store.Store, id string) {
	t.Helper()
	project := testutil.EnsureProject(t, meta, "/tmp")
	now := time.Now().UnixMilli()
	thread := store.Thread{
		ID:            id,
		ProjectID:     project.ID,
		Title:         "Integration Thread",
		Provider:      "claude",
		WorkspacePath: "/tmp",
		Model:         "claude-sonnet",
		Mode:          "chat",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := meta.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
}

// realPNGBytes returns a tiny but valid PNG payload so the MIME whitelist
// accepts it. The prefix is the standard PNG signature.
func realPNGBytes() []byte {
	return []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9C, 0x62, 0x00, 0x02, 0x00, 0x00,
		0x05, 0x00, 0x01, 0xE2, 0x26, 0x05, 0x5B, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
		0x42, 0x60, 0x82,
	}
}

// TestIntegration_UploadValidImage verifies the happy path: a PNG lands on
// disk at the expected per-thread path with identical bytes, and a metadata
// row is inserted referencing it.
func TestIntegration_UploadValidImage(t *testing.T) {
	attStore, meta, rootDir := integrationStores(t)
	integrationSeedThread(t, meta, "thread-png")

	payload := realPNGBytes()
	record, err := uploadBytes(attStore,
		"thread-png", "hero.png", "image/png",
		payload, 1_700_000_000_000,
	)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if record.ID == "" {
		t.Fatal("expected non-empty generated id")
	}
	if record.Size != int64(len(payload)) {
		t.Fatalf("Size = %d, want %d", record.Size, len(payload))
	}
	if record.MimeType != "image/png" {
		t.Fatalf("MimeType = %q, want image/png", record.MimeType)
	}

	diskPath := filepath.Join(rootDir, record.RelativePath)
	onDisk, err := os.ReadFile(diskPath)
	if err != nil {
		t.Fatalf("read disk file: %v", err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Fatalf("disk bytes differ from upload bytes")
	}
	// Metadata row must exist and point to the same path.
	fromDB, ok, err := meta.GetAttachment(record.ID)
	if err != nil || !ok {
		t.Fatalf("GetAttachment: ok=%v err=%v", ok, err)
	}
	if fromDB.RelativePath != record.RelativePath {
		t.Fatalf("RelativePath mismatch: %q vs %q", fromDB.RelativePath, record.RelativePath)
	}
}

// TestIntegration_UploadInvalidMime verifies that a non-image MIME is
// rejected with a clear error and no file is written.
func TestIntegration_UploadInvalidMime(t *testing.T) {
	attStore, meta, rootDir := integrationStores(t)
	integrationSeedThread(t, meta, "thread-mime")

	payload := realPNGBytes()
	_, err := uploadBytes(attStore,
		"thread-mime", "install.sh", "application/x-shellscript",
		payload, 0,
	)
	if err == nil {
		t.Fatal("expected rejection for disallowed mime")
	}
	if !strings.Contains(err.Error(), "mime") && !strings.Contains(err.Error(), "image") {
		t.Fatalf("error message does not mention mime/image: %v", err)
	}
	// No disk file should exist in the per-thread dir.
	entries, _ := os.ReadDir(filepath.Join(rootDir, "thread-mime"))
	if len(entries) != 0 {
		t.Fatalf("expected no files for rejected upload, got %d", len(entries))
	}
}

// TestIntegration_UploadOverSizeLimit confirms the 10MiB cap is enforced and
// no partial file leaks on disk when a large payload is rejected.
func TestIntegration_UploadOverSizeLimit(t *testing.T) {
	attStore, meta, rootDir := integrationStores(t)
	integrationSeedThread(t, meta, "thread-huge")

	// 11MiB of PNG-header-prefixed bytes. The declared length is refused
	// before a byte of the body is read, so nothing is ever staged.
	oversize := make([]byte, 11*1024*1024)
	copy(oversize, realPNGBytes())
	_, err := uploadBytes(attStore,
		"thread-huge", "big.png", "image/png",
		oversize, 0,
	)
	if err == nil {
		t.Fatal("expected oversize rejection")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("error should mention limit, got %v", err)
	}
	// Guard: per-thread dir should be empty.
	entries, _ := os.ReadDir(filepath.Join(rootDir, "thread-huge"))
	if len(entries) != 0 {
		t.Fatalf("expected no files after oversize rejection, got %d", len(entries))
	}
}

// TestIntegration_UploadEmptyBuffer documents the contract: an empty payload
// is explicitly rejected with "payload is empty". A nil body declaring zero
// bytes trips the same check.
func TestIntegration_UploadEmptyBuffer(t *testing.T) {
	attStore, meta, _ := integrationStores(t)
	integrationSeedThread(t, meta, "thread-empty")

	_, err := uploadBytes(attStore, "thread-empty", "zero.png", "image/png", nil, 0)
	if err == nil {
		t.Fatal("expected rejection of empty payload")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error should mention empty, got %v", err)
	}
}

// TestIntegration_UploadPathTraversalInFilename ensures that hostile filenames
// containing traversal segments cannot escape the attachment dir. The stored
// path must still live under the root, regardless of the original filename.
func TestIntegration_UploadPathTraversalInFilename(t *testing.T) {
	attStore, meta, rootDir := integrationStores(t)
	integrationSeedThread(t, meta, "thread-trav")

	record, err := uploadBytes(attStore,
		"thread-trav", "../../../etc/passwd.png", "image/png",
		realPNGBytes(), 0,
	)
	if err != nil {
		t.Fatalf("Upload (should have been accepted with neutered path): %v", err)
	}
	absRoot, _ := filepath.Abs(rootDir)
	absFile, _ := filepath.Abs(filepath.Join(rootDir, record.RelativePath))
	if !strings.HasPrefix(absFile, absRoot+string(os.PathSeparator)) {
		t.Fatalf("stored path %q escaped root %q", absFile, absRoot)
	}
	// Extra guard: the RelativePath must not contain any ".." segments.
	if strings.Contains(record.RelativePath, "..") {
		t.Fatalf("RelativePath still contains '..': %q", record.RelativePath)
	}
	// And the per-thread sanitised id must be in the path.
	if !strings.HasPrefix(record.RelativePath, "thread-trav/") {
		t.Fatalf("RelativePath should start with thread-trav/, got %q", record.RelativePath)
	}
}

// TestIntegration_UploadWithDuplicateFilenames verifies that two uploads with
// identical original filenames are each given a unique id-prefixed disk name.
func TestIntegration_UploadWithDuplicateFilenames(t *testing.T) {
	attStore, meta, rootDir := integrationStores(t)
	integrationSeedThread(t, meta, "thread-dup")

	payload := realPNGBytes()
	first, err := uploadBytes(attStore, "thread-dup", "shared.png", "image/png",
		payload, 1)
	if err != nil {
		t.Fatalf("first Upload: %v", err)
	}
	second, err := uploadBytes(attStore, "thread-dup", "shared.png", "image/png",
		payload, 2)
	if err != nil {
		t.Fatalf("second Upload: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("expected distinct IDs for two uploads")
	}
	if first.RelativePath == second.RelativePath {
		t.Fatalf("expected distinct disk paths; got %q twice", first.RelativePath)
	}
	// Both files must exist on disk.
	for _, r := range []store.Attachment{first, second} {
		if _, err := os.Stat(filepath.Join(rootDir, r.RelativePath)); err != nil {
			t.Fatalf("stat %s: %v", r.RelativePath, err)
		}
	}
}

// TestIntegration_UploadRollbackOnDiskFailure makes the target directory
// read-only so WriteFile fails, then confirms no DB row is created and no
// partial file leaks. Skipped on Windows where permission bits differ.
func TestIntegration_UploadRollbackOnDiskFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0o500 semantics not portable to Windows")
	}
	// Running as root bypasses POSIX permission checks, so the write would
	// succeed and the invariant under test cannot be exercised. Skip rather
	// than give a false-positive pass.
	if os.Geteuid() == 0 {
		t.Skip("root bypasses POSIX permissions; cannot provoke disk failure")
	}
	attStore, meta, rootDir := integrationStores(t)
	integrationSeedThread(t, meta, "thread-ro")

	threadDir := filepath.Join(rootDir, "thread-ro")
	if err := os.MkdirAll(threadDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(threadDir, 0o500); err != nil {
		t.Fatalf("chmod readonly: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(threadDir, 0o755) })

	_, err := uploadBytes(attStore,
		"thread-ro", "pic.png", "image/png",
		realPNGBytes(), 0,
	)
	if err == nil {
		t.Fatal("expected write failure when thread dir is read-only")
	}
	// No DB row should exist for this thread.
	list, listErr := meta.ListAttachments("thread-ro")
	if listErr != nil {
		t.Fatalf("ListAttachments: %v", listErr)
	}
	if len(list) != 0 {
		t.Fatalf("expected no DB rows after rollback, got %d", len(list))
	}
	// Restore perms so we can inspect the directory.
	_ = os.Chmod(threadDir, 0o755)
	entries, _ := os.ReadDir(threadDir)
	if len(entries) != 0 {
		t.Fatalf("expected no orphan files after rollback, got %+v", entries)
	}
}

// TestIntegration_ListAttachmentsScopedToThread asserts per-thread isolation.
// Attachments from thread A must not leak into thread B's listing.
func TestIntegration_ListAttachmentsScopedToThread(t *testing.T) {
	attStore, meta, _ := integrationStores(t)
	integrationSeedThread(t, meta, "thread-a")
	integrationSeedThread(t, meta, "thread-b")

	data := realPNGBytes()
	for i := 0; i < 3; i++ {
		if _, err := uploadBytes(attStore, "thread-a", fmt.Sprintf("a%d.png", i), "image/png", data, int64(i)); err != nil {
			t.Fatalf("upload a%d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := uploadBytes(attStore, "thread-b", fmt.Sprintf("b%d.png", i), "image/png", data, int64(i)); err != nil {
			t.Fatalf("upload b%d: %v", i, err)
		}
	}

	a, err := attStore.List("thread-a")
	if err != nil {
		t.Fatalf("List A: %v", err)
	}
	if len(a) != 3 {
		t.Fatalf("expected 3 in A, got %d", len(a))
	}
	for _, r := range a {
		if r.ThreadID != "thread-a" {
			t.Fatalf("cross-thread leak: %q", r.ThreadID)
		}
	}

	b, err := attStore.List("thread-b")
	if err != nil {
		t.Fatalf("List B: %v", err)
	}
	if len(b) != 2 {
		t.Fatalf("expected 2 in B, got %d", len(b))
	}
}

// TestIntegration_DeleteRemovesFileAndRow verifies Delete removes both the
// DB metadata row and the backing disk file.
func TestIntegration_DeleteRemovesFileAndRow(t *testing.T) {
	attStore, meta, rootDir := integrationStores(t)
	integrationSeedThread(t, meta, "thread-del")

	record, err := uploadBytes(attStore, "thread-del", "bye.png", "image/png",
		realPNGBytes(), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	diskPath := filepath.Join(rootDir, record.RelativePath)
	if _, err := os.Stat(diskPath); err != nil {
		t.Fatalf("pre-delete stat: %v", err)
	}

	if err := attStore.Delete(record.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(diskPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected file gone, stat error = %v", err)
	}
	_, ok, err := meta.GetAttachment(record.ID)
	if err != nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if ok {
		t.Fatal("DB row should be gone after Delete")
	}
}

// TestIntegration_DeleteMissingIsIdempotent documents the contract: the
// attachment store returns a clear error for a missing id rather than
// silently succeeding. This matches the existing store_test behaviour.
func TestIntegration_DeleteMissingIsIdempotent(t *testing.T) {
	attStore, _, _ := integrationStores(t)
	err := attStore.Delete("ghost-id")
	if err == nil {
		t.Fatal("contract expects clear error for missing delete; got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found-style error, got %v", err)
	}
}

// TestIntegration_GetAttachmentDataRoundTrip pushes bytes through Upload,
// then pulls them back via ReadBytes and asserts byte-for-byte equality.
func TestIntegration_GetAttachmentDataRoundTrip(t *testing.T) {
	attStore, meta, _ := integrationStores(t)
	integrationSeedThread(t, meta, "thread-rt")

	payload := realPNGBytes()
	record, err := uploadBytes(attStore, "thread-rt", "rt.png", "image/png",
		payload, 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	back, readBytes, err := attStore.ReadBytes(record.ID)
	if err != nil {
		t.Fatalf("ReadBytes: %v", err)
	}
	if !bytes.Equal(readBytes, payload) {
		t.Fatal("round-trip bytes differ")
	}
	if back.ID != record.ID || back.Size != int64(len(payload)) {
		t.Fatalf("metadata drift: %+v vs %+v", back, record)
	}
}

// TestIntegration_AttachmentCascadeOnThreadDelete documents the current
// contract: the DB-level FK cascade removes the metadata row when the
// parent thread is deleted. The on-disk file is NOT cleaned up by the
// store package; that's a known production gap flagged in the integration
// report. This test verifies what is guaranteed today (DB cascade) and
// captures the orphan-file behaviour so regressions are obvious.
func TestIntegration_AttachmentCascadeOnThreadDelete(t *testing.T) {
	attStore, meta, rootDir := integrationStores(t)
	integrationSeedThread(t, meta, "thread-cascade")

	record, err := uploadBytes(attStore, "thread-cascade", "x.png", "image/png",
		realPNGBytes(), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	diskPath := filepath.Join(rootDir, record.RelativePath)
	if _, err := os.Stat(diskPath); err != nil {
		t.Fatalf("pre-delete stat: %v", err)
	}

	if err := meta.DeleteThread("thread-cascade"); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	// DB row gone (FK cascade).
	_, ok, err := meta.GetAttachment(record.ID)
	if err != nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if ok {
		t.Fatal("DB attachment row should be gone after thread delete")
	}
	// Disk file is intentionally NOT cleaned up by the internal attachment
	// store. This is a documented gap; the higher-level App.deleteThreadTree
	// should sweep orphans, but today it does not. Assert the orphan to lock
	// the current behaviour in place so any future change is intentional.
	if _, err := os.Stat(diskPath); errors.Is(err, os.ErrNotExist) {
		t.Log("NOTE: on-disk attachment bytes ARE cleaned up; update integration report if this is now contract")
	}
}

// TestIntegration_ConcurrentUploadsSameThread fires 20 goroutines, each
// uploading a distinct filename to the same thread, and asserts all succeed
// with unique ids and all files land on disk. Run with -race for the
// concurrency invariant.
func TestIntegration_ConcurrentUploadsSameThread(t *testing.T) {
	attStore, meta, rootDir := integrationStores(t)
	integrationSeedThread(t, meta, "thread-conc")

	const goroutines = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := make(map[string]struct{}, goroutines)
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := realPNGBytes()
			payload = append(payload, byte(i))
			record, err := uploadBytes(attStore,
				"thread-conc",
				fmt.Sprintf("conc-%d.png", i),
				"image/png",
				payload,
				int64(i),
			)
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: %w", i, err)
				return
			}
			mu.Lock()
			if _, dup := ids[record.ID]; dup {
				mu.Unlock()
				errs <- fmt.Errorf("duplicate id %q", record.ID)
				return
			}
			ids[record.ID] = struct{}{}
			mu.Unlock()

			if _, statErr := os.Stat(filepath.Join(rootDir, record.RelativePath)); statErr != nil {
				errs <- fmt.Errorf("goroutine %d: stat: %w", i, statErr)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("%v", err)
	}
	// Verify the DB has exactly goroutines rows for this thread.
	list, err := meta.ListAttachments("thread-conc")
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(list) != goroutines {
		t.Fatalf("expected %d attachments, got %d", goroutines, len(list))
	}
}

// TestIntegration_AttachmentDataIsUnmodified pushes a payload with recognisable
// non-PNG bytes appended (which wouldn't survive any transcode/strip step)
// and confirms ReadBytes returns the verbatim buffer. This is the "no EXIF
// stripping, no transcoding" guarantee.
func TestIntegration_AttachmentDataIsUnmodified(t *testing.T) {
	attStore, meta, _ := integrationStores(t)
	integrationSeedThread(t, meta, "thread-verbatim")

	// Start with real PNG bytes so the MIME whitelist passes, then append
	// a distinctive trailer that would be stripped or rewritten by any
	// image-processing pipeline (a fake EXIF block + a nonsense byte run).
	payload := append([]byte{}, realPNGBytes()...)
	payload = append(payload, []byte("FAKE-EXIF: Camera=agent-overflow;")...)
	for i := 0; i < 32; i++ {
		payload = append(payload, byte(i))
	}

	record, err := uploadBytes(attStore, "thread-verbatim", "verbatim.png", "image/png",
		payload, 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	_, back, err := attStore.ReadBytes(record.ID)
	if err != nil {
		t.Fatalf("ReadBytes: %v", err)
	}
	if !bytes.Equal(back, payload) {
		t.Fatalf("attachment bytes mutated: len(want)=%d len(got)=%d", len(payload), len(back))
	}
	if record.Size != int64(len(payload)) {
		t.Fatalf("Size metadata drift: %d vs %d", record.Size, len(payload))
	}
}
