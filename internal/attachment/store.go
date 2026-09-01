// Package attachment manages disk storage for image attachments tied to
// threads. The SQLite side keeps metadata (id, size, mime, path); this
// package validates + writes the raw bytes to a bounded on-disk layout.
package attachment

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// DefaultMaxSize is the largest attachment payload we accept. The knob is
// configurable via Config.MaxSize but defaults to 10 MiB.
const DefaultMaxSize int64 = 10 * 1024 * 1024

// DefaultMaxCount is the largest number of image attachments accepted for a
// single user turn. Kept with the attachment policy constants so UI/backend
// mirrors have one backend source of truth.
const DefaultMaxCount = 8

const (
	privateDirPerm    os.FileMode = 0o700
	sensitiveFilePerm os.FileMode = 0o600
)

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
	if err := ensurePrivateTree(cfg.RootDir); err != nil {
		return nil, fmt.Errorf("attachment: create root %s: %w", cfg.RootDir, err)
	}
	return &Store{root: cfg.RootDir, maxSize: cfg.MaxSize, meta: meta}, nil
}

// MaxSize is the largest payload this store accepts, so a caller that
// must refuse an oversize transfer BEFORE it starts reads the same number
// the write path enforces rather than restating the default.
func (s *Store) MaxSize() int64 { return s.maxSize }

// copyBufferSize is how much of an upload is in memory at once. The whole
// point of the streaming write is that a 10 MiB image never exists as a
// single heap buffer, so this is sized for syscall efficiency rather than
// for the payload: it also backs the header peek below, which needs 12
// bytes.
const copyBufferSize = 32 << 10

// Upload STREAMS one attachment body onto disk, validates it, and inserts
// a metadata row atomically from the caller's point of view. The sequence
// is: write to a tmp sibling file first, INSERT the DB row, then atomic
// rename to the final path on commit. If the DB insert fails the tmp file
// is removed; if the atomic rename fails the DB row is deleted. ThreadID
// must reference an existing thread (FK enforced).
//
// The tmp-then-rename pattern means a crash at ANY point leaves a
// consistent view: either the DB row + final file both exist, or
// neither does. A tmp file left behind after a crash is detectable by
// its .tmp suffix; we don't currently sweep those, but they're bounded
// in size and never referenced from any code path.
//
// declaredSize is what the caller was told to expect, and the body must
// deliver EXACTLY that many bytes. Two parties agreeing on a length
// before the transfer is what lets an HTTP caller refuse an oversize
// request from its headers instead of after 10 MiB of it arrived, and it
// is why a short body is a failure here rather than a truncated image
// nobody notices until it renders. The cap is enforced inside this
// function too (io.LimitReader below), so a caller that forgot its own
// bound still cannot make this store write past MaxSize.
//
// The image-signature check reads the first bytes through a peek rather
// than the whole payload: the format is decided by at most 12 bytes, and
// buffering the rest to look at them would put back exactly the
// allocation this path exists to remove.
func (s *Store) Upload(threadID, filename, mimeType string, declaredSize int64, body io.Reader, createdAt int64) (store.Attachment, error) {
	if strings.TrimSpace(threadID) == "" {
		return store.Attachment{}, errors.New("attachment: thread id is required")
	}
	if strings.TrimSpace(filename) == "" {
		return store.Attachment{}, errors.New("attachment: filename is required")
	}
	if body == nil {
		return store.Attachment{}, errors.New("attachment: body is required")
	}
	if declaredSize <= 0 {
		return store.Attachment{}, errors.New("attachment: payload is empty")
	}
	if declaredSize > s.maxSize {
		return store.Attachment{}, fmt.Errorf("attachment: payload %d bytes exceeds limit %d", declaredSize, s.maxSize)
	}

	normalizedMIME, ext, err := ValidateType(mimeType, filename)
	if err != nil {
		return store.Attachment{}, err
	}

	id := uuid.NewString()
	relativePath := filepath.Join(sanitizeThreadID(threadID), id+ext)
	absolutePath := filepath.Join(s.root, relativePath)
	tmpPath := absolutePath + ".tmp"

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

	if err := os.MkdirAll(filepath.Dir(absolutePath), privateDirPerm); err != nil {
		return store.Attachment{}, fmt.Errorf("attachment: mkdir: %w", err)
	}
	// Stage bytes to a sibling tmp file so a crash between here and the
	// final rename leaves only a .tmp (no orphan row, no visible final
	// file). Removed on every failure below by this one defer, since a
	// streaming write has more ways to fail part-way than a single
	// os.WriteFile did.
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	written, err := s.writeTemp(tmpPath, normalizedMIME, declaredSize, body)
	if err != nil {
		return store.Attachment{}, err
	}
	if written != declaredSize {
		return store.Attachment{}, fmt.Errorf("attachment: body delivered %d bytes, declared %d", written, declaredSize)
	}

	record := store.Attachment{
		ID:           id,
		ThreadID:     threadID,
		Filename:     filename,
		MimeType:     normalizedMIME,
		Size:         declaredSize,
		RelativePath: filepath.ToSlash(relativePath),
		CreatedAt:    createdAt,
	}
	if err := s.meta.InsertAttachment(record); err != nil {
		// DB row never landed; the deferred remove tears down the tmp file.
		return store.Attachment{}, err
	}

	// Atomic rename publishes the file at its final path. If this fails
	// (e.g. FS error between directories), roll back the DB row so we
	// don't leave a metadata row pointing at a path that doesn't exist.
	if err := os.Rename(tmpPath, absolutePath); err != nil {
		if derr := s.meta.DeleteAttachment(record.ID); derr != nil {
			return store.Attachment{}, fmt.Errorf("attachment: rename %s → %s: %w (rollback also failed: %v)",
				tmpPath, absolutePath, err, derr)
		}
		return store.Attachment{}, fmt.Errorf("attachment: rename %s → %s: %w", tmpPath, absolutePath, err)
	}
	committed = true
	return record, nil
}

