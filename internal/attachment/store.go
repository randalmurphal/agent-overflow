// Package attachment manages disk storage for the files tied to threads.
// The SQLite side keeps metadata (id, size, mime, path, kind); this package
// decides the KIND, validates it, and writes the raw bytes to a bounded
// on-disk layout.
//
// Two kinds, and the kind is what every other layer switches on
// (docs/specs/file-attachments.md):
//
//   - store.AttachmentKindImage — a declared image MIME or image extension
//     whose bytes really are that image. Delivered to the provider as inline
//     bytes or a local path, bound positionally to a `[Image #N]` marker, and
//     the only kind whose bytes are ever served back to a client.
//   - store.AttachmentKindFile — everything else, at face value. Copied to
//     the root under its own directory so the agent-facing path carries the
//     real filename, referenced by that path in the prompt, and never
//     decoded, thumbnailed, or served.
//
// Nothing is ever reclassified INTO an image: a payload that declares itself
// an image and is not one is refused, not demoted to a file.
package attachment

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// DefaultMaxSize is the largest IMAGE payload we accept. The knob is
// configurable via Config.MaxSize but defaults to 10 MiB.
const DefaultMaxSize int64 = 10 * 1024 * 1024

// DefaultMaxFileSize is the largest non-image payload we accept
// (Config.MaxFileSize). Bigger than the image cap because an image is
// re-encoded into a model's context while a file is only referenced by
// path — but still bounded, because the transitional transfer carrier
// base64s the bytes through one WS frame.
const DefaultMaxFileSize int64 = 50 * 1024 * 1024

// DefaultMaxCount is the largest number of attachments accepted for a single
// user turn, images and files together. Kept with the attachment policy
// constants so UI/backend mirrors have one backend source of truth.
const DefaultMaxCount = 8

// ErrNotAnImage is what the image-only accessors return for a `file` row.
// Typed so a caller can tell "this attachment cannot be decoded" from "this
// attachment is broken" without matching on message text.
var ErrNotAnImage = errors.New("attachment: not an image")

const (
	privateDirPerm    os.FileMode = 0o700
	sensitiveFilePerm os.FileMode = 0o600
)

// maxFilenameBytes bounds the sanitized on-disk name of a `file`
// attachment. Well under the 255-byte NAME_MAX every filesystem we run on
// allows, with room for the `.tmp` suffix the staged write appends.
const maxFilenameBytes = 128

// allowedMIMEs maps image MIME types to the filename extension we persist
// them as. A declared MIME on this list makes the upload an IMAGE, which
// means its bytes must match — see classifyUpload.
var allowedMIMEs = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/jpg":  ".jpg",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// allowedExtensions is the same list keyed by filename extension, for the
// upload that declares no MIME (or a non-image one) but names an image file.
var allowedExtensions = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
}

// fallbackFileMIME is what a `file` upload records when the caller declared
// no content type. A browser drop of an unknown extension carries exactly
// that, and an empty mime_type column would be a second spelling of it.
const fallbackFileMIME = "application/octet-stream"

// maxDeclaredMIMEBytes bounds the caller-declared content type a `file`
// upload is stored with. Files skip the MIME allowlist entirely, so this is
// the only thing between an unbounded wire string and the metadata row.
const maxDeclaredMIMEBytes = 128

// Config describes runtime knobs for the store. A zero MaxSize / MaxFileSize
// means "use the default"; a negative one means "no upload allowed" and is
// rejected so a misconfiguration fails loudly rather than silently rejecting
// everything.
type Config struct {
	RootDir     string
	MaxSize     int64
	MaxFileSize int64
}

// Store writes attachments to disk and records metadata via the SQLite
// store. Callers must supply a store.Store; metadata-only round-trips go
// through that, and this package owns the byte layout on disk.
type Store struct {
	root        string
	maxSize     int64
	maxFileSize int64
	meta        *store.Store
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
	if cfg.MaxSize < 0 || cfg.MaxFileSize < 0 {
		return nil, fmt.Errorf("attachment: negative max size is invalid")
	}
	if cfg.MaxSize == 0 {
		cfg.MaxSize = DefaultMaxSize
	}
	if cfg.MaxFileSize == 0 {
		cfg.MaxFileSize = DefaultMaxFileSize
	}
	if err := ensurePrivateTree(cfg.RootDir); err != nil {
		return nil, fmt.Errorf("attachment: create root %s: %w", cfg.RootDir, err)
	}
	return &Store{root: cfg.RootDir, maxSize: cfg.MaxSize, maxFileSize: cfg.MaxFileSize, meta: meta}, nil
}

