package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/gitdiff"
)

// A Git LFS pointer blob whose object is in the repository's local LFS
// store is shown as that object, with no network and no git-lfs: the
// store is lfs.storage from git config (a relative value under the git
// common dir) or else <common dir>/lfs, and the object is
// objects/<oid[0:2]>/<oid[2:4]>/<oid> there. It is present when it is a
// regular file, not a symlink, of the pointer's size within
// attachment.DisplayImageMaxBytes. Its header is classified as any blob's,
// and its bytes are verified against the pointer's sha256 as they are
// read.
//
// What a pointer blob is (its parsed pointer) is content addressed and
// held by blob id without expiry. Whether its object is present is not (a
// `git lfs pull` or prune changes it), so every lookup decides it with one
// lstat of the object's path. A present object's classification is held
// by its sha256, size and file time without expiry, so a repeat lookup reads none of
// its bytes. The store's directory is held per workspace for
// changingFactTTL. All three live in the blob image cache, under keys no
// blob id can spell.

// lfsReadChunk is how much of an LFS object is read between checks of the
// caller's context.
const lfsReadChunk = 1 << 20

var (
	// ErrLFSObjectMismatch is an LFS object whose bytes do not hash to its
	// pointer's sha256.
	ErrLFSObjectMismatch = errors.New("the Git LFS object does not match its pointer")
	// errNoLFSObject is an object that is not present in the local store.
	errNoLFSObject = errors.New("the Git LFS object is not in the local store")
)

// lfsStoreKey is the blob image cache key holding cwd's LFS store
// directory (in lfsPath), and lfsObjectKey the one holding an LFS
// object's classification.
func lfsStoreKey(cwd string) string { return "lfs-store\x00" + cwd }

// The header is classified before any read verifies the object's hash, so
// the key names the file inspected too (its modification time): a file
// that replaces a corrupt object is classified again.
func lfsObjectKey(pointer gitdiff.LFSPointer, info fs.FileInfo) string {
	return "lfs-object\x00" + pointer.OID + "\x00" + strconv.FormatInt(pointer.Size, 10) + "\x00" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
}

// lfsStore is one call's view of a repository's local LFS store: its
// directory is resolved on first use and shared by the call's reads.
type lfsStore struct {
	core *Core
	ctx  context.Context
	cwd  string
	once sync.Once
	dir  string
	err  error
}

func (c *Core) lfsStore(ctx context.Context, cwd string) *lfsStore {
	return &lfsStore{core: c, ctx: ctx, cwd: cwd}
}

// objects answers the store's directory: held for the workspace, else
// read (CommonDir, memoized per workspace, and one `git config`) and held
// for changingFactTTL. A failure is not held.
func (s *lfsStore) objects() (string, error) {
	s.once.Do(func() {
		key := lfsStoreKey(s.cwd)
		if held, ok := s.core.blobImages.get(key, s.core.nowFn()); ok {
			s.dir = held.lfsPath
			return
		}
		s.dir, s.err = s.core.lfsStorageDir(s.ctx, s.cwd)
		if s.err == nil {
			s.core.blobImages.put(key, BlobImage{lfsPath: s.dir}, s.core.nowFn().Add(changingFactTTL))
		}
	})
	return s.dir, s.err
}

func (c *Core) lfsStorageDir(ctx context.Context, cwd string) (string, error) {
	common, err := c.CommonDir(cwd)
	if err != nil {
		return "", err
	}
	result, err := c.factGit(ctx, cwd, commandSpec{maxBytes: 64 << 10, args: []string{"config", "-z", "--get", "lfs.storage"}})
	storage := strings.TrimSuffix(result.stdout, "\x00")
	switch {
	case err != nil && result.exitCode == 1 && result.stdout == "":
		// Unset.
		storage = ""
	case err != nil:
		return "", fmt.Errorf("read lfs.storage: %w", err)
	}
	switch {
	case storage == "":
		return filepath.Join(common, "lfs"), nil
	case filepath.IsAbs(storage):
		return filepath.Clean(storage), nil
	}
	return filepath.Join(common, storage), nil
}

