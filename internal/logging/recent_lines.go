package logging

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// The process log retained in memory for error reports. The backend log goes
// to stderr, which a desktop launch may not persist anywhere a person can
// read, so the lines leading up to a failure are kept here until they age
// out of the byte budget.
const (
	recentLogBudget  = 512 << 10
	recentLogLineMax = 4 << 10
)

type recentLines struct {
	mu    sync.Mutex
	lines []string
	head  int // index of the oldest retained line
	bytes int
}

var recentLog recentLines

// add retains each complete line of p, evicting the oldest lines past the
// budget. A line longer than recentLogLineMax is cut, not dropped.
func (r *recentLines) add(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for line := range bytes.SplitSeq(bytes.TrimRight(p, "\n"), []byte("\n")) {
		if len(line) > recentLogLineMax {
			line = append(line[:recentLogLineMax:recentLogLineMax], "…"...)
		}
		text := strings.ToValidUTF8(string(line), "�")
		r.lines = append(r.lines, text)
		r.bytes += len(text)
		for r.bytes > recentLogBudget && r.head < len(r.lines) {
			r.bytes -= len(r.lines[r.head])
			r.lines[r.head] = ""
			r.head++
		}
	}
	// Compact once the evicted prefix outgrows the live lines, so the
	// backing array stays proportional to what is retained.
	if r.head > len(r.lines)-r.head {
		r.lines = append(r.lines[:0], r.lines[r.head:]...)
		r.head = 0
	}
}

// before returns up to limit retained lines ending at the most recent line
// containing marker, and whether such a line is still retained.
func (r *recentLines) before(marker string, limit int) ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.lines) - 1; i >= r.head; i-- {
		if strings.Contains(r.lines[i], marker) {
			start := max(r.head, i+1-limit)
			return append([]string(nil), r.lines[start:i+1]...), true
		}
	}
	return nil, false
}

// RecentLogLinesBefore returns up to limit lines of this process's log
// ending at the most recent line that contains marker, such as an error
// reference. found is false once that line has aged out of memory.
func RecentLogLinesBefore(marker string, limit int) (lines []string, found bool) {
	return recentLog.before(marker, limit)
}

type recentLinesWriter struct {
	output io.Writer
	ring   *recentLines
}

func (w *recentLinesWriter) Write(p []byte) (int, error) {
	w.ring.add(p)
	return w.output.Write(p)
}

// Output is the log output every shell installs: output, with the idle HTTP
// warning annotated (WithHTTPDiagnostics) and recent lines retained for
// error reports (RecentLogLinesBefore).
func Output(output io.Writer) io.Writer {
	if _, ok := output.(*httpDiagnosticWriter); ok {
		return output
	}
	return WithHTTPDiagnostics(&recentLinesWriter{output: output, ring: &recentLog})
}
