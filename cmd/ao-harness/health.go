package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The pure half of `ao-harness health`: the since-last-check cursor, the
// log scanners, and the rule that turns a set of section verdicts into an
// exit code. Everything here takes paths and bytes, so the whole rollup is
// testable against canned files with no instance running.

// healthCursorFileName sits in the instance's own data dir, so two
// harnesses keep independent "what have I already reported" state and
// deleting a data root takes its cursor with it.
const healthCursorFileName = "health-cursor.json"

// healthFileCursor remembers where a scan stopped. Size travels with the
// offset because it is the only way to notice a ROTATION: uitrace rotates
// at a size cap and `up` truncates the stderr capture, and a reader that
// kept its old offset would then skip the head of the new file or read
// past the end of the new one.
type healthFileCursor struct {
	Offset int64 `json:"offset"`
	Size   int64 `json:"size"`
}

type healthCursor struct {
	Instance       string           `json:"instance"`
	CheckedAt      string           `json:"checkedAt"`
	FrontendErrors healthFileCursor `json:"frontendErrors"`
	UITrace        healthFileCursor `json:"uiTrace"`
	BackendStderr  healthFileCursor `json:"backendStderr"`
}

func healthCursorPath(dataDir string) string {
	return filepath.Join(dataDir, healthCursorFileName)
}

// loadHealthCursor reads the cursor, treating an absent or unreadable file
// as a fresh start. A corrupt cursor must not stop a health check: the
// worst it costs is one over-reported tick.
func loadHealthCursor(dataDir string) healthCursor {
	data, err := os.ReadFile(healthCursorPath(dataDir))
	if err != nil {
		return healthCursor{}
	}
	var cursor healthCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return healthCursor{}
	}
	return cursor
}

func saveHealthCursor(dataDir string, cursor healthCursor) error {
	path := healthCursorPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(cursor, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o600)
}

// scanNewLines reads whole lines from `from.Offset` to end of file and
// reports where to resume. A file that SHRANK since the last check
// rotated, so the scan restarts at zero and says so; a caller renders that
// as "rotated" rather than as a suspiciously large burst of new lines.
func scanNewLines(path string, from healthFileCursor) (lines []string, next healthFileCursor, rotated bool, err error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, healthFileCursor{}, false, nil
		}
		return nil, from, false, err
	}
	start := from.Offset
	if info.Size() < from.Size || start > info.Size() {
		start, rotated = 0, true
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, from, rotated, err
	}
	defer file.Close()
	if start > 0 {
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			return nil, from, rotated, err
		}
	}
	reader := bufio.NewReader(file)
	consumed := start
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			// A trailing partial line is a writer mid-append: leave the
			// cursor before it so the next check reads it whole.
			break
		}
		consumed += int64(len(line))
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines, healthFileCursor{Offset: consumed, Size: info.Size()}, rotated, nil
}

// healthOracleLabels are the ui-trace records that mean a standing
// regression oracle FIRED. Both are silent when their fix is in place, so
// any emission is the finding.
//
// `timeline.row.resize` is deliberately not here: it is the row-resize
// TRACKER, which emits continuously in an oracle build and says nothing
// about correctness. Counting it would bury the two records that do.
var healthOracleLabels = []string{
	"timeline.margin.diverge",
	"timeline.reasoning.tailJump",
}

// countOracleTriggers folds ui-trace lines into per-oracle counts. The
// trace is NDJSON whose every record carries a `label`; a line that does
// not decode is skipped rather than failing the scan, because the trace is
// append-only diagnostic output and a torn line is normal.
func countOracleTriggers(lines []string) map[string]int {
	counts := map[string]int{}
	for _, line := range lines {
		var record struct {
			Label string `json:"label"`
		}
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		for _, label := range healthOracleLabels {
			if record.Label == label {
				counts[label]++
			}
		}
	}
	return counts
}

// healthFrontendScan splits captured renderer errors into the two things
// that land in one file. A FAULT is an application error: something threw
// and nothing caught it. A NOTICE is the engine talking, and the only
// member so far is "ResizeObserver loop completed with undelivered
// notifications", emitted through window.onerror with no stack whenever a
// resize callback chain does not settle in one frame, which a heavy stream
// produces routinely and which no code threw.
//
// They are separated rather than filtered because the notice is still
// evidence (it means layout work outran a frame) and hiding it would lose
// a signal this timeline actually cares about. It is counted and reported;
// it just does not turn a rollup red on its own.
type healthFrontendScan struct {
	Faults  int      `json:"faults"`
	Notices int      `json:"notices"`
	Sample  []string `json:"sample,omitempty"`
}