// lfsBlobImage answers record, a pointer blob's held record (its pointer
// set), as the image its local LFS object is now: found with the blob's id
// and the object's size and header, or the blob as LFS when the object is
// not present. It costs one lstat, plus a header read of an object whose
// classification is not held.
func (s *lfsStore) lfsBlobImage(record BlobImage) (BlobImage, error) {
	notLocal := BlobImage{OID: record.OID, Size: record.Size, LFS: true}
	dir, err := s.objects()
	if err != nil {
		return BlobImage{}, err
	}
	oid, size := record.pointer.OID, record.pointer.Size
	path := filepath.Join(dir, "objects", oid[0:2], oid[2:4], oid)
	info, err := statLFSObject(path, size)
	switch {
	case errors.Is(err, errNoLFSObject):
		return notLocal, nil
	case err != nil:
		return BlobImage{}, err
	}
	key := lfsObjectKey(record.pointer, info)
	object, ok := s.core.blobImages.get(key, s.core.nowFn())
	if !ok {
		object, err = s.core.classifyLFSObject(path, info, size)
		switch {
		case errors.Is(err, errNoLFSObject):
			return notLocal, nil
		case err != nil:
			return BlobImage{}, err
		}
		s.core.blobImages.put(key, object, time.Time{})
	}
	answer := BlobImage{OID: record.OID, Size: size}
	// An object that is itself a pointer is no image.
	if object.Displayable() {
		answer.MIME, answer.Width, answer.Height = object.MIME, object.Width, object.Height
		answer.lfsPath, answer.lfsOID = path, oid
	}
	return answer, nil
}

// classifyLFSObject reads the header of the object at path, which info
// inspected, as an image: its type and pixel size, nothing else set.
func (c *Core) classifyLFSObject(path string, info fs.FileInfo, size int64) (object BlobImage, err error) {
	if hook := c.beforeLFSObjectRead; hook != nil {
		hook(path)
	}
	file, err := openLFSObjectAt(path, info, size)
	if err != nil {
		return BlobImage{}, err
	}
	defer func() {
		if closeErr := c.closeLFSObject(file); closeErr != nil {
			object, err = BlobImage{}, errors.Join(err, closeErr)
		}
	}()
	image, _, err := classifyImageReader(&countingReader{r: file}, size)
	if err != nil {
		return BlobImage{}, err
	}
	return BlobImage{MIME: image.MIME, Width: image.Width, Height: image.Height}, nil
}

// statLFSObject answers the lstat of path when it is a regular file of
// size bytes, else errNoLFSObject.
func statLFSObject(path string, size int64) (fs.FileInfo, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, errNoLFSObject
	case err != nil:
		return nil, err
	case !info.Mode().IsRegular() || info.Size() != size:
		return nil, errNoLFSObject
	}
	return info, nil
}

// openLFSObject opens the regular file at path when it is size bytes, or
// answers errNoLFSObject.
func openLFSObject(path string, size int64) (*os.File, error) {
	info, err := statLFSObject(path, size)
	if err != nil {
		return nil, err
	}
	return openLFSObjectAt(path, info, size)
}

// openLFSObjectAt opens the file at path that info, statLFSObject's
// answer, inspected, or answers errNoLFSObject: a file swapped in after
// the inspection is not opened.
func openLFSObjectAt(path string, info fs.FileInfo, size int64) (*os.File, error) {
	file, err := os.Open(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, errNoLFSObject
	case err != nil:
		return nil, err
	}
	opened, err := file.Stat()
	if err == nil && (!os.SameFile(info, opened) || opened.Size() != size) {
		err = errNoLFSObject
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

// closeLFSObject closes file, an LFS object a read opened: nil, or an
// error that names none of its content.
func (c *Core) closeLFSObject(file *os.File) error {
	err := file.Close()
	if hook := c.lfsObjectClosed; hook != nil {
		err = hook(err)
	}
	if err != nil {
		return fmt.Errorf("close the Git LFS object: %w", err)
	}
	return nil
}

// readLFSObject fills buf with image's LFS object, hashing it as it is
// read: exactly len(buf) bytes that hash to the pointer's sha256, or an
// error that names none of its content.
func (c *Core) readLFSObject(ctx context.Context, image BlobImage, buf []byte) (err error) {
	file, err := openLFSObject(image.lfsPath, int64(len(buf)))
	if errors.Is(err, errNoLFSObject) {
		return fmt.Errorf("%w: %w", ErrNoLocalBlob, err)
	}
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := c.closeLFSObject(file); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	sum := sha256.New()
	for read := 0; read < len(buf); {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := buf[read:min(read+lfsReadChunk, len(buf))]
		n, err := io.ReadFull(file, chunk)
		sum.Write(chunk[:n])
		read += n
		if err != nil {
			return fmt.Errorf("read the Git LFS object: %w", err)
		}
	}
	if hex.EncodeToString(sum.Sum(nil)) != image.lfsOID {
		return ErrLFSObjectMismatch
	}
	return nil
}
