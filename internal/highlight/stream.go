package highlight

import (
	"sort"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// Stream highlights a document that grows only at its end, such as a code
// fence an agent is still writing. It keeps the syntax tree between appends,
// so each append reparses incrementally and re-queries only the lines the
// append can have changed: the lines from the one holding the old end, and
// any earlier lines whose syntax the new text restructured (the tree's
// changed ranges). Lines always equals Highlight(lang, source).Lines for the
// source appended so far.
//
// Grammars with language injections repaint the whole document on each
// append, because an injected region's classes come from separate parses
// the host tree's changed ranges do not describe.
//
// A Stream is not safe for concurrent use. Close releases its tree.
type Stream struct {
	eng        *engine
	src        []byte
	lineStarts []int
	end        tree_sitter.Point
	tree       *tree_sitter.Tree
	lines      []EncodedLine
	// stale is the first byte whose lines are behind the source, or -1
	// when every line is current: nothing has been painted yet, or a failed
	// parse left the lines from that byte behind.
	stale int
	over  bool
}

// StreamState is the outcome of one Append.
type StreamState int

const (
	// StreamOK: Lines describes the whole source.
	StreamOK StreamState = iota
	// StreamIncomplete: the parse failed (timeout), so Lines still
	// describes an earlier source. The next append retries.
	StreamIncomplete
	// StreamOverCap: the source passed a highlight cap. The stream stops
	// highlighting; Highlight returns the capped result for such input.
	StreamOverCap
)

// NewStream starts an empty document in lang. Unknown languages stream
// plain lines.
func NewStream(lang Lang) *Stream {
	return &Stream{eng: engineFor(lang), lineStarts: []int{0}, lines: []EncodedLine{{}}, stale: 0}
}

// Append adds p to the end of the document and reports the first line whose
// spans changed; lines before it keep their previous spans. keepTree says
// whether to hold the new tree for the next append. Without it each append
// parses the whole document.
func (s *Stream) Append(p []byte, keepTree bool) (from int, state StreamState) {
	if s.over {
		return len(s.lines), StreamOverCap
	}
	if len(p) == 0 && s.stale < 0 {
		return len(s.lines), StreamOK
	}
	oldLen, oldEnd := len(s.src), s.end
	s.src = append(s.src, p...)
	for i, b := range p {
		if b == '\n' {
			s.lineStarts = append(s.lineStarts, oldLen+i+1)
			s.end = tree_sitter.Point{Row: s.end.Row + 1}
		} else {
			s.end.Column++
		}
	}
	if len(s.src) > maxInputBytes || len(s.lineStarts) > maxResultLines {
		s.over = true
		s.dropTree()
		return len(s.lines), StreamOverCap
	}
	if s.eng == nil {
		from = len(s.lines)
		for len(s.lines) < len(s.lineStarts) {
			s.lines = append(s.lines, EncodedLine{})
		}
		return from, StreamOK
	}
	if s.eng.inj != nil {
		s.dropTree()
		return s.repaintAll()
	}

	var reuse *tree_sitter.Tree
	if s.tree != nil {
		s.tree.Edit(&tree_sitter.InputEdit{
			StartByte: uint(oldLen), OldEndByte: uint(oldLen), NewEndByte: uint(len(s.src)),
			StartPosition: oldEnd, OldEndPosition: oldEnd, NewEndPosition: s.end,
		})
		reuse = s.tree
	}
	tree := s.eng.parse(s.src, reuse)
	if tree == nil {
		// The parse ran out of time, which a loaded host can cause on its
		// own. The edited tree still describes the painted lines, so it is
		// kept and the next append parses incrementally again rather than
		// reparsing the whole document, which would only take longer.
		if !keepTree {
			s.dropTree()
		}
		if s.stale < 0 || oldLen < s.stale {
			s.stale = oldLen
		}
		return len(s.lines), StreamIncomplete
	}
	start := 0
	if reuse != nil {
		// The changed ranges are measured from the tree the lines were
		// painted with, and stale covers what was appended since.
		start = oldLen
		if s.stale >= 0 {
			start = min(start, s.stale)
		}
		for _, r := range reuse.ChangedRanges(tree) {
			start = min(start, int(r.StartByte))
		}
	}
	s.dropTree()
	if keepTree {
		s.tree = tree
	} else {
		defer tree.Close()
	}

	first := s.lineOf(start)
	lo := s.lineStarts[first]
	classes := make([]uint16, len(s.src)-lo)
	s.eng.paintCaptures(tree, s.src, lo, len(s.src), classes)
	s.stale = -1
	return s.replaceLines(first, encodeLines(s.src[lo:], classes)), StreamOK
}

// repaintAll classifies the whole document, as Highlight does.
func (s *Stream) repaintAll() (int, StreamState) {
	classes, complete := s.eng.classify(s.src)
	if classes == nil || !complete {
		s.stale = 0
		return len(s.lines), StreamIncomplete
	}
	s.stale = -1
	return s.replaceLines(0, encodeLines(s.src, classes)), StreamOK
}

// replaceLines installs fresh encodings for lines [first, end) and returns
// the first line whose runs differ from the previous encoding.
func (s *Stream) replaceLines(first int, fresh []EncodedLine) int {
	from := first
	for from < len(s.lines) && from-first < len(fresh) && sameRuns(s.lines[from], fresh[from-first]) {
		from++
	}
	s.lines = append(s.lines[:first], fresh...)
	return from
}

// lineOf returns the index of the line holding byte offset off.
func (s *Stream) lineOf(off int) int {
	return sort.Search(len(s.lineStarts), func(i int) bool { return s.lineStarts[i] > off }) - 1
}

// Lines returns the current encoding of lines [from, end). The slice is the
// caller's; later appends do not change it.
func (s *Stream) Lines(from int) []EncodedLine {
	if from >= len(s.lines) {
		return nil
	}
	return append([]EncodedLine(nil), s.lines[from:]...)
}

// LineCount is the number of lines in the source appended so far.
func (s *Stream) LineCount() int {
	return len(s.lineStarts)
}

// Source is the document appended so far. The caller must not modify it.
func (s *Stream) Source() []byte {
	return s.src
}

// HoldsTree reports whether the stream keeps a syntax tree.
func (s *Stream) HoldsTree() bool {
	return s.tree != nil
}

// Close releases the syntax tree. The stream stays usable; its next append
// parses the whole document.
func (s *Stream) Close() {
	s.dropTree()
}

func (s *Stream) dropTree() {
	if s.tree != nil {
		s.tree.Close()
		s.tree = nil
	}
}

// parse parses src with a pooled parser, reusing old when it has been
// edited to match src. It returns nil when the parse failed.
func (e *engine) parse(src []byte, old *tree_sitter.Tree) *tree_sitter.Tree {
	p := acquireParser()
	if err := p.SetLanguage(e.lang); err != nil {
		releaseParser(p)
		return nil
	}
	tree := p.Parse(src, old)
	if tree == nil {
		// A cancelled parse poisons the parser (see releaseParser).
		p.Close()
		return nil
	}
	releaseParser(p)
	return tree
}

func sameRuns(a, b EncodedLine) bool {
	if len(a.Runs) != len(b.Runs) {
		return false
	}
	for i := range a.Runs {
		if a.Runs[i] != b.Runs[i] {
			return false
		}
	}
	return true
}
