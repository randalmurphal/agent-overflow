package cilog

import (
	"bytes"
	"regexp"
	"strconv"
)

// lineSink receives a log as display lines: text appends to the current
// line, endLine closes it. marker is a GitLab section marker, which is not
// a line of its own in the display text.
type lineSink interface {
	text(p []byte) error
	endLine() error
	marker(start bool, unix int64, name string) error
}

// traceSegmentMax bounds the bytes the GitLab cleaner holds of one run of
// trace text without a carriage return or newline. A longer run is closed
// as a display line of its own there, so a pathological trace cannot make
// the cleaner hold an unbounded line; a real trace's progress frames and
// output lines are far shorter.
const traceSegmentMax = 1 << 20

var (
	// Timestamped trace prefix (GitLab 17+): "<RFC3339 ts> 00O+ ": two-digit
	// stream number, O/E stream type, optional continuation marker. The
	// timestamp is kept; the stream flags are noise.
	traceStreamPrefix = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d+Z) \d{2}[OE][+ ]?`)
	// A collapsible-section marker: section_start:<unix>:<name> with
	// optional [key=value,...] options, or section_end:<unix>:<name>.
	traceSectionMarker = regexp.MustCompile(`section_(start|end):(\d+):([A-Za-z0-9_.-]+)(?:\[[^\]]*\])?`)
	// CSI erase-in-line (ESC[K, ESC[0K, ESC[1K, ESC[2K). The ANSI renderer
	// handles colors but not erase controls, which would show as "[0K".
	ansiEraseInLine = regexp.MustCompile("\x1b\\[[0-2]?K")
)

// traceCleaner turns a raw GitLab job trace, fed in arbitrary chunks, into
// display lines and section markers, holding at most one segment of text.
//
// Per raw line: the stream flags after a leading timestamp are dropped and
// the timestamp prefixes each piece of text; carriage-return overwrites are
// resolved terminal-style, so a segment followed by \r is replaced by what
// follows it; erase-line escapes are removed. Section markers split the
// line: the text before, between and after them become display lines of
// their own when anything visible is left, and each marker goes to the
// sink in its place. A line with no marker is one display line, blank
// included. The line after a section_start is the section's header.
type traceCleaner struct {
	sink lineSink
	seg  []byte
	// lineStart is true until the line's first segment is processed, which
	// is where its timestamp prefix is.
	lineStart bool
	timestamp []byte
	// sawMarker is true once the current raw line held a marker.
	sawMarker bool
	err       error
}

func newTraceCleaner(sink lineSink) *traceCleaner {
	return &traceCleaner{sink: sink, lineStart: true}
}

func (c *traceCleaner) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexAny(p, "\r\n")
		if i < 0 {
			c.seg = append(c.seg, p...)
			if len(c.seg) > traceSegmentMax {
				c.fail(c.endLine())
			}
			break
		}
		c.seg = append(c.seg, p[:i]...)
		if p[i] == '\r' {
			c.fail(c.overwrite())
		} else {
			c.fail(c.endLine())
		}
		p = p[i+1:]
		if c.err != nil {
			return 0, c.err
		}
	}
	return n, c.err
}

func (c *traceCleaner) fail(err error) {
	if c.err == nil {
		c.err = err
	}
}

// close ends the trace: a last line without a newline is a line.
func (c *traceCleaner) close() error {
	if c.err != nil {
		return c.err
	}
	if len(c.seg) > 0 || !c.lineStart {
		return c.endLine()
	}
	return nil
}

func (c *traceCleaner) takePrefix() {
	if !c.lineStart {
		return
	}
	c.lineStart = false
	c.timestamp = c.timestamp[:0]
	if m := traceStreamPrefix.FindSubmatchIndex(c.seg); m != nil {
		c.timestamp = append(c.timestamp, c.seg[m[2]:m[3]]...)
		c.seg = c.seg[:copy(c.seg, c.seg[m[1]:])]
	}
}

// overwrite handles a segment ended by \r: the text before and between
// its markers stands, the rest is overwritten by what follows.
func (c *traceCleaner) overwrite() error {
	c.takePrefix()
	markers := traceSectionMarker.FindAllSubmatchIndex(c.seg, -1)
	err := c.pieces(markers, false)
	c.seg = c.seg[:0]
	return err
}

// endLine handles the segment ending a raw line.
func (c *traceCleaner) endLine() error {
	c.takePrefix()
	markers := traceSectionMarker.FindAllSubmatchIndex(c.seg, -1)
	var err error
	if markers == nil && !c.sawMarker {
		err = c.emit(c.seg, true)
	} else {
		err = c.pieces(markers, true)
	}
	c.seg = c.seg[:0]
	c.lineStart = true
	c.sawMarker = false
	return err
}

// pieces emits the text around a segment's markers and the markers; the
// text after the last one only when the segment ends the line.
func (c *traceCleaner) pieces(markers [][]int, last bool) error {
	at := 0
	for _, m := range markers {
		c.sawMarker = true
		if err := c.emit(c.seg[at:m[0]], false); err != nil {
			return err
		}
		unix, err := strconv.ParseInt(string(c.seg[m[4]:m[5]]), 10, 64)
		if err != nil {
			unix = 0
		}
		if err := c.sink.marker(string(c.seg[m[2]:m[3]]) == "start", unix, string(c.seg[m[6]:m[7]])); err != nil {
			return err
		}
		at = m[1]
	}
	if last {
		return c.emit(c.seg[at:], false)
	}
	return nil
}

// emit writes one piece of text as a display line under the line's
// timestamp, erase escapes removed; an empty piece only when keepEmpty.
func (c *traceCleaner) emit(text []byte, keepEmpty bool) error {
	text = ansiEraseInLine.ReplaceAll(text, nil)
	if len(text) == 0 && !keepEmpty {
		return nil
	}
	if len(text) > 0 && len(c.timestamp) > 0 {
		if err := c.sink.text(c.timestamp); err != nil {
			return err
		}
		if err := c.sink.text([]byte{' '}); err != nil {
			return err
		}
	}
	if err := c.sink.text(text); err != nil {
		return err
	}
	return c.sink.endLine()
}

// lineSplitter passes a log through as display lines unchanged: text up to
// each newline is a line, written as it arrives without holding the line.
// A UTF-8 byte order mark at the very start is dropped.
type lineSplitter struct {
	sink    lineSink
	started bool
	bom     []byte
	open    bool
	err     error
}

const utf8BOM = "\xef\xbb\xbf"

func (s *lineSplitter) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n := len(p)
	if !s.started {
		// Hold the first bytes until they can be told from a BOM.
		need := len(utf8BOM) - len(s.bom)
		take := min(need, len(p))
		s.bom = append(s.bom, p[:take]...)
		p = p[take:]
		if len(s.bom) < len(utf8BOM) && bytes.HasPrefix([]byte(utf8BOM), s.bom) {
			return n, nil
		}
		s.started = true
		head := s.bom
		if string(head) == utf8BOM {
			head = nil
		}
		if err := s.feed(head); err != nil {
			return 0, err
		}
	}
	if err := s.feed(p); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *lineSplitter) feed(p []byte) error {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.open = true
			s.err = s.sink.text(p)
			return s.err
		}
		if i > 0 {
			if s.err = s.sink.text(p[:i]); s.err != nil {
				return s.err
			}
		}
		s.open = false
		if s.err = s.sink.endLine(); s.err != nil {
			return s.err
		}
		p = p[i+1:]
	}
	return nil
}

// close ends the log: a last line without a newline is a line.
func (s *lineSplitter) close() error {
	if s.err != nil {
		return s.err
	}
	if !s.started {
		// A log shorter than a BOM that is not one is text.
		s.started = true
		if err := s.feed(s.bom); err != nil {
			return err
		}
	}
	if s.open {
		s.open = false
		return s.sink.endLine()
	}
	return nil
}
