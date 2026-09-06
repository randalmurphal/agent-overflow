package transferfiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"agent-overflow/internal/atomicfile"
)

const MaxUploadChunk = 8 << 20
const uploadStateName = "upload.json"
const uploadFileName = "archive.part"

// ErrUploadCorrupt means no byte checkpoint may be trusted. The coordinator
// may reset an UNPREPARED upload from its independently sealed SQL identity.
// Ordinary I/O/permission failures must not be mistaken for corrupt contents.
var ErrUploadCorrupt = errors.New("transfer: upload checkpoint or accepted bytes are corrupt")

// UploadProgress is a durable byte checkpoint, not an event log. The caller
// serializes operations on this private directory with its operation lock.
type UploadProgress struct {
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Received int64  `json:"received"`
}

func validDigest(digest string) bool {
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == digest
}

// BeginUpload is idempotent for the same content. Checkpoint zero is durable
// before the first bytes arrive, so a crash never invents an accepted prefix.
func BeginUpload(directory, digest string, size int64) (UploadProgress, error) {
	if directory == "" || !validDigest(digest) || size < 1024 || size > MaxArchiveBytes || size%512 != 0 {
		return UploadProgress{}, errors.New("transfer: invalid upload identity or size")
	}
	if progress, found, err := readUpload(directory); err != nil {
		return UploadProgress{}, err
	} else if found {
		if progress.SHA256 != digest || progress.Size != size {
			return UploadProgress{}, errors.New("transfer: operation already holds different archive content")
		}
		return progress, nil
	}
	progress := UploadProgress{SHA256: digest, Size: size}
	if err := atomicfile.WriteJSON(filepath.Join(directory, uploadStateName), progress); err != nil {
		return UploadProgress{}, err
	}
	return progress, nil
}

func readUpload(directory string) (UploadProgress, bool, error) {
	var progress UploadProgress
	if directory == "" {
		return progress, false, errors.New("transfer: missing upload directory")
	}
	file, err := os.Open(filepath.Join(directory, uploadStateName))
	if errors.Is(err, os.ErrNotExist) {
		return progress, false, nil
	}
	if err != nil {
		return progress, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return progress, true, err
	}
	if len(data) > 4096 {
		return progress, true, ErrUploadCorrupt
	}
	if err := json.Unmarshal(data, &progress); err != nil {
		return progress, true, ErrUploadCorrupt
	}
	if !validDigest(progress.SHA256) || progress.Size < 1024 || progress.Size > MaxArchiveBytes || progress.Size%512 != 0 || progress.Received < 0 || progress.Received > progress.Size {
		return UploadProgress{}, true, ErrUploadCorrupt
	}
	if progress.Received > 0 {
		info, err := os.Lstat(filepath.Join(directory, uploadFileName))
		if errors.Is(err, os.ErrNotExist) {
			return progress, true, ErrUploadCorrupt
		}
		if err != nil {
			return progress, true, err
		}
		if !info.Mode().IsRegular() || info.Size() < progress.Received {
			return progress, true, ErrUploadCorrupt
		}
	}
	return progress, true, nil
}

// ResetUpload discards the byte checkpoint, not the immutable archive identity.
// Only the coordinator can prove the operation has never acknowledged prepared.
// Write checkpoint zero first: a crash must never claim truncated bytes exist.
func ResetUpload(directory, digest string, size int64) (UploadProgress, error) {
	if directory == "" || !validDigest(digest) || size < 1024 || size > MaxArchiveBytes || size%512 != 0 {
		return UploadProgress{}, errors.New("transfer: invalid reset identity")
	}
	progress := UploadProgress{SHA256: digest, Size: size}
	if err := atomicfile.WriteJSON(filepath.Join(directory, uploadStateName), progress); err != nil {
		return progress, err
	}
	// ReceiveChunk truncates the unacknowledged tail before the next append.
	return progress, nil
}

func ReadUpload(directory string) (UploadProgress, error) {
	progress, found, err := readUpload(directory)
	if err != nil {
		return progress, err
	}
	if !found {
		return progress, os.ErrNotExist
	}
	return progress, nil
}

