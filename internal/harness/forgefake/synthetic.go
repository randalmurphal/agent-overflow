package forgefake

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// A job log the fake serves: a seeded Log, or a SyntheticLog generated
// line by line on every request, so a spec can ask for a log of many
// megabytes without the fixture, the engine or the response holding it.
// Both are served with the forge's own Range behavior (logRange).

// SyntheticLog generates a job's log instead of Log. Each section is one
// GitHub step or one GitLab section of Lines body lines, LineBytes bytes
// each with the newline. On GitHub a section's lines carry the time of the
// job's matching started step (the n-th step that has a startedAt, in
// order) and its first line is the line the runner writes when the step
// begins (`##[group]<name>`), so the app splits the log by its steps. On
// GitLab each section is wrapped in the runner's markers. A running
// GitLab job serves RunningLines lines of it on the first trace request
// and GrowLines more on each later one, the way a running trace grows
// between polls; a completed job serves all of it.
type SyntheticLog struct {
	Sections     []SyntheticSection `json:"sections"`
	LineBytes    int                `json:"lineBytes,omitempty"`
	RunningLines int                `json:"runningLines,omitempty"`
	GrowLines    int                `json:"growLines,omitempty"`
}

// SyntheticSection is one step or section of a SyntheticLog.
type SyntheticSection struct {
	Name  string `json:"name"`
	Lines int    `json:"lines"`
}

const (
	defaultSyntheticLineBytes = 80
	// minSyntheticLineBytes fits the GitHub time prefix and a line number.
	minSyntheticLineBytes = 64
	maxSyntheticLineBytes = 1 << 20
	// maxSyntheticBytes bounds what one seeded log can generate: a spec
	// asks for tens of megabytes, not for the harness to stream for minutes.
	maxSyntheticBytes = 1 << 30
)

func (s *SyntheticLog) normalize() error {
	if len(s.Sections) == 0 {
		return errors.New("synthetic log needs at least one section")
	}
	if s.LineBytes == 0 {
		s.LineBytes = defaultSyntheticLineBytes
	}
	if s.LineBytes < minSyntheticLineBytes || s.LineBytes > maxSyntheticLineBytes {
		return fmt.Errorf("synthetic lineBytes %d is outside %d..%d", s.LineBytes, minSyntheticLineBytes, maxSyntheticLineBytes)
	}
	var total int64
	for i, section := range s.Sections {
		if section.Name == "" || section.Lines <= 0 {
			return fmt.Errorf("synthetic sections[%d]: a name and a positive line count are required", i)
		}
		total += int64(section.Lines) * int64(s.LineBytes)
	}
	if total > maxSyntheticBytes {
		return fmt.Errorf("synthetic log of %d bytes is over the %d the fake generates", total, maxSyntheticBytes)
	}
	if s.RunningLines < 0 || s.GrowLines < 0 {
		return errors.New("synthetic runningLines and growLines cannot be negative")
	}
	return nil
}

// logBody is a job log as a response carries it: written after the
// engine's lock is released, from values the handler copied under it.
type logBody interface {
	size() int64
	// writeRange writes bytes [from, to) of the log.
	writeRange(w io.Writer, from, to int64) error
}

type bytesBody []byte

func (b bytesBody) size() int64 { return int64(len(b)) }

func (b bytesBody) writeRange(w io.Writer, from, to int64) error {
	_, err := w.Write(b[from:to])
	return err
}

// syntheticBody generates a SyntheticLog for one forge. units bounds what
// is served: every line, marker lines included, counts as one unit.
type syntheticBody struct {
	forge     string
	sections  []SyntheticSection
	lineBytes int
	// times holds each section's GitHub second, RFC 3339.
	times []string
	// units is the number of lines served; -1 serves all of them.
	units int
	total int64
}

func newSyntheticBody(forge string, job *Job, units int) *syntheticBody {
	log := job.Synthetic
	b := &syntheticBody{forge: forge, sections: log.Sections, lineBytes: log.LineBytes, units: units}
	if forge == "github" {
		var started []string
		for _, step := range job.Steps {
			if step.StartedAt != "" && step.Status != "skipped" {
				started = append(started, step.StartedAt)
			}
		}
		for i := range log.Sections {
			at := job.StartedAt
			if i < len(started) {
				at = started[i]
			}
			b.times = append(b.times, githubLineTime(at))
		}
	}
	b.total = b.generate(io.Discard, 0, -1)
	return b
}

// githubLineTime is a step's start as a GitHub log line's time prefix, to
// 100 ns.
func githubLineTime(at string) string {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return t.UTC().Format("2006-01-02T15:04:05") + ".0000000Z "
}

func (b *syntheticBody) size() int64 { return b.total }

