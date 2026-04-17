// Package attachment manages disk storage for image attachments tied to
// threads. The SQLite side keeps metadata (id, size, mime, path); this
// package validates + writes the raw bytes to a bounded on-disk layout.
package attachment

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// DefaultMaxSize is the largest attachment payload we accept. The knob is
// configurable via Config.MaxSize but defaults to 10 MiB to match forge.
const DefaultMaxSize int64 = 10 * 1024 * 1024

// allowedMIMEs maps MIME types to the filename extension we persist them as.
// Keeping this tight (images only) makes the attachment dir safe to serve as
// static content.
var allowedMIMEs = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/jpg":  ".jpg",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// allowedExtensions is a whitelist of filename extensions we accept. Used to
// derive a MIME type when the caller did not provide one.
var allowedExtensions = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
}

// Config describes runtime knobs for the store. MaxSize of 0 means "use the
// default"; a negative MaxSize means "no upload allowed" and is rejected so a
// misconfiguration fails loudly rather than silently rejecting everything.
type Config struct {
	RootDir string
	MaxSize int64
}

// Store writes attachments to disk and records metadata via the SQLite
// store. Callers must supply a store.Store; metadata-only round-trips go
// through that, and this package owns the byte layout on disk.
type Store struct {
	root    string
	maxSize int64
	meta    *store.Store
}

// NewStore creates the root directory if needed and returns a ready store.
// Errors are fatal — callers are expected to return them from startup.
func NewStore(cfg Config, meta *store.Store) (*Store, error) {
	if cfg.RootDir == "" {
		return nil, fmt.Errorf("attachment: root directory is required")
	}
	if meta == nil {
		return nil, fmt.Errorf("attachment: store reference is required")
	}
	if cfg.MaxSize < 0 {
		return nil, fmt.Errorf("attachment: negative max size is invalid")
	}
	if cfg.MaxSize == 0 {
		cfg.MaxSize = DefaultMaxSize
	}
	if err := os.MkdirAll(cfg.RootDir, 0o755); err != nil {
		return nil, fmt.Errorf("attachment: create root %s: %w", cfg.RootDir, err)
	}
	return &Store{root: cfg.RootDir, maxSize: cfg.MaxSize, meta: meta}, nil
}

// Upload accepts base64-encoded bytes, validates them, writes to disk, and
// inserts a metadata row. ThreadID must reference an existing thread (the
// FK constraint in the attachments table catches violations).
func (s *Store) Upload(threadID, filename, mimeType, dataB64 string, createdAt int64) (store.Attachment, error) {
	if strings.TrimSpace(threadID) == "" {
		return store.Attachment{}, errors.New("attachment: thread id is required")
	}
	if strings.TrimSpace(filename) == "" {
		return store.Attachment{}, errors.New("attachment: filename is required")
	}

	data, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return store.Attachment{}, fmt.Errorf("attachment: decode base64: %w", err)
	}
	if int64(len(data)) == 0 {
		return store.Attachment{}, errors.New("attachment: payload is empty")
	}
	if int64(len(data)) > s.maxSize {
		return store.Attachment{}, fmt.Errorf("attachment: payload %d bytes exceeds limit %d", len(data), s.maxSize)
	}

	normalizedMIME, ext, err := validateType(mimeType, filename)
	if err != nil {
		return store.Attachment{}, err
	}

	id := uuid.NewString()
	relativePath := filepath.Join(sanitizeThreadID(threadID), id+ext)
	absolutePath := filepath.Join(s.root, relativePath)

	// Extra defence in depth: make sure we stayed under the root after
	// path-joining, even though sanitizeThreadID already strips separators.
	absRoot, err := filepath.Abs(s.root)
	if err != nil {
		return store.Attachment{}, fmt.Errorf("attachment: absolute root: %w", err)
	}
	absFile, err := filepath.Abs(absolutePath)
	if err != nil {
		return store.Attachment{}, fmt.Errorf("attachment: absolute file: %w", err)
	}
	if !strings.HasPrefix(absFile, absRoot+string(os.PathSeparator)) {
		return store.Attachment{}, fmt.Errorf("attachment: refusing to write outside %s", absRoot)
	}

	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
		return store.Attachment{}, fmt.Errorf("attachment: mkdir: %w", err)
	}
	if err := os.WriteFile(absolutePath, data, 0o644); err != nil {
		return store.Attachment{}, fmt.Errorf("attachment: write file: %w", err)
	}

	record := store.Attachment{
		ID:           id,
		ThreadID:     threadID,
		Filename:     filename,
		MimeType:     normalizedMIME,
		Size:         int64(len(data)),
		RelativePath: filepath.ToSlash(relativePath),
		CreatedAt:    createdAt,
	}
	if err := s.meta.InsertAttachment(record); err != nil {
		// Roll back the on-disk write so disk and DB stay consistent.
		_ = os.Remove(absolutePath)
		return store.Attachment{}, err
	}
	return record, nil
}