// ReceiveChunk verifies a bounded chunk before appending it. Bytes are fsynced
// before advancing the checkpoint. A retry compares an already accepted range
// rather than appending it twice. An uncommitted crash tail is truncated back to
// the checkpoint before the next append, never reported as progress.
func ReceiveChunk(ctx context.Context, directory string, offset, size int64, digest string, input io.Reader) (UploadProgress, error) {
	progress, err := ReadUpload(directory)
	if err != nil {
		return progress, err
	}
	if !validDigest(digest) || offset < 0 || size <= 0 || size > MaxUploadChunk || offset > progress.Received || size > progress.Size-offset {
		return progress, errors.New("transfer: invalid chunk or upload offset")
	}
	if offset < progress.Received && size > progress.Received-offset {
		return progress, errors.New("transfer: chunk overlaps the upload checkpoint")
	}
	// One serialized upload has one scratch chunk. Reuse its private name so
	// repeated process deaths cannot accumulate a new file on every attempt.
	chunkPath := filepath.Join(directory, ".chunk.part")
	if err := os.Remove(chunkPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return progress, err
	}
	tmp, err := os.OpenFile(chunkPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return progress, err
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	hash := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(tmp, hash), &contextReader{ctx, input}, size); err != nil {
		return progress, err
	}
	var extra [1]byte
	n, readErr := (&contextReader{ctx, input}).Read(extra[:])
	if readErr != nil && readErr != io.EOF {
		return progress, readErr
	}
	if n != 0 || readErr != io.EOF {
		return progress, errors.New("transfer: chunk length does not match its declaration")
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return progress, errors.New("transfer: chunk checksum mismatch")
	}
	archive, err := os.OpenFile(filepath.Join(directory, uploadFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return progress, err
	}
	defer archive.Close()
	info, err := archive.Stat()
	if err != nil {
		return progress, err
	}
	if !info.Mode().IsRegular() || info.Size() < progress.Received {
		return progress, errors.New("transfer: accepted upload bytes are missing")
	}
	if offset < progress.Received {
		hash.Reset()
		if _, err := io.Copy(hash, io.NewSectionReader(archive, offset, size)); err != nil {
			return progress, err
		}
		if hex.EncodeToString(hash.Sum(nil)) != digest {
			return progress, errors.New("transfer: previously accepted chunk has different content")
		}
		return progress, nil
	}
	if err := archive.Truncate(progress.Received); err != nil {
		return progress, err
	}
	if _, err := archive.Seek(progress.Received, io.SeekStart); err != nil {
		return progress, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return progress, err
	}
	if _, err := io.Copy(archive, &contextReader{ctx, tmp}); err != nil {
		return progress, err
	}
	if err := archive.Sync(); err != nil {
		return progress, err
	}
	next := progress
	next.Received += size
	if err := atomicfile.WriteJSON(filepath.Join(directory, uploadStateName), next); err != nil {
		return progress, err
	}
	return next, nil
}

// VerifyUploadContent distinguishes damaged disk bytes from an invalid archive
// that was faithfully uploaded. Call after extraction fails, while still holding
// the operation lock. A healthy transfer pays no second complete file read.
func VerifyUploadContent(ctx context.Context, file *os.File, digest string, size int64) error {
	if !validDigest(digest) || size < 1024 || size > MaxArchiveBytes || size%512 != 0 {
		return errors.New("transfer: invalid verification identity")
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return ErrUploadCorrupt
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, &contextReader{ctx, io.LimitReader(file, size+1)})
	if err != nil {
		return err
	}
	if n != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return ErrUploadCorrupt
	}
	return nil
}

// UploadedArchive is available only after every byte is acknowledged. Extract
// still verifies its complete digest and member structure before preparation.
func UploadedArchive(directory string) (string, UploadProgress, error) {
	progress, err := ReadUpload(directory)
	if err != nil {
		return "", progress, err
	}
	if progress.Received != progress.Size {
		return "", progress, fmt.Errorf("transfer: upload incomplete (%d of %d bytes)", progress.Received, progress.Size)
	}
	return filepath.Join(directory, uploadFileName), progress, nil
}