// Root is the directory every attachment lives under. Exposed because the
// Claude spawn passes it as `--add-dir`, so the agent can Read an attachment
// without a permission prompt; the app must not re-derive the path.
func (s *Store) Root() string { return s.root }

// Upload accepts base64-encoded bytes, decides the kind, validates them,
// writes the file and inserts a metadata row atomically from the caller's
// point of view. The sequence is: write to a tmp file first, INSERT the DB
// row, then atomic rename to the final path on commit. If the DB insert
// fails the staged bytes are removed; if the atomic rename fails the DB row
// is deleted. ThreadID must reference an existing thread (FK enforced).
//
// The tmp-then-rename pattern means a crash at ANY point leaves a
// consistent view: either the DB row + final file both exist, or
// neither does. A tmp file left behind after a crash is detectable by
// its .tmp suffix; we don't currently sweep those, but they're bounded
// in size and never referenced from any code path.
//
// The kind is decided BEFORE the size check, so a 30 MiB PNG is still
// refused at the image cap rather than sliding under the file one.
func (s *Store) Upload(threadID, filename, mimeType, dataB64 string, createdAt int64) (store.Attachment, error) {
	if strings.TrimSpace(threadID) == "" {
		return store.Attachment{}, errors.New("attachment: thread id is required")
	}
	if strings.TrimSpace(filename) == "" {
		return store.Attachment{}, errors.New("attachment: filename is required")
	}

	upload, err := classifyUpload(mimeType, filename)
	if err != nil {
		return store.Attachment{}, err
	}

	data, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return store.Attachment{}, fmt.Errorf("attachment: decode base64: %w", err)
	}
	if len(data) == 0 {
		return store.Attachment{}, errors.New("attachment: payload is empty")
	}
	if limit := s.limitFor(upload.kind); int64(len(data)) > limit {
		return store.Attachment{}, fmt.Errorf("attachment: payload %d bytes exceeds limit %d", len(data), limit)
	}
	if upload.kind == store.AttachmentKindImage {
		// An image says what it is, so it has to be it. A mismatch is
		// refused rather than demoted to a file: the caller asked for the
		// image path (inline bytes, a `[Image #N]` slot, a thumbnail) and
		// silently giving it something else would be the surprise.
		if err := validateImagePayload(upload.mime, data); err != nil {
			return store.Attachment{}, err
		}
	}

	id := uuid.NewString()
	relativePath := upload.relativePath(threadID, id)
	absolutePath, err := s.resolveWritePath(relativePath)
	if err != nil {
		return store.Attachment{}, err
	}
	tmpPath := absolutePath + ".tmp"

	// For a file this creates the attachment's own `<id>` directory; for an
	// image it is the thread directory, which usually already exists.
	if err := os.MkdirAll(filepath.Dir(absolutePath), privateDirPerm); err != nil {
		return store.Attachment{}, fmt.Errorf("attachment: mkdir: %w", err)
	}
	// Stage bytes to a tmp sibling so a crash between here and the final
	// rename leaves only a .tmp (no orphan row, no visible final file).
	if err := os.WriteFile(tmpPath, data, sensitiveFilePerm); err != nil {
		s.rollbackStagedWrite(upload.kind, tmpPath, absolutePath)
		return store.Attachment{}, fmt.Errorf("attachment: write tmp file: %w", err)
	}

	record := store.Attachment{
		ID:           id,
		ThreadID:     threadID,
		Filename:     filename,
		MimeType:     upload.mime,
		Size:         int64(len(data)),
		RelativePath: filepath.ToSlash(relativePath),
		CreatedAt:    createdAt,
		Kind:         upload.kind,
	}
	return record, s.commitStagedWrite(record, tmpPath, absolutePath)
}