// Get returns the metadata row and the resolved absolute on-disk path.
// Second return value is false when no attachment has that id.
func (s *Store) Get(attachmentID string) (store.Attachment, string, bool, error) {
	record, ok, err := s.meta.GetAttachment(attachmentID)
	if err != nil || !ok {
		return store.Attachment{}, "", ok, err
	}
	absolutePath, err := s.resolveAbsolute(record.RelativePath)
	if err != nil {
		return store.Attachment{}, "", false, err
	}
	return record, absolutePath, true, nil
}

// ReadBytes returns the raw on-disk bytes for an attachment.
func (s *Store) ReadBytes(attachmentID string) (store.Attachment, []byte, error) {
	record, absolutePath, ok, err := s.Get(attachmentID)
	if err != nil {
		return store.Attachment{}, nil, err
	}
	if !ok {
		return store.Attachment{}, nil, fmt.Errorf("attachment: id %q not found", attachmentID)
	}
	data, err := os.ReadFile(absolutePath)
	if err != nil {
		return store.Attachment{}, nil, fmt.Errorf("attachment: read file: %w", err)
	}
	return record, data, nil
}

// List returns metadata for every attachment on a thread.
func (s *Store) List(threadID string) ([]store.Attachment, error) {
	return s.meta.ListAttachments(threadID)
}

// DeleteThreadDir removes every on-disk attachment file for a thread by
// deleting the thread's attachment directory under the store root. Safe to
// call after the DB cascade has already dropped the metadata rows; a missing
// directory is treated as success.
func (s *Store) DeleteThreadDir(threadID string) error {
	if strings.TrimSpace(threadID) == "" {
		return errors.New("attachment: thread id is required")
	}
	sanitized := sanitizeThreadID(threadID)
	threadDir := filepath.Join(s.root, sanitized)
	absRoot, err := filepath.Abs(s.root)
	if err != nil {
		return fmt.Errorf("attachment: absolute root: %w", err)
	}
	absDir, err := filepath.Abs(threadDir)
	if err != nil {
		return fmt.Errorf("attachment: absolute dir: %w", err)
	}
	if !strings.HasPrefix(absDir, absRoot+string(os.PathSeparator)) {
		return fmt.Errorf("attachment: refusing to remove path outside %s", absRoot)
	}
	if err := os.RemoveAll(threadDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("attachment: remove thread dir: %w", err)
	}
	return nil
}

// Delete removes the row and the backing file. Missing files are treated as
// success because the thing we were asked to delete is already gone.
func (s *Store) Delete(attachmentID string) error {
	record, ok, err := s.meta.GetAttachment(attachmentID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("attachment: id %q not found", attachmentID)
	}
	if err := s.meta.DeleteAttachment(attachmentID); err != nil {
		return err
	}
	absolutePath, err := s.resolveAbsolute(record.RelativePath)
	if err != nil {
		return err
	}
	if err := os.Remove(absolutePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("attachment: remove file: %w", err)
	}
	return nil
}

func (s *Store) resolveAbsolute(relativePath string) (string, error) {
	if relativePath == "" {
		return "", errors.New("attachment: relative path is empty")
	}
	clean := filepath.Clean(relativePath)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("attachment: unsafe relative path %q", relativePath)
	}
	absolutePath := filepath.Join(s.root, clean)
	absRoot, err := filepath.Abs(s.root)
	if err != nil {
		return "", fmt.Errorf("attachment: absolute root: %w", err)
	}
	absFile, err := filepath.Abs(absolutePath)
	if err != nil {
		return "", fmt.Errorf("attachment: absolute file: %w", err)
	}
	if !strings.HasPrefix(absFile, absRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("attachment: resolved path escapes root")
	}
	return absolutePath, nil
}

// validateType returns the canonical MIME type and filename extension. We
// trust the MIME when it's on the whitelist; otherwise we fall back to the
// extension. Anything outside the whitelist is rejected.
func validateType(mimeType, filename string) (string, string, error) {
	mime := strings.ToLower(strings.TrimSpace(mimeType))
	if ext, ok := allowedMIMEs[mime]; ok {
		return mime, ext, nil
	}

	ext := strings.ToLower(filepath.Ext(filename))
	if mimeFromExt, ok := allowedExtensions[ext]; ok {
		return mimeFromExt, ext, nil
	}
	if mime == "" {
		return "", "", fmt.Errorf("attachment: unable to infer image type from filename %q", filename)
	}
	return "", "", fmt.Errorf("attachment: disallowed mime type %q", mime)
}

// sanitizeThreadID strips path separators from the thread id so it can be
// used as a directory segment. Thread ids in our system are UUIDs so this is
// defence in depth.
func sanitizeThreadID(threadID string) string {
	replacer := strings.NewReplacer(
		string(os.PathSeparator), "-",
		"/", "-",
		"\\", "-",
		"..", "-",
	)
	clean := replacer.Replace(strings.TrimSpace(threadID))
	if clean == "" {
		clean = "unknown"
	}
	return clean
}
