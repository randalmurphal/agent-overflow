package attachment

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/store"
	"github.com/google/uuid"
)

// CopyToThread clones an owned attachment under a new identity. Bytes and
// directory entries are flushed before returning so a durable draft move can
// safely remove the source.
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

	committed := false
	defer func() {
		if !committed {
			s.rollbackStagedWrite(source.Kind, tmpPath, absolutePath)
		}
	}()

	if err := os.MkdirAll(filepath.Dir(absolutePath), privateDirPerm); err != nil {
		return store.Attachment{}, fmt.Errorf("attachment: mkdir: %w", err)
	}
	size, err := copyFileTo(sourcePath, tmpPath)
	if err != nil {
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
	if err := s.commitStagedWrite(record, tmpPath, absolutePath); err != nil {
		return store.Attachment{}, err
	}
	if err := s.syncCopy(s.root, absolutePath); err != nil {
		return store.Attachment{}, errors.Join(fmt.Errorf("attachment: sync copied file: %w", err), s.Delete(targetThreadID, id))
	}
	committed = true
	return record, nil
}

// copyFileTo streams one attachment's bytes into a staged destination,
// returning what was actually written. Streamed rather than read whole so a
// 50 MiB file clone costs a buffer instead of two copies of the payload.
func copyFileTo(sourcePath, tmpPath string) (size int64, err error) {
	src, err := os.Open(sourcePath)
	if err != nil {
		return 0, fmt.Errorf("attachment: open source: %w", err)
	}
	defer func() { err = errors.Join(err, src.Close()) }()
	dst, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, sensitiveFilePerm)
	if err != nil {
		return 0, fmt.Errorf("attachment: create tmp file: %w", err)
	}
	size, err = io.Copy(dst, src)
	if closeErr := dst.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, fmt.Errorf("attachment: copy bytes: %w", err)
	}
	return size, nil
}

// A draft move may delete the source as soon as copying succeeds. Flush both
// the destination bytes and the directories that make those bytes reachable.
func syncCopiedAttachment(root, path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	stop := filepath.Dir(filepath.Clean(root))
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if err := atomicfile.SyncDir(dir); err != nil {
			return err
		}
		if dir == stop {
			return nil
		}
		if filepath.Dir(dir) == dir {
			return errors.New("attachment: copied file is outside its root")
		}
	}
}