func (b *syntheticBody) writeRange(w io.Writer, from, to int64) error {
	var failed error
	b.generate(writerFunc(func(p []byte) (int, error) {
		if failed == nil {
			_, failed = w.Write(p)
		}
		return len(p), nil
	}), from, to)
	return failed
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// generate renders the log, writing the bytes in [from, to) (to < 0 is the
// end), and returns its size. One line buffer is reused, so memory does
// not grow with the log.
func (b *syntheticBody) generate(w io.Writer, from, to int64) int64 {
	var at int64
	units := 0
	line := make([]byte, 0, b.lineBytes+64)
	emit := func() bool {
		if b.units >= 0 && units >= b.units {
			return false
		}
		units++
		start, end := at, at+int64(len(line))
		at = end
		lo, hi := max(start, from), end
		if to >= 0 {
			hi = min(end, to)
		}
		if lo < hi {
			_, _ = w.Write(line[lo-start : hi-start])
		}
		return true
	}
	const unix = 1_767_225_600 // 2026-01-01T00:00:00Z
	if b.forge == "gitlab" {
		line = append(line[:0], "Running with gitlab-runner 17.0.0 (synthetic)\n"...)
		if !emit() {
			return at
		}
	}
	for i, section := range b.sections {
		slug := syntheticSlug(section.Name, i)
		line = line[:0]
		if b.forge == "github" {
			line = append(line, b.times[i]...)
			line = append(line, "##[group]"...)
		} else {
			line = append(line, "section_start:"...)
			line = strconv.AppendInt(line, unix+int64(i), 10)
			line = append(line, ':')
			line = append(line, slug...)
			line = append(line, "\r\x1b[0K"...)
		}
		line = append(line, section.Name...)
		line = append(line, '\n')
		if !emit() {
			return at
		}
		for n := 1; n <= section.Lines; n++ {
			line = line[:0]
			if b.forge == "github" {
				line = append(line, b.times[i]...)
			}
			line = append(line, slug...)
			line = append(line, " line "...)
			line = strconv.AppendInt(line, int64(n), 10)
			line = append(line, ' ')
			for len(line) < b.lineBytes-1 {
				line = append(line, '.')
			}
			line = append(line, '\n')
			if !emit() {
				return at
			}
		}
		if b.forge == "gitlab" {
			line = append(line[:0], "\x1b[0Ksection_end:"...)
			line = strconv.AppendInt(line, unix+int64(i)+1, 10)
			line = append(line, ':')
			line = append(line, slug...)
			line = append(line, "\r\x1b[0K\n"...)
			if !emit() {
				return at
			}
		}
	}
	return at
}

// syntheticSlug is a section's name as a GitLab marker spells it.
func syntheticSlug(name string, index int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String() + "_" + strconv.Itoa(index)
}

// syntheticUnits is the number of lines a SyntheticLog has in all.
func syntheticUnits(forge string, log *SyntheticLog) int {
	units := 0
	if forge == "gitlab" {
		units++
	}
	for _, section := range log.Sections {
		units += section.Lines + 1
		if forge == "gitlab" {
			units++
		}
	}
	return units
}

// syntheticETag names one generated log: the same spec served to the same
// extent is the same log.
func syntheticETag(forge string, job *Job, units int) string {
	sum := sha256.New()
	writeHash(sum, "%s\x00%d\x00%d\x00", forge, job.ID, units)
	for _, step := range job.Steps {
		writeHash(sum, "%s\x00%s\x00", step.Status, step.StartedAt)
	}
	writeHash(sum, "%d\x00", job.Synthetic.LineBytes)
	for _, section := range job.Synthetic.Sections {
		writeHash(sum, "%s\x00%d\x00", section.Name, section.Lines)
	}
	return `"` + hex.EncodeToString(sum.Sum(nil)[:16]) + `"`
}

// rangePolicy is how one log endpoint answers Range, as measured
// (docs/references/forge-api-measurements.md#logs).
type rangePolicy int

const (
	// rangeIgnored: every Range is ignored (a running GitLab trace).
	rangeIgnored rangePolicy = iota
	// rangeBlob: start and closed ranges are honored, a suffix range is
	// ignored (the GitHub log blob).
	rangeBlob
	// rangeTrace: start and closed ranges are honored, a suffix range is
	// refused with 400 (a completed GitLab trace).
	rangeTrace
)

// logRange answers a log response's Range header under policy: the whole
// body (200), a 206 with Content-Range, a 416 for a start at or past the
// end, or a 400 for a range the endpoint refuses. A spec that is not one
// bytes range is ignored, as servers do.
func logRange(resp response, spec string, policy rangePolicy) response {
	if spec == "" || resp.body == nil || resp.status != 0 || policy == rangeIgnored {
		return resp
	}
	size := resp.body.size()
	first, last, suffix, ok := parseByteRange(spec)
	if !ok {
		return resp
	}
	if suffix {
		if policy == rangeTrace {
			return response{route: resp.route, status: http.StatusBadRequest, stderr: "400 Bad request - Range"}
		}
		return resp
	}
	if first >= size {
		header := resp.header.Clone()
		header.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
		return response{route: resp.route, status: http.StatusRequestedRangeNotSatisfiable, header: header, stderr: "416 Range Not Satisfiable"}
	}
	if last < 0 || last >= size {
		last = size - 1
	}
	resp.header = resp.header.Clone()
	resp.header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, size))
	resp.status = http.StatusPartialContent
	resp.from, resp.to = first, last+1
	return resp
}

// parseByteRange reads `bytes=a-`, `bytes=a-b` or `bytes=-n`; last is -1
// for an open range.
func parseByteRange(spec string) (first, last int64, suffix, ok bool) {
	value, found := strings.CutPrefix(spec, "bytes=")
	if !found || strings.Contains(value, ",") {
		return 0, 0, false, false
	}
	lo, hi, found := strings.Cut(value, "-")
	if !found {
		return 0, 0, false, false
	}
	if lo == "" {
		n, err := strconv.ParseInt(hi, 10, 64)
		return 0, 0, true, err == nil && n > 0
	}
	first, err := strconv.ParseInt(lo, 10, 64)
	if err != nil || first < 0 {
		return 0, 0, false, false
	}
	if hi == "" {
		return first, -1, false, true
	}
	last, err = strconv.ParseInt(hi, 10, 64)
	if err != nil || last < first {
		return 0, 0, false, false
	}
	return first, last, false, true
}

// writeHash formats into a hash, whose Write never fails.
func writeHash(sum hash.Hash, format string, args ...any) {
	_, _ = fmt.Fprintf(sum, format, args...)
}
