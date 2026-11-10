package attachment

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"

	"github.com/google/uuid"
)

// Shell output is a `file` attachment whose row carries store.AttachmentShell:
// the combined stdout and stderr of a command the person ran from the
// composer. It is the one file whose text is served back to a client
// (ReadShellOutput), because the person produced it on this machine and the
// timeline row shows it. The model reads it inline up to
// ShellInlineLimitBytes and by path beyond that (ShellPromptOutput).

// ShellOutputFilename is the on-disk name of every shell output file.
const ShellOutputFilename = "output.txt"

const shellOutputMIME = "text/plain; charset=utf-8"

const (
	// ShellInlineLimitBytes is the most output the model reads inline.
	ShellInlineLimitBytes = 30_000
	// shellExcerptBytes is each of the head and the tail the model reads
	// inline when the output is longer than that.
	shellExcerptBytes = 10_000
	// ShellReadMaxBytes bounds one ReadShellOutput chunk.
	ShellReadMaxBytes = 256 << 10
	// ShellPreviewBytes and ShellPreviewLines bound the output a timeline row
	// carries in its own metadata. Kept under the item wire's leaf floor so
	// the preview is never elided.
	ShellPreviewBytes = 640
	ShellPreviewLines = 12
)

// ErrNotShellOutput is what the shell accessors return for any other row.
var ErrNotShellOutput = errors.New("attachment: not shell output")

// SaveShellOutput adopts a finished command's capture as a `file` attachment
// owned by the thread. The capture is copied, not moved: it lives outside the
// attachment root and its owner removes it. An empty capture is valid.
func (s *Store) SaveShellOutput(threadID string, shell store.AttachmentShell, capturePath string, createdAt int64) (store.Attachment, error) {
	if strings.TrimSpace(threadID) == "" {
		return store.Attachment{}, errors.New("attachment: thread id is required")
	}
	if shell.IsZero() {
		return store.Attachment{}, errors.New("attachment: shell command is required")
	}
	info, err := os.Stat(capturePath)
	if err != nil {
		return store.Attachment{}, fmt.Errorf("attachment: stat shell output: %w", err)
	}
	if !info.Mode().IsRegular() {
		return store.Attachment{}, errors.New("attachment: shell output is not a regular file")
	}
	if limit := s.MaxSizeFor(store.AttachmentKindFile); info.Size() > limit {
		return store.Attachment{}, fmt.Errorf("attachment: shell output %d bytes exceeds limit %d", info.Size(), limit)
	}

	id := uuid.NewString()
	relativePath := filepath.Join(sanitizeThreadID(threadID), id, ShellOutputFilename)
	absolutePath, err := s.resolveWritePath(relativePath)
	if err != nil {
		return store.Attachment{}, err
	}
	tmpPath := absolutePath + ".tmp"
	committed := false
	defer func() {
		if !committed {
			s.rollbackStagedWrite(store.AttachmentKindFile, tmpPath, absolutePath)
		}
	}()
	if err := os.MkdirAll(filepath.Dir(absolutePath), privateDirPerm); err != nil {
		return store.Attachment{}, fmt.Errorf("attachment: mkdir: %w", err)
	}
	size, err := copyFileTo(capturePath, tmpPath)
	if err != nil {
		return store.Attachment{}, err
	}
	if shell.Preview, err = shellPreview(tmpPath); err != nil {
		return store.Attachment{}, err
	}
	record := store.Attachment{
		ID: id, ThreadID: threadID, Filename: ShellOutputFilename, MimeType: shellOutputMIME,
		Size: size, RelativePath: filepath.ToSlash(relativePath), CreatedAt: createdAt,
		Kind: store.AttachmentKindFile, Shell: shell,
	}
	if err := s.commitStagedWrite(record, tmpPath, absolutePath); err != nil {
		return store.Attachment{}, err
	}
	committed = true
	return record, nil
}

// shellPreview is the start of the output as the timeline shows it: whole
// lines up to ShellPreviewLines and ShellPreviewBytes, with terminal escape
// sequences kept and never cut.
func shellPreview(path string) (_ string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("attachment: open shell output: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	buffer := make([]byte, ShellPreviewBytes+1)
	n, err := io.ReadFull(file, buffer)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("attachment: read shell output: %w", err)
	}
	whole := n <= ShellPreviewBytes
	head := buffer[:min(n, ShellPreviewBytes)]
	lines, end := 0, len(head)
	for i, b := range head {
		if b != '\n' {
			continue
		}
		if lines++; lines == ShellPreviewLines {
			end, whole = i+1, whole && i+1 == len(head)
			break
		}
	}
	head = head[:end]
	if !whole && lines < ShellPreviewLines {
		// The byte bound cut the stream: end on the last whole line, or on a
		// whole rune outside any escape sequence when one line is that long.
		if cut := strings.LastIndexByte(string(head), '\n'); cut >= 0 {
			head = head[:cut+1]
		} else {
			head = head[:wholeRunePrefix(head)]
			if esc := strings.LastIndexByte(string(head), 0x1b); esc >= 0 {
				if next := stringsx.SkipANSIEscape(head, esc); next == esc+1 || next > len(head) {
					head = head[:esc]
				}
			}
		}
	}
	return strings.ToValidUTF8(string(head), "\uFFFD"), nil
}