var healthFrontendNotices = []string{"ResizeObserver loop"}

func scanFrontendErrors(lines []string) healthFrontendScan {
	scan := healthFrontendScan{}
	for _, line := range lines {
		if isFrontendNotice(line) {
			scan.Notices++
			continue
		}
		scan.Faults++
		if len(scan.Sample) < healthLogSampleCap {
			scan.Sample = append(scan.Sample, truncate(line, 160))
		}
	}
	return scan
}

func isFrontendNotice(line string) bool {
	for _, notice := range healthFrontendNotices {
		if strings.Contains(line, notice) {
			return true
		}
	}
	return false
}

// healthLogScan is the backend-stderr verdict: the counts plus the first
// few lines that produced them, because a count with no sample sends the
// reader to `logs backend` for something health could have shown.
type healthLogScan struct {
	Total  int      `json:"total"`
	Fatal  int      `json:"fatal"`
	Errors int      `json:"errors"`
	Warns  int      `json:"warns"`
	Sample []string `json:"sample,omitempty"`
}

// healthLogSampleCap bounds the excerpt. Three lines identify what is
// happening; a wall of them is what `logs backend` is for.
const healthLogSampleCap = 3

// scanBackendLog classifies free-text stderr. Go's `log` output carries no
// level, so the classification is by KEYWORD, in severity order so a line
// saying "error" inside a panic dump counts once, as a panic.
func scanBackendLog(lines []string) healthLogScan {
	scan := healthLogScan{Total: len(lines)}
	for _, line := range lines {
		lower := strings.ToLower(line)
		switch {
		case strings.Contains(lower, "panic:"), strings.Contains(lower, "fatal error"):
			scan.Fatal++
		case strings.Contains(lower, "error"):
			scan.Errors++
		case strings.Contains(lower, "warn"):
			scan.Warns++
		default:
			continue
		}
		if len(scan.Sample) < healthLogSampleCap {
			scan.Sample = append(scan.Sample, truncate(line, 160))
		}
	}
	return scan
}

// healthStatus is the three-way verdict every section carries.
type healthStatus string

const (
	healthOK   healthStatus = "ok"
	healthWarn healthStatus = "warn"
	healthRed  healthStatus = "red"
)

// healthSection is one concern's answer.
type healthSection struct {
	Name   string       `json:"name"`
	Status healthStatus `json:"status"`
	Detail string       `json:"detail"`
	// Lines are the supporting excerpt, printed under the section.
	Lines []string `json:"lines,omitempty"`
}

// healthReport is the whole rollup.
type healthReport struct {
	At       string          `json:"at"`
	Instance string          `json:"instance"`
	Sections []healthSection `json:"sections"`
}

// Worst is the report's overall verdict: the most severe section wins.
func (r healthReport) Worst() healthStatus {
	worst := healthOK
	for _, section := range r.Sections {
		switch section.Status {
		case healthRed:
			return healthRed
		case healthWarn:
			worst = healthWarn
		}
	}
	return worst
}

// exitHealthRed is health's "the answer is bad news" code, the same third
// code bench drift uses: 1 stays "the harness refused", 2 stays "you typed
// it wrong". A warn is not a failure and exits 0: a rollup that failed on
// every stderr warning would be ignored within a day.
const exitHealthRed = 3

func healthExitCode(report healthReport) int {
	if report.Worst() == healthRed {
		return exitHealthRed
	}
	return exitOK
}

func renderHealthReport(report healthReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "health %s  instance %s  %s\n", report.Worst(), report.Instance, report.At)
	for _, section := range report.Sections {
		fmt.Fprintf(&b, "  %-4s %-16s %s\n", section.Status, section.Name, section.Detail)
		for _, line := range section.Lines {
			fmt.Fprintf(&b, "         %s\n", line)
		}
	}
	return b.String()
}

// renderHealthWatchLines is --watch's form: one timestamped line per
// section, appended forever. No clear-screen and no cursor movement, so
// the output pipes into a file and greps like any other log.
func renderHealthWatchLines(report healthReport) string {
	var b strings.Builder
	for _, section := range report.Sections {
		fmt.Fprintf(&b, "%s %-4s %-16s %s\n", report.At, section.Status, section.Name, section.Detail)
	}
	return b.String()
}

func formatOracleCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s x%d", name, counts[name]))
	}
	return strings.Join(parts, ", ")
}

func formatUptime(startedAt string) string {
	started, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		return "unknown uptime"
	}
	return "up " + time.Since(started).Round(time.Second).String()
}
