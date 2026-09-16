package attachment

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/store"
)

// ErrImportOutsideRoot is a source path that does not live under the
// directory the caller allowed. Typed so a caller can report "that file is
// not where generated images go" separately from "that file is unreadable".
var ErrImportOutsideRoot = errors.New("attachment: source path is outside the permitted directory")

// ImportImageFromDir copies an image the machine already holds into this
// thread's attachments and returns its row.
//
// allowedDir is the ONLY directory a source may come from, and containment is
// decided after BOTH paths are canonicalized (filepath.EvalSymlinks), so a
// symlink planted inside the directory cannot reach out of it. The rest of the
// checks are the same ones a client upload passes, in the same order: a
// regular file, a bounded length, and a signature that proves the bytes are
// one of the four formats this application renders. Upload then re-checks the
// signature against the MIME reported here and performs the atomic
// stage → insert → rename publish, so an import is exactly as durable as an
// upload and never publishes a half-written row.
//
// The length is checked against MaxSizeFor(image) — the store's ONE answer to
// how large an image row may be — from the stat, before the file is opened.
// An import is not a special case with a cap of its own: a second number here
// would mean an oversize file was opened and hashed only to be refused by
// Upload with a different message.
//
// The declared length is the size observed on the canonical path. A file
// rewritten between the stat and the read therefore fails the exact-length
// contract rather than landing truncated.
func (s *Store) ImportImageFromDir(threadID, allowedDir, sourcePath string, createdAt int64) (store.Attachment, error) {
	resolved, err := resolveImportPath(allowedDir, sourcePath)
	if err != nil {
		return store.Attachment{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return store.Attachment{}, fmt.Errorf("attachment: stat import source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return store.Attachment{}, fmt.Errorf("attachment: import source %s is not a regular file", filepath.Base(resolved))
	}
	size := info.Size()
	if size <= 0 {
		return store.Attachment{}, errors.New("attachment: import source is empty")
	}
	if maxSize := s.MaxSizeFor(store.AttachmentKindImage); size > maxSize {
		return store.Attachment{}, fmt.Errorf("attachment: import source is %d bytes, max %d", size, maxSize)
	}

	file, err := os.Open(resolved)
	if err != nil {
		return store.Attachment{}, fmt.Errorf("attachment: open import source: %w", err)
	}
	defer file.Close()

	header := make([]byte, signatureBytes)
	read, err := io.ReadFull(file, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return store.Attachment{}, fmt.Errorf("attachment: read import source: %w", err)
	}
	mime, err := DetectImageMIME(header[:read])
	if err != nil {
		return store.Attachment{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return store.Attachment{}, fmt.Errorf("attachment: rewind import source: %w", err)
	}
	return s.Upload(threadID, importedImageFilename(resolved, mime), mime, size, file, createdAt)
}

// resolveImportPath canonicalizes both paths and answers the containment
// question on the canonical forms. An unresolvable allowedDir is a refusal
// rather than a fallback to the lexical path: "the directory imports are
// allowed from does not exist" must never read as "anywhere is fine".
func resolveImportPath(allowedDir, sourcePath string) (string, error) {
	if strings.TrimSpace(allowedDir) == "" {
		return "", errors.New("attachment: permitted import directory is required")
	}
	if strings.TrimSpace(sourcePath) == "" {
		return "", errors.New("attachment: import source path is required")
	}
	if !filepath.IsAbs(sourcePath) {
		return "", fmt.Errorf("attachment: import source path must be absolute: %w", ErrImportOutsideRoot)
	}
	root, err := filepath.EvalSymlinks(allowedDir)
	if err != nil {
		return "", fmt.Errorf("attachment: resolve permitted import directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(sourcePath)
	if err != nil {
		return "", fmt.Errorf("attachment: resolve import source: %w", err)
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", fmt.Errorf("attachment: compare import source with permitted directory: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
		return "", ErrImportOutsideRoot
	}
	return resolved, nil
}

// importedImageFilename is the name the row carries: the source's own base
// name with the extension the DETECTED format owns, so a `.png` holding JPEG
// bytes is not recorded under a lie. Images are stored as `<id><ext>`, so
// this name is metadata (alt text, a download name) rather than a path.
func importedImageFilename(resolved, mime string) string {
	base := sanitizeFilename(filepath.Base(resolved))
	ext := allowedMIMEs[mime]
	if current := strings.ToLower(filepath.Ext(base)); current != "" {
		if _, isImage := allowedExtensions[current]; isImage {
			base = strings.TrimSuffix(base, filepath.Ext(base))
		}
	}
	if base == "" || base == "file" {
		base = "generated-image"
	}
	return base + ext
}
