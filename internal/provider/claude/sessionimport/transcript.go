package sessionimport

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider/claude/sessionfork"
)

// transcript.go — pass 1 of the two-pass transcript reader.
//
// The split is what keeps a real Claude home importable. Transcripts reach
// hundreds of megabytes, and decoding one whole into []map[string]any
// retains several times its size on the heap — a 220 MB file measured at
// 0.5-0.9 GB — before a single event exists. So pass 1 STREAMS the file and
// keeps a ~100-byte skeleton per row (the fields the DAG walks, plus the
// byte range the row's line occupies); BuildBranches runs on those. Pass 2
// (LoadedSession.ConvertBranch, session.go) seeks back and decodes ONLY the
// rows of the branch it is converting, one branch at a time.
//
// Row admission is sessionfork's, not a second opinion: the same
// TranscriptTypes set, the same empty-uuid rejection, the same
// skip-an-unparseable-line posture. What is NOT shared is the scanner —
// ParseTranscript's bufio.Scanner turns one over-long line into a terminal
// error that abandons the rest of the file, which for an import means one
// runaway tool result costs the user the whole session.

const (
	// maxTranscriptLineBytes caps one transcript line. Claude writes one
	// record per message / tool result, so anything past this is corruption
	// rather than something to buffer — and it costs that line only.
	maxTranscriptLineBytes = 16 << 20
	// transcriptScanBuffer is the reader window. Longer lines are assembled
	// across ReadSlice calls; it only decides how often that happens.
	transcriptScanBuffer = 256 << 10
	// maxTranscriptBytes refuses a file no session legitimately is. The
	// largest transcripts on a real home are a few hundred megabytes; a
	// gigabyte is a runaway writer or the wrong file, and reading one would
	// take the whole app's memory with it. Refusing is per-session — the
	// rest of an "Import All" is unaffected.
	maxTranscriptBytes = 1 << 30
)

// lastPromptType is the record that names a leaf. It is not a transcript
// type — ParseTranscript drops it — but it is what titles a branch.
const lastPromptType = "last-prompt"

// Warning codes emitted by the transcript scan.
const (
	// WarnOversizedLine marks lines skipped for exceeding the line cap.
	WarnOversizedLine = "transcript-oversized-line"
)

// ErrTranscriptTooLarge is what LoadSession refuses an absurd file with.
// The returned error's own message is user-facing prose; this sentinel is
// for callers that need to route it (the orchestrator surfaces the prose
// verbatim rather than wrapping it in a path).
var ErrTranscriptTooLarge = errors.New("sessionimport: transcript is too large to import")

// transcriptTooLargeError carries user-facing prose while still answering
// errors.Is(err, ErrTranscriptTooLarge). A wrapped sentinel would append
// its own sentence to one written for the user.
type transcriptTooLargeError struct {
	path string
	size int64
}

func (e *transcriptTooLargeError) Error() string {
	return fmt.Sprintf(
		"%s is %d MB, which is past the %d MB ceiling Agent Overflow will read a session file at. "+
			"It was skipped; nothing else in this import is affected.",
		e.path, e.size>>20, int64(maxTranscriptBytes)>>20)
}

func (e *transcriptTooLargeError) Unwrap() error { return ErrTranscriptTooLarge }

// transcriptLine is one line of a transcript with its absolute byte range.
//
// Start is the line's first byte; Data is the line without its terminator,
// so Start+len(Data) is exactly the JSON pass 2 re-reads. Data aliases the
// scanner's buffer and is valid only until the next call.
type transcriptLine struct {
	Data      []byte
	Start     int64
	Oversized bool
}

// transcriptScanner walks a JSONL transcript with exact byte offsets.
//
// A trailing line with no newline IS returned, matching ParseTranscript: a
// transcript is not being resumed from an offset the way a Codex rollout
// is, and a half-written tail simply fails to decode.
type transcriptScanner struct {
	r      *bufio.Reader
	offset int64
	buf    []byte
	done   bool
}

func newTranscriptScanner(r io.Reader) *transcriptScanner {
	return &transcriptScanner{r: bufio.NewReaderSize(r, transcriptScanBuffer)}
}

// next returns the next line, or io.EOF when none remains.
func (s *transcriptScanner) next() (transcriptLine, error) {
	if s.done {
		return transcriptLine{}, io.EOF
	}
	start := s.offset
	s.buf = s.buf[:0]
	oversized := false
	for {
		chunk, err := s.r.ReadSlice('\n')
		s.offset += int64(len(chunk))
		if !oversized && len(s.buf)+len(chunk) > maxTranscriptLineBytes {
			// Past the cap: stop accumulating but keep draining to the
			// newline so every following line stays readable. Releasing the
			// buffer here is the point — holding a 100 MB line to report it
			// would be the same failure the cap exists to prevent.
			oversized = true
			s.buf = s.buf[:0]
		}
		if !oversized {
			s.buf = append(s.buf, chunk...)
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			s.done = true
			if len(s.buf) == 0 && !oversized {
				return transcriptLine{}, io.EOF
			}
		case err != nil:
			s.done = true
			return transcriptLine{}, err
		}
		return transcriptLine{
			Data:      bytes.TrimRight(s.buf, "\r\n"),
			Start:     start,
			Oversized: oversized,
		}, nil
	}
}

