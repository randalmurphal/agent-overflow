package cilog

import (
	"slices"
	"strconv"
)

// Span kinds of a GitLab trace: a closed top-level section, lines outside
// every section, a section still open at the end of the trace, and the
// whole log of a trace without markers. The last two take their status
// from the job when the index is read.
const (
	spanSection = "section"
	spanOutput  = "output"
	spanOpen    = "open"
	spanJob     = "job"
)

// span is one row of a GitLab trace, as the builder found it. End and
// EndByte of an open or job span are the log's end.
type span struct {
	Kind      string  `json:"kind"`
	Key       string  `json:"key"`
	Name      string  `json:"name"`
	Start     int     `json:"start"`
	End       int     `json:"end"`
	StartByte int64   `json:"startByte"`
	EndByte   int64   `json:"endByte"`
	Duration  float64 `json:"duration,omitempty"`
}

// spanBuilder splits a GitLab trace into rows from its markers as the
// lines pass. Top-level sections are rows; nested ones stay inside their
// parent; lines outside every section form rows of their own, named by
// their first line with visible text. A section is named by its header,
// the line after its start marker, or by its marker name when that line
// is blank or not written yet.
type spanBuilder struct {
	spans []span
	open  *openSpan
	stack []string
	// The rows of lines outside every section start at looseStart; their
	// name is the first line there with visible text.
	looseStart     int
	looseStartByte int64
	looseName      string
	previousKey    string
	sawMarker      bool
}

type openSpan struct {
	name      string
	unix      int64
	start     int
	startByte int64
	header    string
}

func (s *spanBuilder) init() {
	s.previousKey = "start"
}

// line is a display line, its start offset and its head.
func (s *spanBuilder) line(index int, startByte int64, head []byte) {
	if s.open != nil {
		if index == s.open.start {
			s.open.header = plainName(head)
		}
		return
	}
	if s.looseName == "" {
		s.looseName = plainName(head)
	}
}

func (s *spanBuilder) marker(start bool, unix int64, name string, lines int, at int64) {
	s.sawMarker = true
	if start {
		if s.open != nil {
			s.stack = append(s.stack, name)
			return
		}
		s.closeLoose(lines, at)
		s.open = &openSpan{name: name, unix: unix, start: lines, startByte: at}
		s.stack = append(s.stack[:0], name)
		return
	}
	depth := -1
	for i := len(s.stack) - 1; i >= 0; i-- {
		if s.stack[i] == name {
			depth = i
			break
		}
	}
	switch {
	case s.open != nil && depth > 0:
		s.stack = s.stack[:depth]
	case s.open != nil && depth == 0:
		open := s.open
		key := "section:" + open.name + ":" + strconv.FormatInt(open.unix, 10)
		row := span{Kind: spanSection, Key: key, Name: open.rowName(), Start: open.start, End: lines, StartByte: open.startByte, EndByte: at}
		if unix >= open.unix {
			row.Duration = float64(unix - open.unix)
		}
		s.spans = append(s.spans, row)
		s.previousKey = key
		s.open = nil
		s.stack = s.stack[:0]
		s.looseStart, s.looseStartByte, s.looseName = lines, at, ""
	}
	// Any other end without its start is ignored.
}

func (o *openSpan) rowName() string {
	if o.header != "" {
		return o.header
	}
	return o.name
}

func (s *spanBuilder) closeLoose(lines int, at int64) {
	if lines <= s.looseStart || s.looseName == "" {
		return
	}
	key := "output:" + s.previousKey
	s.spans = append(s.spans, span{Kind: spanOutput, Key: key, Name: s.looseName, Start: s.looseStart, End: lines, StartByte: s.looseStartByte, EndByte: at})
	s.previousKey = key
}

// result is the rows of the trace so far, ending at lines and at.
func (s *spanBuilder) result(lines int, at int64) []span {
	out := slices.Clone(s.spans)
	switch {
	case s.open != nil:
		out = append(out, span{Kind: spanOpen, Key: "section:" + s.open.name + ":" + strconv.FormatInt(s.open.unix, 10), Name: s.open.rowName(), Start: s.open.start, End: lines, StartByte: s.open.startByte, EndByte: at})
	case !s.sawMarker:
		if lines > 0 {
			out = append(out, span{Kind: spanJob, Key: "job", Start: 0, End: lines, EndByte: at})
		}
	case lines > s.looseStart && s.looseName != "":
		out = append(out, span{Kind: spanOutput, Key: "output:" + s.previousKey, Name: s.looseName, Start: s.looseStart, End: lines, StartByte: s.looseStartByte, EndByte: at})
	}
	return out
}
