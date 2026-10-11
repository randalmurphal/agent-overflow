package cilog

import (
	"bufio"
	"bytes"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// indexStride is the line distance between the byte offsets a log's index
// keeps. A window read seeks to the offset at or before its first line and
// scans fewer than indexStride lines to reach it, and an index costs 8
// bytes per indexStride lines: a 100 MB log of 100-byte lines keeps 4,000
// offsets (32 KB) and a read scans at most 255 lines (about 25 KB) to find
// its start.
const indexStride = 256

// headMax is how much of a line the index keeps while it is built, for
// what a section is named by and a GitHub line's time.
const headMax = 1024

// lineIndex locates a log's lines in its display file.
type lineIndex struct {
	// offsets[k] is the byte offset of line k*indexStride.
	offsets []int64
	lines   int
	bytes   int64
}

// second is a GitHub log line's time to the second, as Unix time.
type secondMark struct {
	line   int
	second int64
}

// builder writes a log's display lines to its file and indexes them as
// they pass: the sparse line offsets, a GitHub log's per-second marks, a
// GitLab trace's top-level sections. It also compares the lines against
// the version it replaces, to report how many leading lines are
// unchanged. Nothing it keeps grows with the bytes of the log.
type builder struct {
	out     *bufio.Writer
	written int64
	idx     lineIndex

	lineOpen bool
	lineLen  int64
	head     []byte

	github  bool
	seconds []secondMark
	lastSec []byte

	spans spanBuilder

	// prev reads the version this one replaces; stable is the count of
	// leading lines found equal, -1 while no difference was seen.
	prev   *bufio.Reader
	stable int
	cmp    []byte
}

func newBuilder(out io.Writer, github bool, prev io.Reader) *builder {
	b := &builder{out: bufio.NewWriterSize(out, 64<<10), github: github, stable: -1}
	if prev != nil {
		b.prev = bufio.NewReaderSize(prev, 64<<10)
	} else {
		b.stable = 0
	}
	b.spans.init()
	return b
}

func (b *builder) text(p []byte) error {
	if !b.lineOpen {
		b.lineOpen = true
		b.lineLen = 0
		b.head = b.head[:0]
		if b.idx.lines%indexStride == 0 {
			b.idx.offsets = append(b.idx.offsets, b.written)
		}
	}
	if room := headMax - len(b.head); room > 0 {
		b.head = append(b.head, p[:min(room, len(p))]...)
	}
	b.lineLen += int64(len(p))
	return b.write(p)
}

var newline = []byte{'\n'}

func (b *builder) endLine() error {
	if !b.lineOpen {
		// An empty line.
		if err := b.text(nil); err != nil {
			return err
		}
	}
	if err := b.write(newline); err != nil {
		return err
	}
	line := b.idx.lines
	b.idx.lines++
	b.lineOpen = false
	if b.github {
		b.markSecond(line)
	} else {
		b.spans.line(line, b.written-b.lineLen-1, b.head)
	}
	return nil
}

func (b *builder) marker(start bool, unix int64, name string) error {
	if b.github {
		return nil
	}
	b.spans.marker(start, unix, name, b.idx.lines, b.written)
	return nil
}

func (b *builder) write(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	b.compare(p)
	n, err := b.out.Write(p)
	b.written += int64(n)
	return err
}

// compare checks p against the same bytes of the previous version. The
// first difference, or either version ending first, fixes stable at the
// lines completed before the line it falls in.
func (b *builder) compare(p []byte) {
	if b.stable >= 0 {
		return
	}
	if cap(b.cmp) < len(p) {
		b.cmp = make([]byte, len(p))
	}
	got, err := io.ReadFull(b.prev, b.cmp[:len(p)])
	if err != nil || !bytes.Equal(b.cmp[:got], p) {
		b.stable = b.idx.lines
	}
}

// finish flushes the file and settles the comparison: a version that
// matched the previous one to its end keeps every line it has, and one
// the previous version continues past keeps them too.
func (b *builder) finish() (lineIndex, int, error) {
	if err := b.out.Flush(); err != nil {
		return lineIndex{}, 0, err
	}
	b.idx.bytes = b.written
	stable := b.stable
	if stable < 0 {
		stable = b.idx.lines
	}
	return b.idx, stable, nil
}

// A GitHub log line starts with its time to 100 ns and a space, 29 bytes.
const githubPrefix = 29

var githubTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d+Z $`)

// markSecond records the line where a new second starts. A line without
// its own time keeps the one before it; lines before the first timed line
// take that line's.
func (b *builder) markSecond(line int) {
	if b.lineLen <= githubPrefix || len(b.head) < githubPrefix || !githubTime.Match(b.head[:githubPrefix]) {
		return
	}
	sec := b.head[:19]
	if len(b.seconds) > 0 && bytes.Equal(sec, b.lastSec) {
		return
	}
	t, err := time.Parse("2006-01-02T15:04:05", string(sec))
	if err != nil {
		return
	}
	b.lastSec = append(b.lastSec[:0], sec...)
	at := line
	if len(b.seconds) == 0 {
		at = 0
	}
	b.seconds = append(b.seconds, secondMark{line: at, second: t.Unix()})
}

var (
	leadingTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z `)
	// An SGR or erase sequence; ESC is the one control byte that opens one.
	ansiSequence = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")
)

// plainName is a line as a row name: no leading time, no escape
// sequences, trimmed, valid UTF-8.
func plainName(line []byte) string {
	line = leadingTime.ReplaceAll(line, nil)
	line = ansiSequence.ReplaceAll(line, nil)
	name := strings.TrimSpace(string(line))
	if !utf8.ValidString(name) {
		name = strings.ToValidUTF8(name, "�")
	}
	return name
}