// transcriptSkeleton is one streaming pass over a transcript: every
// admitted row reduced to what the DAG walks, plus the per-leaf titles the
// `last-prompt` records carry.
type transcriptSkeleton struct {
	Rows       []Row
	LeafTitles map[string]string
	Warnings   []importir.Warning
}

// scanTranscript is pass 1. It never retains a decoded line.
func scanTranscript(r io.Reader) (transcriptSkeleton, error) {
	out := transcriptSkeleton{LeafTitles: map[string]string{}}
	sc := newTranscriptScanner(r)
	oversized := 0
	for {
		line, err := sc.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return transcriptSkeleton{}, fmt.Errorf("read transcript: %w", err)
		}
		if line.Oversized {
			oversized++
			continue
		}
		row, ok := decodeSkeletonRow(line.Data, len(out.Rows), line.Start)
		if !ok {
			// Unparseable line. ParseTranscript skips these too — a crashing
			// session leaves a truncated last line and it must not cost the
			// rest of the file.
			continue
		}
		switch {
		case row.Type == lastPromptType:
			if leaf, prompt := decodeLastPrompt(line.Data); leaf != "" && prompt != "" {
				out.LeafTitles[leaf] = prompt
			}
		case admitsTranscriptRow(row):
			out.Rows = append(out.Rows, row)
		}
	}
	if oversized > 0 {
		out.Warnings = append(out.Warnings, importir.Warning{
			Code: WarnOversizedLine,
			Message: fmt.Sprintf(
				"Skipped %d record(s) larger than %d MB; their content could not be imported.",
				oversized, maxTranscriptLineBytes>>20),
		})
	}
	return out, nil
}

// admitsTranscriptRow is ParseTranscript's admission rule, applied to a
// skeleton: a transcript type (the exported set, never a local copy) and a
// non-empty uuid, which the parent-chain walk keys on.
func admitsTranscriptRow(row Row) bool {
	if row.UUID == "" {
		return false
	}
	_, ok := sessionfork.TranscriptTypes[row.Type]
	return ok
}

// skeletonLine is the narrow decode pass 1 pays per line. Decoding into
// this instead of map[string]any is the whole memory win: the parser still
// walks the JSON, but the only thing that survives the line is a handful of
// short strings.
type skeletonLine struct {
	Type                    string `json:"type"`
	Subtype                 string `json:"subtype"`
	UUID                    string `json:"uuid"`
	ParentUUID              string `json:"parentUuid"`
	LogicalParentUUID       string `json:"logicalParentUuid"`
	SourceToolAssistantUUID string `json:"sourceToolAssistantUUID"`
	IsSidechain             bool   `json:"isSidechain"`
	IsMeta                  bool   `json:"isMeta"`
	IsCompactSummary        bool   `json:"isCompactSummary"`
	Timestamp               string `json:"timestamp"`
}

// decodeSkeletonRow projects one line into a Row with no Raw attached.
func decodeSkeletonRow(data []byte, index int, start int64) (Row, bool) {
	var line skeletonLine
	if json.Unmarshal(data, &line) == nil {
		return Row{
			UUID:                    line.UUID,
			ParentUUID:              line.ParentUUID,
			LogicalParentUUID:       line.LogicalParentUUID,
			Type:                    line.Type,
			Subtype:                 line.Subtype,
			SourceToolAssistantUUID: line.SourceToolAssistantUUID,
			IsSidechain:             line.IsSidechain,
			IsMeta:                  line.IsMeta,
			IsCompactSummary:        line.IsCompactSummary,
			Timestamp:               parseISOMillis(line.Timestamp),
			Index:                   index,
			Offset:                  start,
			Length:                  len(data),
		}, true
	}
	// One of the modelled fields is not the JSON type we expect (an older
	// writer, or corruption in a field the DAG does not even read). The
	// generic decode is what ParseTranscript does; paying for it only here
	// keeps a strict-decode miss from silently dropping a real row.
	var raw map[string]any
	if json.Unmarshal(data, &raw) != nil || raw == nil {
		return Row{}, false
	}
	row := newRow(raw, index)
	row.Raw = nil
	row.Offset = start
	row.Length = len(data)
	return row, true
}

// lastPromptRecord is the per-leaf title record. It is written after the
// transcript rows and names the leaf by uuid.
type lastPromptRecord struct {
	LastPrompt string `json:"lastPrompt"`
	LeafUUID   string `json:"leafUuid"`
}

func decodeLastPrompt(data []byte) (leaf, prompt string) {
	var record lastPromptRecord
	if json.Unmarshal(data, &record) != nil {
		return "", ""
	}
	return record.LeafUUID, record.LastPrompt
}
