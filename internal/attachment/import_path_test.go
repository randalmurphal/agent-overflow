package attachment

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

func writeImportSource(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestImportImageFromDirCopiesBytes(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	allowed := t.TempDir()
	source := writeImportSource(t, allowed, "render.png", pngData(t))

	record, err := attStore.ImportImageFromDir("t1", allowed, source, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if record.ThreadID != "t1" || record.MimeType != "image/png" || record.Kind != "image" {
		t.Fatalf("record = %+v", record)
	}
	if record.Size != int64(len(pngData(t))) {
		t.Fatalf("record size = %d, want %d", record.Size, len(pngData(t)))
	}
	if record.Filename != "render.png" {
		t.Fatalf("filename = %q, want render.png", record.Filename)
	}

	_, data, err := attStore.ReadThreadBytes("t1", record.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != string(pngData(t)) {
		t.Fatalf("stored bytes differ from the source")
	}
	// The source is left where the provider put it; import is a copy.
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source must survive the import: %v", err)
	}
}

func TestImportImageFromDirNamesFileByDetectedFormat(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	allowed := t.TempDir()
	// A `.png` name over JPEG bytes: the row records what the bytes are.
	source := writeImportSource(t, allowed, "render.png", jpegData(t))

	record, err := attStore.ImportImageFromDir("t1", allowed, source, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if record.MimeType != "image/jpeg" {
		t.Fatalf("mime = %q, want image/jpeg", record.MimeType)
	}
	if filepath.Ext(record.Filename) != ".jpg" {
		t.Fatalf("filename = %q, want a .jpg extension", record.Filename)
	}
}

func TestImportImageFromDirRefusesOutsideSources(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	allowed := t.TempDir()
	outside := t.TempDir()
	outsideFile := writeImportSource(t, outside, "elsewhere.png", pngData(t))

	cases := []struct {
		name   string
		source func() string
	}{
		{"sibling directory", func() string { return outsideFile }},
		{"traversal", func() string { return filepath.Join(allowed, "..", filepath.Base(outside), "elsewhere.png") }},
		{"relative path", func() string { return "render.png" }},
		{"empty path", func() string { return "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := attStore.ImportImageFromDir("t1", allowed, tc.source(), time.Now().UnixMilli()); err == nil {
				t.Fatal("expected refusal")
			}
		})
	}
}

func TestImportImageFromDirRefusesSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	allowed := t.TempDir()
	outside := t.TempDir()
	target := writeImportSource(t, outside, "secret.png", pngData(t))

	// The link LIVES in the allowed directory and points out of it. A lexical
	// containment check would accept it; canonicalizing both sides does not.
	link := filepath.Join(allowed, "render.png")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := attStore.ImportImageFromDir("t1", allowed, link, time.Now().UnixMilli())
	if !errors.Is(err, ErrImportOutsideRoot) {
		t.Fatalf("err = %v, want ErrImportOutsideRoot", err)
	}
}

func TestImportImageFromDirAcceptsSymlinkWithinRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	allowed := t.TempDir()
	target := writeImportSource(t, allowed, "nested/render.png", pngData(t))
	link := filepath.Join(allowed, "latest.png")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := attStore.ImportImageFromDir("t1", allowed, link, time.Now().UnixMilli()); err != nil {
		t.Fatalf("a link resolving inside the root is allowed: %v", err)
	}
}

func TestImportImageFromDirRefusesNonRegularSource(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	allowed := t.TempDir()
	dir := filepath.Join(allowed, "subdir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if _, err := attStore.ImportImageFromDir("t1", allowed, dir, time.Now().UnixMilli()); err == nil {
		t.Fatal("expected a directory to be refused")
	}
}

func TestImportImageFromDirRefusesEmptyAndOversizedSources(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	allowed := t.TempDir()

	empty := writeImportSource(t, allowed, "empty.png", nil)
	if _, err := attStore.ImportImageFromDir("t1", allowed, empty, time.Now().UnixMilli()); err == nil {
		t.Fatal("expected an empty file to be refused")
	}

	// One cap, the store's own: the refusal names MaxSizeFor(image) and is
	// decided from the stat, so the file is never opened. Written sparse so
	// the test does not produce megabytes of real bytes.
	maxSize := attStore.MaxSizeFor(store.AttachmentKindImage)
	huge := filepath.Join(allowed, "huge.png")
	file, err := os.Create(huge)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := file.Truncate(maxSize + 1); err != nil {
		file.Close()
		t.Fatalf("truncate: %v", err)
	}
	file.Close()
	// Unreadable, so an import that got as far as opening it would fail with
	// a permission error instead of the size refusal under test.
	if err := os.Chmod(huge, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(huge, 0o644) })

	_, err = attStore.ImportImageFromDir("t1", allowed, huge, time.Now().UnixMilli())
	if err == nil {
		t.Fatal("expected an oversized file to be refused")
	}
	if !strings.Contains(err.Error(), strconv.FormatInt(maxSize, 10)) {
		t.Fatalf("refusal must name the store's image cap %d: %v", maxSize, err)
	}
}

// The import path must not carry a cap of its own: a file the store would
// refuse has to be refused with the store's number, and one the store accepts
// has to land.
func TestImportImageFromDirUsesTheStoresImageCap(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	if got := attStore.MaxSizeFor(store.AttachmentKindImage); got != DefaultMaxSize {
		t.Fatalf("image cap = %d, want DefaultMaxSize %d", got, DefaultMaxSize)
	}
	allowed := t.TempDir()
	source := writeImportSource(t, allowed, "render.png", pngData(t))
	if _, err := attStore.ImportImageFromDir("t1", allowed, source, time.Now().UnixMilli()); err != nil {
		t.Fatalf("a file under the cap must import: %v", err)
	}
}

func TestImportImageFromDirRefusesNonImageBytes(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	allowed := t.TempDir()
	source := writeImportSource(t, allowed, "render.png", []byte("#!/bin/sh\nrm -rf /\n"))

	if _, err := attStore.ImportImageFromDir("t1", allowed, source, time.Now().UnixMilli()); err == nil {
		t.Fatal("expected non-image bytes to be refused")
	}
	// Nothing was published: the thread has no attachment row.
	rows, err := attStore.List("t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("attachments = %d, want 0", len(rows))
	}
}

func TestImportImageFromDirRefusesMissingAllowedDir(t *testing.T) {
	attStore, meta := newTestStores(t)
	seedThread(t, meta, "t1")
	allowed := t.TempDir()
	source := writeImportSource(t, allowed, "render.png", pngData(t))

	// An unresolvable permitted directory must refuse, never widen to "any".
	if _, err := attStore.ImportImageFromDir("t1", filepath.Join(allowed, "missing"), source, time.Now().UnixMilli()); err == nil {
		t.Fatal("expected a missing permitted directory to be refused")
	}
	if _, err := attStore.ImportImageFromDir("t1", "", source, time.Now().UnixMilli()); err == nil {
		t.Fatal("expected an empty permitted directory to be refused")
	}
}