// commitStagedWrite is the second half of every write path: INSERT the row,
// then publish the staged bytes with an atomic rename, rolling the row back
// if the rename fails. Shared by Upload and CopyToThread so the "DB row +
// final file both exist, or neither does" invariant has one implementation.
func (s *Store) commitStagedWrite(record store.Attachment, tmpPath, absolutePath string) error {
	if err := s.meta.InsertAttachment(record); err != nil {
		// DB row never landed; tear down the staged bytes so we don't leak.
		s.rollbackStagedWrite(record.Kind, tmpPath, absolutePath)
		return err
	}
	// Atomic rename publishes the file at its final path. If this fails
	// (e.g. FS error between directories), roll back the DB row so we
	// don't leave a metadata row pointing at a path that doesn't exist.
	if err := os.Rename(tmpPath, absolutePath); err != nil {
		s.rollbackStagedWrite(record.Kind, tmpPath, absolutePath)
		if derr := s.meta.DeleteAttachment(record.ID); derr != nil {
			return fmt.Errorf("attachment: rename %s → %s: %w (rollback also failed: %v)",
				tmpPath, absolutePath, err, derr)
		}
		return fmt.Errorf("attachment: rename %s → %s: %w", tmpPath, absolutePath, err)
	}
	return nil
}

// rollbackStagedWrite removes what a failed write left behind. A `file`
// owns its whole `<id>` directory, so that is what comes off — otherwise a
// failed upload would leave an empty directory per attempt under the
// thread. An image only ever staged a tmp sibling in the shared thread
// directory, so only that file is removed.
func (s *Store) rollbackStagedWrite(kind, tmpPath, absolutePath string) {
	_ = os.Remove(tmpPath)
	if kind == store.AttachmentKindFile {
		_ = os.RemoveAll(filepath.Dir(absolutePath))
	}
}

// limitFor is the per-kind payload cap. Images are re-encoded into a
// model's context; a file is only ever referenced by path.
func (s *Store) limitFor(kind string) int64 {
	if kind == store.AttachmentKindFile {
		return s.maxFileSize
	}
	return s.maxSize
}

// resolveWritePath joins a freshly built relative path onto the root and
// re-verifies containment. Defence in depth: the thread id and the filename
// are both sanitized before the join, so this can only fire if one of those
// sanitizers regressed — which is exactly when it is worth having.
func (s *Store) resolveWritePath(relativePath string) (string, error) {
	absolutePath := filepath.Join(s.root, relativePath)
	absRoot, err := filepath.Abs(s.root)
	if err != nil {
		return "", fmt.Errorf("attachment: absolute root: %w", err)
	}
	absFile, err := filepath.Abs(absolutePath)
	if err != nil {
		return "", fmt.Errorf("attachment: absolute file: %w", err)
	}
	if !strings.HasPrefix(absFile, absRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("attachment: refusing to write outside %s", absRoot)
	}
	return absolutePath, nil
}