// writeTemp streams the body into the staging file and reports how many
// bytes landed. The caller owns removing the file on any error.
//
// The limit is declaredSize+1 rather than declaredSize so an over-long
// body is DETECTED (the extra byte makes the count disagree) instead of
// being silently truncated to exactly the length it claimed.
func (s *Store) writeTemp(tmpPath, normalizedMIME string, declaredSize int64, body io.Reader) (int64, error) {
	file, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, sensitiveFilePerm)
	if err != nil {
		return 0, fmt.Errorf("attachment: create tmp file: %w", err)
	}
	reader := bufio.NewReaderSize(io.LimitReader(body, declaredSize+1), copyBufferSize)
	header, err := reader.Peek(signatureBytes)
	if err != nil && !errors.Is(err, io.EOF) {
		_ = file.Close()
		return 0, fmt.Errorf("attachment: read payload: %w", err)
	}
	// Judged on the signature, before a single byte is committed: a body
	// whose first bytes are not the image type it claims is refused
	// without writing the rest of it.
	if err := validateImagePayload(normalizedMIME, header); err != nil {
		_ = file.Close()
		return 0, err
	}
	written, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	if copyErr != nil {
		return written, fmt.Errorf("attachment: write payload: %w", copyErr)
	}
	if closeErr != nil {
		return written, fmt.Errorf("attachment: close tmp file: %w", closeErr)
	}
	return written, nil
}

// Content is one attachment opened for streaming: its metadata row, an
// open handle on the bytes, and the modification time a conditional
// request is answered from.
//
// The CALLER closes File. Handing back a handle rather than a []byte is
// the point: the byte route streams a 10 MiB image through a 32 KiB
// buffer instead of holding the whole file — and, before wave 6b, its
// base64 inflation as well — in the heap at once.
type Content struct {
	Record  store.Attachment
	File    *os.File
	ModTime time.Time
}

