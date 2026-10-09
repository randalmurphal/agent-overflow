package workspacefiles

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
)

// Sentinels a caller matches with errors.Is to name a refusal to a person;
// the os errors (fs.ErrNotExist, fs.ErrPermission) pass through unwrapped.
var (
	ErrNotRegularFile = errors.New("not a regular file")
	ErrTooLarge       = errors.New("file is too large")
)

// OpenRegular opens a workspace file that a coding agent may be mutating
// concurrently. Every check runs on the open descriptor: O_NONBLOCK keeps the
// open itself from hanging on a FIFO, and the fstat classifies what was
// actually opened, where a stat before the open could pass a path that is
// swapped before the read. The caller closes the file.
func OpenRegular(path string) (*os.File, fs.FileInfo, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, nil, closeAfter(file, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, closeAfter(file, ErrNotRegularFile)
	}
	return file, info, nil
}

// ReadOpened reads a file OpenRegular returned, capped at maxBytes. The cap
// is checked against the fstat size first and then enforced on the read, so
// a file that grows after the fstat still cannot allocate past it. maxBytes
// <= 0 means unbounded.
func ReadOpened(file *os.File, info fs.FileInfo, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return io.ReadAll(file)
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("%w: exceeds %d bytes", ErrTooLarge, maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: exceeds %d bytes", ErrTooLarge, maxBytes)
	}
	return data, nil
}

// ReadRegular is OpenRegular and ReadOpened in one call.
func ReadRegular(path string, maxBytes int64) ([]byte, error) {
	file, info, err := OpenRegular(path)
	if err != nil {
		return nil, err
	}
	data, err := ReadOpened(file, info, maxBytes)
	if err != nil {
		return nil, closeAfter(file, err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close %s: %w", path, err)
	}
	return data, nil
}

// closeAfter releases a descriptor on a path that already failed, reporting
// a close failure beside the original error rather than dropping it.
func closeAfter(file *os.File, cause error) error {
	if err := file.Close(); err != nil {
		return errors.Join(cause, fmt.Errorf("close: %w", err))
	}
	return cause
}