// CopyToThread clones an existing attachment of either kind onto another
// thread by copying the file on disk, under a fresh id and the same
// tmp-then-rename + INSERT invariant every other write keeps.
//
// It exists because the cross-thread draft clone used to round-trip the
// bytes through base64 and re-run Upload: that re-validated a payload the
// store had already accepted, held the whole file in memory twice, and —
// once files existed — would have re-derived a kind rather than preserving
// the one already decided. The source thread is a parameter so the clone
// keeps the ownership boundary ReadThreadBytes / PathForThread enforce; a
// stale cross-thread id must not be able to pull another thread's file in.
func (s *Store) CopyToThread(sourceThreadID, targetThreadID, attachmentID string, createdAt int64) (store.Attachment, error) {
	if strings.TrimSpace(targetThreadID) == "" {
		return store.Attachment{}, errors.New("attachment: thread id is required")
	}
	source, sourcePath, err := s.resolveThreadAttachment(sourceThreadID, attachmentID)
	if err != nil {
		return store.Attachment{}, err
	}

	id := uuid.NewString()
	// The on-disk NAME is reused verbatim from the source row: it is
	// already sanitized (a file) or already the canonical extension (an
	// image), so re-deriving it here would be a second answer to a question
	// the original write settled.
	sourceName := filepath.Base(filepath.FromSlash(source.RelativePath))
	relativePath := filepath.Join(sanitizeThreadID(targetThreadID), id+filepath.Ext(sourceName))
	if source.Kind == store.AttachmentKindFile {
		relativePath = filepath.Join(sanitizeThreadID(targetThreadID), id, sourceName)
	}
	absolutePath, err := s.resolveWritePath(relativePath)
	if err != nil {
		return store.Attachment{}, err
	}
	tmpPath := absolutePath + ".tmp"

	if err := os.MkdirAll(filepath.Dir(absolutePath), privateDirPerm); err != nil {
		return store.Attachment{}, fmt.Errorf("attachment: mkdir: %w", err)
	}
	size, err := copyFileTo(sourcePath, tmpPath)
	if err != nil {
		s.rollbackStagedWrite(source.Kind, tmpPath, absolutePath)
		return store.Attachment{}, err
	}

	record := store.Attachment{
		ID:           id,
		ThreadID:     targetThreadID,
		Filename:     source.Filename,
		MimeType:     source.MimeType,
		Size:         size,
		RelativePath: filepath.ToSlash(relativePath),
		CreatedAt:    createdAt,
		Kind:         source.Kind,
	}
	return record, s.commitStagedWrite(record, tmpPath, absolutePath)
}

