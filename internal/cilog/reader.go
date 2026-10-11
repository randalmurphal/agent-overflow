package cilog

import (
	"bufio"
	"errors"
	"io"
	"unicode/utf8"
)

// readBlock is the buffer a line read scans through. A line longer than it
// is read through in pieces, never held whole unless it is returned.
const readBlock = 64 << 10

// lineReader reads lines of one published version of a log through its
// index. Every read is bounded by the version's size, so bytes a later
// download appends are never seen.
type lineReader struct {
	file io.ReaderAt
	idx  lineIndex
}

// at returns a reader positioned at line, which is at most the line count,
// having scanned fewer than indexStride lines to reach it.
func (r lineReader) at(line int) (*bufio.Reader, int64, error) {
	if line < 0 || line > r.idx.lines {
		return nil, 0, errors.New("cilog: line out of range")
	}
	if line == r.idx.lines {
		return bufio.NewReaderSize(io.NewSectionReader(r.file, r.idx.bytes, 0), 16), r.idx.bytes, nil
	}
	k := line / indexStride
	offset := r.idx.offsets[k]
	br := bufio.NewReaderSize(io.NewSectionReader(r.file, offset, r.idx.bytes-offset), readBlock)
	for i := k * indexStride; i < line; i++ {
		n, err := skipLine(br)
		if err != nil {
			return nil, 0, err
		}
		offset += n
	}
	return br, offset, nil
}

// offset is the byte offset where line starts; the line count gives the
// log's size.
func (r lineReader) offset(line int) (int64, error) {
	_, offset, err := r.at(line)
	return offset, err
}

// skipLine reads past one line and returns its length with the newline.
func skipLine(br *bufio.Reader) (int64, error) {
	var n int64
	for {
		chunk, err := br.ReadSlice('\n')
		n += int64(len(chunk))
		switch {
		case err == nil:
			return n, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && n > 0:
			return n, nil
		default:
			return n, unexpectedEnd(err)
		}
	}
}

func unexpectedEnd(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// scan calls fn for each line from from up to and including to (bounded
// by the line count) with the line's first headMax bytes, without its
// newline, and its length. fn returning false stops the scan.
func (r lineReader) scan(from, to int, fn func(line int, head []byte, length int64) bool) error {
	to = min(to, r.idx.lines-1)
	if from > to {
		return nil
	}
	br, _, err := r.at(from)
	if err != nil {
		return err
	}
	head := make([]byte, 0, headMax)
	for line := from; line <= to; line++ {
		head = head[:0]
		var length int64
		for {
			chunk, err := br.ReadSlice('\n')
			length += int64(len(chunk))
			if room := headMax - len(head); room > 0 {
				head = append(head, chunk[:min(room, len(chunk))]...)
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil && !(errors.Is(err, io.EOF) && length > 0) {
				return unexpectedEnd(err)
			}
			break
		}
		if length > 0 && len(head) > 0 && head[len(head)-1] == '\n' && int64(len(head)) == length {
			head = head[:len(head)-1]
		}
		if !fn(line, head, length-1) {
			return nil
		}
	}
	return nil
}

// Lines is a window of a log's lines.
type Lines struct {
	// From is the first line's number.
	From  int      `json:"from"`
	Lines []string `json:"lines"`
	// Clipped lists the lines (by number) cut short at the read's byte
	// budget: a single line longer than the whole budget.
	Clipped []int `json:"clipped,omitempty"`
	// TotalLines and Version describe the published version read.
	TotalLines int    `json:"totalLines"`
	Version    uint64 `json:"version"`
}

// lines reads up to count lines from from, keeping their text within
// budget bytes: it stops before a line that would pass the budget, except
// that the first line is always returned, clipped at the budget on a rune
// boundary when longer.
func (r lineReader) lines(from, count int, budget int) ([]string, []int, error) {
	if from >= r.idx.lines || count <= 0 {
		return nil, nil, nil
	}
	count = min(count, r.idx.lines-from)
	br, _, err := r.at(from)
	if err != nil {
		return nil, nil, err
	}
	out := make([]string, 0, count)
	var clipped []int
	used := 0
	var line []byte
	for i := 0; i < count; i++ {
		line = line[:0]
		room := budget - used
		over := false
		for {
			chunk, err := br.ReadSlice('\n')
			if errors.Is(err, bufio.ErrBufferFull) {
				if len(line)+len(chunk) > room {
					if i > 0 {
						return out, clipped, nil
					}
					over = true
					line = append(line, chunk[:max(0, room-len(line))]...)
					if _, err := skipLine(br); err != nil {
						return nil, nil, err
					}
					break
				}
				line = append(line, chunk...)
				continue
			}
			if err != nil && !(errors.Is(err, io.EOF) && len(chunk) > 0) {
				return nil, nil, unexpectedEnd(err)
			}
			chunk = chunk[:len(chunk)-btoi(len(chunk) > 0 && chunk[len(chunk)-1] == '\n')]
			if len(line)+len(chunk) > room {
				if i > 0 {
					return out, clipped, nil
				}
				over = true
				chunk = chunk[:max(0, room-len(line))]
			}
			line = append(line, chunk...)
			break
		}
		if over {
			line = trimPartialRune(line)
			clipped = append(clipped, from+i)
		}
		used += len(line)
		out = append(out, string(line))
	}
	return out, clipped, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// trimPartialRune drops a UTF-8 sequence a clip cut in two.
func trimPartialRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			return b
		}
	}
	return b
}