// OpenThread opens an attachment's bytes for streaming, enforcing the same
// thread-ownership check ReadThreadBytes does and for the same reason: a
// stale cross-thread id must not reference another thread's file. The
// ownership check runs on metadata, before the file is touched.
func (s *Store) OpenThread(threadID, attachmentID string) (Content, error) {
	record, absolutePath, err := s.resolveThreadAttachment(threadID, attachmentID)
	if err != nil {
		return Content{}, err
	}
	file, err := os.Open(absolutePath)
	if err != nil {
		return Content{}, fmt.Errorf("attachment: open file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return Content{}, fmt.Errorf("attachment: stat file: %w", err)
	}
	return Content{Record: record, File: file, ModTime: info.ModTime()}, nil
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

// resolveThreadAttachment resolves an attachment's metadata and absolute path,
// enforcing thread ownership so a stale cross-thread id cannot reference another
// thread's file. Shared by the byte-read and path-only accessors; the ownership
// check is the boundary that keeps one thread from forcing a read of (or a
// reference to) another thread's attachment.
func (s *Store) resolveThreadAttachment(threadID, attachmentID string) (store.Attachment, string, error) {
	record, absolutePath, ok, err := s.Get(attachmentID)
	if err != nil {
		return store.Attachment{}, "", err
	}
	if !ok {
		return store.Attachment{}, "", fmt.Errorf("attachment: id %q not found", attachmentID)
	}
	if record.ThreadID != threadID {
		return store.Attachment{}, "", fmt.Errorf("attachment %q belongs to thread %s, not %s", attachmentID, record.ThreadID, threadID)
	}
	return record, absolutePath, nil
}

// ReadThreadBytes returns bytes only when the attachment belongs to the
// expected thread. Ownership is checked from metadata before reading the file
// so stale cross-thread IDs cannot force unnecessary large reads.
func (s *Store) ReadThreadBytes(threadID, attachmentID string) (store.Attachment, []byte, error) {
	record, absolutePath, err := s.resolveThreadAttachment(threadID, attachmentID)
	if err != nil {
		return store.Attachment{}, nil, err
	}
	data, err := os.ReadFile(absolutePath)
	if err != nil {
		return store.Attachment{}, nil, fmt.Errorf("attachment: read file: %w", err)
	}
	return record, data, nil
}

// PathForThread returns the absolute on-disk path for an attachment, enforcing
// the same thread-ownership check as ReadThreadBytes but skipping the file read.
// Used by providers that ingest an image by path (claude-tui pastes the path
// into the real TUI composer) rather than by inline bytes, so a send never reads
// image bytes it won't use.
func (s *Store) PathForThread(threadID, attachmentID string) (store.Attachment, string, error) {
	return s.resolveThreadAttachment(threadID, attachmentID)
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

// signatureBytes is how much of a payload decides its image type — the
// longest signature DetectImageMIME reads is WEBP's twelve. It is the
// whole header the streaming write peeks at, which is what keeps "is this
// really a PNG" from costing a full-payload buffer.
const signatureBytes = 12

// ValidateType returns the canonical MIME type and filename extension. We
// trust the MIME when it's on the whitelist; otherwise we fall back to the
// extension. Anything outside the whitelist is rejected.
//
// Exported so a caller that authorizes a transfer BEFORE it happens can
// refuse a disallowed type without a second copy of the allow-list —
// spending a round trip on a name this store would reject after the bytes
// arrived is a worse answer, not a safer one. It is a pre-check and never
// a substitute: Upload calls it again, and the signature check that runs
// beside it there is the one a filename cannot talk its way past.
func ValidateType(mimeType, filename string) (string, string, error) {
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

// DetectImageMIME identifies the image formats Agent Overflow can render.
// It reads signatures rather than trusting a filename or caller-provided
// content type, so filesystem-backed markdown images and uploaded
// attachments share one byte-level allowlist.
func DetectImageMIME(data []byte) (string, error) {
	if len(data) < 4 {
		return "", fmt.Errorf("attachment: image payload is too short")
	}
	if len(data) >= 8 &&
		data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G' &&
		data[4] == 0x0D && data[5] == 0x0A && data[6] == 0x1A && data[7] == 0x0A {
		return "image/png", nil
	}
	if len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return "image/jpeg", nil
	}
	if len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a") {
		return "image/gif", nil
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp", nil
	}
	return "", fmt.Errorf("attachment: payload is not a supported image")
}

func validateImagePayload(mimeType string, data []byte) error {
	detected, err := DetectImageMIME(data)
	if err != nil {
		return err
	}
	want := mimeType
	if want == "image/jpg" {
		want = "image/jpeg"
	}
	if detected == want {
		return nil
	}
	return fmt.Errorf("attachment: payload does not match %s (detected %s)", mimeType, detected)
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

func ensurePrivateTree(root string) error {
	if err := ensurePrivateDir(root); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return os.Chmod(path, privateDirPerm)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			return os.Chmod(path, sensitiveFilePerm)
		}
		return nil
	})
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, privateDirPerm); err != nil {
		return err
	}
	return os.Chmod(path, privateDirPerm)
}