// copyFileTo streams one attachment's bytes into a staged destination,
// returning what was actually written. Streamed rather than read whole so a
// 50 MiB file clone costs a buffer instead of two copies of the payload.
func copyFileTo(sourcePath, tmpPath string) (int64, error) {
	src, err := os.Open(sourcePath)
	if err != nil {
		return 0, fmt.Errorf("attachment: open source: %w", err)
	}
	defer src.Close()
	dst, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, sensitiveFilePerm)
	if err != nil {
		return 0, fmt.Errorf("attachment: create tmp file: %w", err)
	}
	size, err := io.Copy(dst, src)
	if closeErr := dst.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, fmt.Errorf("attachment: copy bytes: %w", err)
	}
	return size, nil
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
// expected thread AND is an image. Ownership is checked from metadata before
// reading the file so stale cross-thread IDs cannot force unnecessary large
// reads.
//
// The kind check is the one that used to be the MIME allowlist: the
// attachment root now holds arbitrary bytes, so "safe to hand back to a
// client" is a property of the ROW, not of the directory. Every caller that
// serves or inlines bytes goes through here, so the guarantee holds without
// each of them remembering it. A `file` is reached by PATH (PathForThread)
// and copied by CopyToThread; nothing needs its bytes in process.
func (s *Store) ReadThreadBytes(threadID, attachmentID string) (store.Attachment, []byte, error) {
	record, absolutePath, err := s.resolveThreadAttachment(threadID, attachmentID)
	if err != nil {
		return store.Attachment{}, nil, err
	}
	if record.Kind != store.AttachmentKindImage {
		return store.Attachment{}, nil, fmt.Errorf("%w: %q is a %s attachment", ErrNotAnImage, attachmentID, record.Kind)
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

// Delete removes the row and the backing file — for a `file`, its whole
// `<id>` directory, which is what that attachment owns on disk. Missing
// files are treated as success because the thing we were asked to delete is
// already gone.
//
// Ownership is resolved the same way the read accessors resolve it, so a
// stale or foreign id destroys nothing: deleting is at least as privileged
// as reading, and this package has exactly one answer to "does this
// attachment belong to this thread".
func (s *Store) Delete(threadID, attachmentID string) error {
	record, absolutePath, err := s.resolveThreadAttachment(threadID, attachmentID)
	if err != nil {
		return err
	}
	if err := s.meta.DeleteAttachment(attachmentID); err != nil {
		return err
	}
	// A file's parent directory is `<thread>/<id>` by construction. The
	// base check is the tripwire: a row whose path did not come from this
	// package's own layout falls back to removing just the file rather
	// than taking a directory it does not own (the thread's, at worst).
	if record.Kind == store.AttachmentKindFile {
		if dir := filepath.Dir(absolutePath); filepath.Base(dir) == record.ID {
			if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("attachment: remove file dir: %w", err)
			}
			return nil
		}
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

// uploadType is what classifyUpload decided about an upload before a byte
// of the payload has been looked at: which kind it is, the MIME the row
// will carry, and how it will be named on disk.
type uploadType struct {
	kind string
	mime string
	// name is the file's sanitized on-disk name (`file` kind) or the
	// canonical extension the id is suffixed with (`image` kind).
	name string
}

// relativePath is the on-disk layout, and it differs by kind on purpose.
// An image is content the UI renders and never names, so `<id><ext>` is
// enough. A file is referenced BY PATH in the prompt, so it gets its own
// directory and keeps its real filename — the agent reads a path that says
// what the thing is, and a `cp` out of it keeps the name.
func (u uploadType) relativePath(threadID, id string) string {
	if u.kind == store.AttachmentKindFile {
		return filepath.Join(sanitizeThreadID(threadID), id, u.name)
	}
	return filepath.Join(sanitizeThreadID(threadID), id+u.name)
}

// classifyUpload decides the kind from the declared MIME and the filename,
// without looking at the payload.
//
// A declared image MIME, or an image extension, means IMAGE — and an image
// then has to prove itself against its signature or the upload is refused.
// That asymmetry is the whole rule: an image can only ever be something the
// providers ingest as an image, so nothing is reclassified INTO the image
// kind, and image formats no provider ingests (heic, bmp, svg) are files.
//
// Everything else is a file at face value: the declared MIME is kept
// (bounded, `application/octet-stream` when absent) and no signature is
// checked, because there is no signature that would mean anything for an
// arbitrary file.
func classifyUpload(mimeType, filename string) (uploadType, error) {
	mime := strings.ToLower(strings.TrimSpace(mimeType))
	if ext, ok := allowedMIMEs[mime]; ok {
		return uploadType{kind: store.AttachmentKindImage, mime: mime, name: ext}, nil
	}
	if ext := strings.ToLower(filepath.Ext(filename)); ext != "" {
		if mimeFromExt, ok := allowedExtensions[ext]; ok {
			return uploadType{kind: store.AttachmentKindImage, mime: mimeFromExt, name: ext}, nil
		}
	}
	if mime == "" {
		mime = fallbackFileMIME
	}
	if len(mime) > maxDeclaredMIMEBytes {
		return uploadType{}, fmt.Errorf("attachment: declared mime type is %d bytes, max %d", len(mime), maxDeclaredMIMEBytes)
	}
	return uploadType{kind: store.AttachmentKindFile, mime: mime, name: sanitizeFilename(filename)}, nil
}

// sanitizeFilename turns a caller-supplied filename into one path segment
// that is safe to join under the attachment root on every platform we ship.
//
// It is a FILTER, not an allowlist, because the point of the file layout is
// that the agent-facing path carries the user's real filename. What it
// removes is what could make the name mean something other than "one leaf
// under this directory":
//
//   - path separators (both spellings) and `:`, which on Windows opens an
//     alternate data stream or a drive-relative path;
//   - control bytes and invalid UTF-8, which no filesystem should be asked
//     to hold and which mangle every log line the path appears in;
//   - leading dots, so `..` cannot survive as a name and no attachment
//     lands hidden;
//   - trailing dots and spaces, which Windows silently strips — a name that
//     changes on the way to disk is a path the metadata row is wrong about.
//
// The result is capped in BYTES on a rune boundary (filesystem limits are
// byte limits) and falls back to "file" when nothing survives. The join is
// re-verified afterwards by resolveWritePath regardless.
func sanitizeFilename(filename string) string {
	var b strings.Builder
	b.Grow(len(filename))
	for _, r := range strings.TrimSpace(filename) {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == os.PathSeparator:
			continue
		case r < 0x20 || r == 0x7f:
			continue
		case r == utf8.RuneError:
			continue
		}
		if b.Len()+utf8.RuneLen(r) > maxFilenameBytes {
			break
		}
		b.WriteRune(r)
	}
	clean := strings.TrimRight(strings.TrimLeft(b.String(), "."), ". ")
	if clean == "" {
		return "file"
	}
	return clean
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
