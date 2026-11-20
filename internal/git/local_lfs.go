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
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/attachment"
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
// read. Whether the object is present is not content addressed (a
// `git lfs pull` or prune changes it), so every answer read through a
// pointer is held for at most changingFactTTL, in the blob image cache
// under its workspace (another repository's store may differ).

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

// objects answers the store's directory: the common dir comes from
// CommonDir (memoized per workspace) and lfs.storage from one `git
// config`.
func (s *lfsStore) objects() (string, error) {
	s.once.Do(func() { s.dir, s.err = s.core.lfsStorageDir(s.ctx, s.cwd) })
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

// lfsBlobImage answers pointer blob blob, whose first bytes are pointer,
// as the image its local LFS object is: found with the blob's id and the
// object's size and header, or blob itself (LFS) when the pointer does not
// parse or the object is not present. Either is held until now plus
// changingFactTTL.
func (s *lfsStore) lfsBlobImage(blob BlobImage, pointer []byte, now time.Time) (BlobImage, error) {
	blob.expires = now.Add(changingFactTTL)
	parsed, ok := gitdiff.ParseLFSPointer(pointer)
	if !ok || parsed.Size > attachment.DisplayImageMaxBytes {
		return blob, nil
	}
	dir, err := s.objects()
	if err != nil {
		return BlobImage{}, err
	}
	path := filepath.Join(dir, "objects", parsed.OID[0:2], parsed.OID[2:4], parsed.OID)
	file, err := openLFSObject(path, parsed.Size)
	switch {
	case errors.Is(err, errNoLFSObject):
		return blob, nil
	case err != nil:
		return BlobImage{}, err
	}
	defer file.Close()
	image, _, err := classifyImageReader(&countingReader{r: file}, parsed.Size)
	if err != nil {
		return BlobImage{}, err
	}
	answer := BlobImage{OID: blob.OID, Size: parsed.Size, expires: blob.expires}
	// An object that is itself a pointer is no image.
	if image.Displayable() {
		answer.MIME, answer.Width, answer.Height = image.MIME, image.Width, image.Height
		answer.lfsPath, answer.lfsOID = path, parsed.OID
	}
	return answer, nil
}

// openLFSObject opens the regular file at path when it is size bytes, or
// answers errNoLFSObject. The file opened is the one inspected: a symlink,
// or a file swapped in after the inspection, is not opened.
func openLFSObject(path string, size int64) (*os.File, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, errNoLFSObject
	case err != nil:
		return nil, err
	case !info.Mode().IsRegular() || info.Size() != size:
		return nil, errNoLFSObject
	}
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

// readLFSObject fills buf with image's LFS object, hashing it as it is
// read: exactly len(buf) bytes that hash to the pointer's sha256, or an
// error that names none of its content.
func readLFSObject(ctx context.Context, image BlobImage, buf []byte) error {
	file, err := openLFSObject(image.lfsPath, int64(len(buf)))
	if errors.Is(err, errNoLFSObject) {
		return fmt.Errorf("%w: %w", ErrNoLocalBlob, err)
	}
	if err != nil {
		return err
	}
	defer file.Close()
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
