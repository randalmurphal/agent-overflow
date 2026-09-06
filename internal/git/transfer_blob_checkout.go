package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/transferfiles"
)

// checkoutTransferBlobs uses raw blobs, not checkout filters. A destination
// smudge/process filter can execute programs, fetch unavailable LFS content, or
// expand beyond the validated size. Source working conversions travel as deltas.
func (c *Core) checkoutTransferBlobs(ctx context.Context, cwd string, root *os.Root, entries []TransferIndexEntry, sizes []int64) error {
	if len(entries) != len(sizes) {
		return errors.New("transfer: missing blob sizes")
	}
	if len(entries) == 0 {
		return nil
	}
	var input strings.Builder
	limit := int64(len(entries)) * 256
	var total int64
	for i, entry := range entries {
		if sizes[i] < 0 || sizes[i] > transferfiles.MaxFileBytes || (entry.Mode == "120000" && sizes[i] > 4096) {
			return errors.New("transfer: invalid materialized blob size")
		}
		total += sizes[i]
		if total > transferfiles.MaxTotalBytes {
			return errors.New("transfer: materialized blobs exceed the size limit")
		}
		input.WriteString(entry.OID)
		input.WriteByte('\n')
		limit += sizes[i]
	}
	output := &transferBlobWriter{root: root, entries: entries, sizes: sizes}
	defer output.close()
	_, _, err := c.executeSpec(commandSpec{binary: "git", cwd: cwd, ctx: ctx, stdin: input.String(), args: []string{"cat-file", "--batch"},
		extraEnv: []string{"GIT_NO_LAZY_FETCH=1"}, output: output, outputLimit: limit, timeout: 30 * time.Minute})
	if err != nil {
		return err
	}
	if output.next != len(entries) || output.separator || len(output.header) != 0 || output.file != nil {
		return errors.New("transfer: Git blob output was truncated")
	}
	return nil
}

// A streaming batch decoder holds only one header, file descriptor or small
// link target. os/exec joins its writer goroutine before the caller reads state.
type transferBlobWriter struct {
	root      *os.Root
	entries   []TransferIndexEntry
	sizes     []int64
	next      int
	header    []byte
	file      *os.File
	link      []byte
	remaining int64
	content   bool
	separator bool
}

func (w *transferBlobWriter) close() {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
}

func (w *transferBlobWriter) Write(data []byte) (int, error) {
	consumed := 0
	for len(data) > 0 {
		if w.separator {
			if data[0] != '\n' {
				return consumed, errors.New("transfer: invalid Git blob separator")
			}
			w.separator = false
			w.next++
			consumed++
			data = data[1:]
			continue
		}
		if w.next >= len(w.entries) {
			return consumed, errors.New("transfer: unexpected Git blob output")
		}
		if !w.content {
			end := bytes.IndexByte(data, '\n')
			if end < 0 {
				end = len(data)
			}
			if len(w.header)+end > 128 {
				return consumed, errors.New("transfer: oversized Git blob header")
			}
			w.header = append(w.header, data[:end]...)
			data = data[end:]
			consumed += end
			if len(data) == 0 {
				continue
			}
			data = data[1:]
			consumed++
			fields := strings.Fields(string(w.header))
			w.header = w.header[:0]
			if len(fields) != 3 || fields[0] != w.entries[w.next].OID || fields[1] != "blob" {
				return consumed, errors.New("transfer: unexpected Git blob identity")
			}
			size, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil || size != w.sizes[w.next] {
				return consumed, errors.New("transfer: Git blob changed after size validation")
			}
			w.content, w.remaining = true, size
			entry := w.entries[w.next]
			if err := ensureTransferParents(w.root, entry.Path); err != nil {
				return consumed, err
			}
			if entry.Mode == "120000" {
				w.link = make([]byte, 0, int(size))
			} else {
				mode := fs.FileMode(0o600)
				if entry.Mode == "100755" {
					mode = 0o700
				}
				w.file, err = w.root.OpenFile(filepath.FromSlash(entry.Path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
				if err != nil {
					return consumed, err
				}
			}
		}
		count := min(int64(len(data)), w.remaining)
		if w.file != nil {
			n, err := w.file.Write(data[:count])
			if err != nil {
				return consumed + n, err
			}
			if int64(n) != count {
				return consumed + n, io.ErrShortWrite
			}
		} else {
			w.link = append(w.link, data[:count]...)
		}
		consumed += int(count)
		data = data[count:]
		w.remaining -= count
		if w.remaining != 0 {
			continue
		}
		if w.file != nil {
			if err := w.file.Close(); err != nil {
				return consumed, err
			}
			w.file = nil
		} else {
			if len(w.link) == 0 || bytes.IndexByte(w.link, 0) >= 0 {
				return consumed, errors.New("transfer: invalid Git symbolic link")
			}
			if err := w.root.Symlink(string(w.link), filepath.FromSlash(w.entries[w.next].Path)); err != nil {
				return consumed, fmt.Errorf("transfer: create Git symbolic link: %w", err)
			}
			w.link = nil
		}
		w.content, w.separator = false, true
	}
	return consumed, nil
}