// ShellOutputChunk is one bounded read of a shell output file.
type ShellOutputChunk struct {
	Data string
	// Offset is where Data starts and Size is the whole file, both in bytes.
	Offset int64
	Size   int64
	// Next is the offset of the following chunk; equal to Size at the end.
	Next int64
}

// ReadShellOutput returns up to limit bytes of a thread's shell output from
// offset, cut on a rune boundary. It refuses every row that is not shell
// output, so it cannot be used to read an uploaded file.
func (s *Store) ReadShellOutput(threadID, attachmentID string, offset int64, limit int) (_ store.Attachment, _ ShellOutputChunk, err error) {
	record, absolutePath, err := s.resolveThreadAttachment(threadID, attachmentID)
	if err != nil {
		return store.Attachment{}, ShellOutputChunk{}, err
	}
	if record.Shell.IsZero() {
		return store.Attachment{}, ShellOutputChunk{}, fmt.Errorf("%w: %q", ErrNotShellOutput, attachmentID)
	}
	if limit <= 0 || limit > ShellReadMaxBytes {
		limit = ShellReadMaxBytes
	}
	file, err := os.Open(absolutePath)
	if err != nil {
		return store.Attachment{}, ShellOutputChunk{}, fmt.Errorf("attachment: open shell output: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return store.Attachment{}, ShellOutputChunk{}, fmt.Errorf("attachment: stat shell output: %w", err)
	}
	size := info.Size()
	if offset < 0 || offset > size {
		return store.Attachment{}, ShellOutputChunk{}, fmt.Errorf("attachment: shell output offset %d is outside 0..%d", offset, size)
	}
	buffer := make([]byte, min(int64(limit), size-offset))
	n, err := file.ReadAt(buffer, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return store.Attachment{}, ShellOutputChunk{}, fmt.Errorf("attachment: read shell output: %w", err)
	}
	buffer = buffer[:n]
	if offset+int64(n) < size {
		buffer = buffer[:wholeRunePrefix(buffer)]
	}
	return record, ShellOutputChunk{
		Data: strings.ToValidUTF8(string(buffer), "�"), Offset: offset, Size: size,
		Next: offset + int64(len(buffer)),
	}, nil
}

// wholeRunePrefix is b without a trailing rune that has begun and not
// finished. A chunk that is nothing but such a rune is kept whole so a read
// always advances.
func wholeRunePrefix(b []byte) int {
	if n := stringsx.CompleteRunePrefix(b); n > 0 {
		return n
	}
	return len(b)
}

// ShellPromptOutput is the output the model reads inline for the shell output
// file at absolutePath (the store's own resolved path): all of it up to
// ShellInlineLimitBytes, otherwise its head and tail around a line that says
// how much was left out and where the whole file is. Terminal escape
// sequences are removed; the file keeps them for the timeline.
func ShellPromptOutput(absolutePath string) (_ string, err error) {
	file, err := os.Open(absolutePath)
	if err != nil {
		return "", fmt.Errorf("attachment: open shell output: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("attachment: stat shell output: %w", err)
	}
	size := info.Size()
	if size <= ShellInlineLimitBytes {
		data, err := io.ReadAll(file)
		if err != nil {
			return "", fmt.Errorf("attachment: read shell output: %w", err)
		}
		return cleanShellText(data), nil
	}
	head := make([]byte, shellExcerptBytes)
	if _, err := io.ReadFull(file, head); err != nil {
		return "", fmt.Errorf("attachment: read shell output: %w", err)
	}
	tail := make([]byte, shellExcerptBytes)
	if _, err := file.ReadAt(tail, size-shellExcerptBytes); err != nil {
		return "", fmt.Errorf("attachment: read shell output: %w", err)
	}
	// End the head and start the tail on line boundaries when a line break is
	// near, otherwise on rune boundaries.
	if cut := strings.LastIndexByte(string(head), '\n'); cut > shellExcerptBytes/2 {
		head = head[:cut+1]
	} else {
		head = head[:wholeRunePrefix(head)]
	}
	if cut := strings.IndexByte(string(tail), '\n'); cut >= 0 && cut < shellExcerptBytes/2 {
		tail = tail[cut+1:]
	} else {
		for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
			tail = tail[1:]
		}
	}
	omitted := size - int64(len(head)) - int64(len(tail))
	return cleanShellText(head) +
		fmt.Sprintf("\n[... %s of output omitted. The full output is saved at: %s ...]\n", FormatSize(omitted), absolutePath) +
		cleanShellText(tail), nil
}

// cleanShellText is output as prose for a model: valid UTF-8, no terminal
// escape sequences, and line feeds for carriage returns.
func cleanShellText(data []byte) string {
	text := strings.ToValidUTF8(string(data), "�")
	if !strings.ContainsAny(text, "\x1b\r") {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	for i := 0; i < len(text); {
		switch text[i] {
		case 0x1b:
			i = stringsx.SkipANSIEscape(text, i)
		case '\r':
			// A lone carriage return redraws a line; the model reads each
			// redraw as its own line.
			if i+1 >= len(text) || text[i+1] != '\n' {
				out.WriteByte('\n')
			}
			i++
		default:
			out.WriteByte(text[i])
			i++
		}
	}
	return out.String()
}
