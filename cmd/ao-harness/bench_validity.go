package main

// Benchmark validity is kept separate from report formatting. A report can
// contain numbers after a failed oracle, but it must carry a receipt that
// makes those numbers unusable rather than silently presenting them as a
// clean run.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harnessclient"
)

const (
	// A readiness sample is cheap, but two consecutive samples prevent a
	// transient mount from becoming the barrier that starts measurement.
	benchReadinessConfirmations = 2
	// A progress observer may miss one scheduled tick under load. Missing
	// two cadence intervals means the source or the page stopped proving the
	// workload and invalidates the run.
	benchProgressMaxGapFactor = 2
)

// benchReadiness is the synchronized active-multi-pane barrier. All fields
// describe one viewport query, not six independent element queries.
type benchReadiness struct {
	At                time.Time `json:"at"`
	PaneIDs           []string  `json:"paneIds"`
	ActiveThreadIDs   []string  `json:"activeThreadIds"`
	InactiveThreadIDs []string  `json:"inactiveThreadIds"`
}

// validateActivePaneReadiness proves the exact workload geometry. Pane IDs
// must be unique, every expected thread must own one mounted pane, and an
// active row must be visible, have text, and fit inside its pane box. The
// latter rejects a row that merely intersects the viewport while clipped.
func validateActivePaneReadiness(view uiViewport, threadIDs []string) (benchReadiness, error) {
	if !view.Settled {
		return benchReadiness{}, errors.New("active-multi-pane viewport is not settled")
	}
	if len(threadIDs) != benchActivePaneCount {
		return benchReadiness{}, fmt.Errorf("active-multi-pane expects %d threads, got %d", benchActivePaneCount, len(threadIDs))
	}
	if len(view.Panes) != benchActivePaneCount {
		return benchReadiness{}, fmt.Errorf("active-multi-pane mounted %d panes, want exactly %d", len(view.Panes), benchActivePaneCount)
	}
	want := make(map[string]bool, len(threadIDs))
	for _, id := range threadIDs {
		if id == "" || want[id] {
			return benchReadiness{}, fmt.Errorf("active-multi-pane thread IDs are not unique and non-empty")
		}
		want[id] = true
	}
	seenPane := make(map[string]bool, len(view.Panes))
	seenThread := make(map[string]bool, len(view.Panes))
	readiness := benchReadiness{At: time.Now().UTC()}
	for _, pane := range view.Panes {
		if pane.PaneID == "" || seenPane[pane.PaneID] {
			return benchReadiness{}, fmt.Errorf("active-multi-pane has duplicate or empty mounted pane ID %q", pane.PaneID)
		}
		if pane.Rect.W <= 0 || pane.Rect.H <= 0 {
			return benchReadiness{}, fmt.Errorf("active-multi-pane pane %s has no visible geometry (%gx%g)", pane.ThreadID, pane.Rect.W, pane.Rect.H)
		}
		seenPane[pane.PaneID] = true
		if !want[pane.ThreadID] || seenThread[pane.ThreadID] {
			return benchReadiness{}, fmt.Errorf("active-multi-pane pane %q has duplicate or unexpected thread %q", pane.PaneID, pane.ThreadID)
		}
		seenThread[pane.ThreadID] = true
		readiness.PaneIDs = append(readiness.PaneIDs, pane.PaneID)
	}
	for _, id := range threadIDs {
		if !seenThread[id] {
			return benchReadiness{}, fmt.Errorf("active-multi-pane thread %s is not mounted", id)
		}
	}
	active := make(map[string]bool, benchActiveStreamCount)
	for _, id := range threadIDs[:benchActiveStreamCount] {
		active[id] = true
		readiness.ActiveThreadIDs = append(readiness.ActiveThreadIDs, id)
	}
	for _, id := range threadIDs[benchActiveStreamCount:] {
		readiness.InactiveThreadIDs = append(readiness.InactiveThreadIDs, id)
	}
	for _, pane := range view.Panes {
		streaming := make([]uiRow, 0, 1)
		for _, row := range pane.Rows {
			if row.Status == "streaming" && row.Role == "assistant" {
				streaming = append(streaming, row)
			}
		}
		if active[pane.ThreadID] {
			if len(streaming) != 1 {
				return benchReadiness{}, fmt.Errorf("active thread %s has %d mounted streaming assistant rows, want exactly 1", pane.ThreadID, len(streaming))
			}
			row := streaming[0]
			if !row.InViewport || row.TextLength <= 0 || row.Rect.H <= 0 {
				return benchReadiness{}, fmt.Errorf("active thread %s streaming row is not visible with text", pane.ThreadID)
			}
			if row.Rect.Y < pane.Rect.Y || row.Rect.Y+row.Rect.H > pane.Rect.Y+pane.Rect.H {
				return benchReadiness{}, fmt.Errorf("active thread %s streaming row is clipped by pane bounds", pane.ThreadID)
			}
		} else if len(streaming) != 0 {
			return benchReadiness{}, fmt.Errorf("inactive thread %s has %d streaming assistant rows", pane.ThreadID, len(streaming))
		}
	}
	return readiness, nil
}

// validateActivePaneMount is the pre-arm half of the active barrier. The
// stream rows do not exist until the measured release sends the turns, but
// the six-pane geometry and settled document must already be exact.
func validateActivePaneMount(view uiViewport, threadIDs []string) error {
	if !view.Settled {
		return errors.New("active-multi-pane viewport is not settled")
	}
	if len(threadIDs) != benchActivePaneCount || len(view.Panes) != benchActivePaneCount {
		return fmt.Errorf("active-multi-pane mount has %d panes, want exactly %d", len(view.Panes), benchActivePaneCount)
	}
	want := make(map[string]bool, len(threadIDs))
	for _, id := range threadIDs {
		if id == "" || want[id] {
			return errors.New("active-multi-pane thread IDs are not unique and non-empty")
		}
		want[id] = true
	}
	seen := make(map[string]bool, len(view.Panes))
	for _, pane := range view.Panes {
		if pane.PaneID == "" || seen[pane.PaneID] || !want[pane.ThreadID] || pane.Rect.W <= 0 || pane.Rect.H <= 0 {
			return fmt.Errorf("active-multi-pane mount has invalid pane %q", pane.PaneID)
		}
		seen[pane.PaneID] = true
	}
	for _, id := range threadIDs {
		found := false
		for _, pane := range view.Panes {
			if pane.ThreadID == id {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("active-multi-pane thread %s is not mounted", id)
		}
	}
	return nil
}

func waitForActivePaneMount(ctx context.Context, e *env, client *harnessclient.Client, threadIDs []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	confirmations := 0
	var lastErr error
	for time.Now().Before(deadline) {
		probeCtx, cancel := context.WithDeadline(ctx, earlierDeadline(ctx, deadline))
		view, _, err := e.takeViewport(probeCtx, client, 0, 120)
		cancel()
		if err != nil {
			return fmt.Errorf("read active-multi-pane mount: %w", err)
		}
		if err := validateActivePaneMount(view, threadIDs); err != nil {
			lastErr = err
			confirmations = 0
		} else {
			confirmations++
			if confirmations >= benchReadinessConfirmations {
				return nil
			}
		}
		if err := sleepCtx(ctx, benchActivationPollInterval); err != nil {
			return err
		}
	}
	return fmt.Errorf("active-multi-pane mount barrier timed out after %s: %w", timeout, lastErr)
}

// waitForActivePaneReadiness polls one whole viewport at a time. A query
// context is bounded by both the caller and barrier deadlines, so a dead
// page cannot consume the remaining barrier budget one probe at a time.
func waitForActivePaneReadiness(ctx context.Context, e *env, client *harnessclient.Client, threadIDs []string, timeout time.Duration) (benchReadiness, error) {
	deadline := time.Now().Add(timeout)
	confirmations := 0
	var lastErr error
	for {
		if !time.Now().Before(deadline) {
			if lastErr == nil {
				lastErr = errors.New("no readiness sample")
			}
			return benchReadiness{}, fmt.Errorf("active-multi-pane readiness barrier timed out after %s: %w", timeout, lastErr)
		}
		probeCtx, cancel := context.WithDeadline(ctx, earlierDeadline(ctx, deadline))
		view, _, err := e.takeViewport(probeCtx, client, 0, 120)
		cancel()
		if err != nil {
			return benchReadiness{}, fmt.Errorf("read active-multi-pane readiness: %w", err)
		}
		ready, err := validateActivePaneReadiness(view, threadIDs)
		if err != nil {
			lastErr = err
			confirmations = 0
		} else {
			confirmations++
			if confirmations >= benchReadinessConfirmations {
				return ready, nil
			}
		}
		if err := sleepCtx(ctx, benchActivationPollInterval); err != nil {
			return benchReadiness{}, err
		}
	}
}

func earlierDeadline(ctx context.Context, deadline time.Time) time.Time {
	if parent, ok := ctx.Deadline(); ok && parent.Before(deadline) {
		return parent
	}
	return deadline
}

// benchProgressCadence is the temporal part of the DOM oracle. AtMs is a
// page-independent elapsed clock, which keeps tests deterministic and avoids
// mixing wall-clock probe latency into the reported cadence.
type benchProgressCadence struct {
	Samples             int   `json:"samples"`
	FirstAtMs           int64 `json:"firstAtMs"`
	LastAtMs            int64 `json:"lastAtMs"`
	MaxObservationGapMs int64 `json:"maxObservationGapMs"`
}

func validateBenchProgressCadence(samples []benchVisibleProgress, interval time.Duration) (benchProgressCadence, error) {
	if len(samples) == 0 {
		return benchProgressCadence{}, errors.New("active-multi-pane recorded no DOM progress samples")
	}
	if interval <= 0 {
		return benchProgressCadence{}, errors.New("active-multi-pane progress interval must be positive")
	}
	result := benchProgressCadence{Samples: len(samples), FirstAtMs: samples[0].AtMs, LastAtMs: samples[len(samples)-1].AtMs}
	for i := 1; i < len(samples); i++ {
		gap := samples[i].AtMs - samples[i-1].AtMs
		if gap <= 0 {
			return benchProgressCadence{}, fmt.Errorf("DOM progress timestamps are not increasing at sample %d", i)
		}
		if gap > result.MaxObservationGapMs {
			result.MaxObservationGapMs = gap
		}
		if time.Duration(gap)*time.Millisecond > benchProgressMaxGapFactor*interval {
			return benchProgressCadence{}, fmt.Errorf("DOM progress observation gap %dms exceeds %s", gap, benchProgressMaxGapFactor*interval)
		}
	}
	return result, nil
}

// queryBenchElementDeadlineSafe gives one collection a single deadline.
// Four per-pane probes each carrying an independent five-second timeout
// could otherwise turn a nominally bounded sample into a twenty-second one.
func queryBenchElementDeadlineSafe(ctx context.Context, run *benchRun, selector string, includeScroll bool) (benchElementAnswer, error) {
	queryCtx, cancel := context.WithTimeout(ctx, benchProbeTimeout)
	defer cancel()
	return queryBenchElementWithContext(queryCtx, run, selector, includeScroll)
}

func queryBenchElementWithContext(ctx context.Context, run *benchRun, selector string, includeScroll bool) (benchElementAnswer, error) {
	spec := map[string]any{"kind": "element", "selector": selector, "textCap": 1}
	if includeScroll {
		spec["includeScroll"] = true
	}
	raw, err := run.env.queryUI(ctx, run.client, spec)
	if err != nil {
		return benchElementAnswer{}, err
	}
	var answer benchElementAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return benchElementAnswer{}, fmt.Errorf("decode element query for %s: %w", selector, err)
	}
	return answer, nil
}

// benchEvidenceCursor is intentionally run-scoped. Health's persistent
// cursor would hide a fault emitted before a benchmark and would also make a
// repeated run depend on an unrelated health invocation.
type benchEvidenceCursor struct {
	Path  string           `json:"path,omitempty"`
	Start healthFileCursor `json:"start"`
}

type benchEvidenceCursors struct {
	FrontendErrors benchEvidenceCursor `json:"frontendErrors"`
	UITrace        benchEvidenceCursor `json:"uiTrace"`
}

func captureBenchEvidenceCursors(info harnessclient.HarnessInfo) benchEvidenceCursors {
	return benchEvidenceCursors{
		FrontendErrors: newBenchEvidenceCursor(info.FrontendErrorsPath),
		UITrace:        newBenchEvidenceCursor(info.UITracePath),
	}
}

func newBenchEvidenceCursor(path string) benchEvidenceCursor {
	cursor := benchEvidenceCursor{Path: path}
	if path == "" {
		return cursor
	}
	if info, err := os.Stat(path); err == nil {
		cursor.Start = healthFileCursor{Offset: info.Size(), Size: info.Size(), Ident: harnessclient.FileIdentity(info)}
	}
	return cursor
}

type benchFaultReceipt struct {
	FrontendErrors int            `json:"frontendErrors"`
	EngineNotices  int            `json:"engineNotices"`
	UIOracle       map[string]int `json:"uiOracle,omitempty"`
	Sample         []string       `json:"sample,omitempty"`
}

func collectBenchFaultReceipt(cursors benchEvidenceCursors) (benchFaultReceipt, error) {
	receipt, _, err := collectBenchFaultReceiptAt(cursors)
	return receipt, err
}

// collectBenchFaultReceiptAt advances both cursors. The simple wrapper above
// is enough for a bench's end-of-run fold, while this form lets a long run
// sample evidence more than once without counting the same line twice.
func collectBenchFaultReceiptAt(cursors benchEvidenceCursors) (benchFaultReceipt, benchEvidenceCursors, error) {
	result := benchFaultReceipt{UIOracle: map[string]int{}}
	if cursors.FrontendErrors.Path != "" {
		lines, next, _, err := scanNewLines(cursors.FrontendErrors.Path, cursors.FrontendErrors.Start)
		if err != nil {
			return benchFaultReceipt{}, cursors, fmt.Errorf("read run-scoped frontend errors: %w", err)
		}
		cursors.FrontendErrors.Start = next
		scan := scanFrontendErrors(lines)
		result.FrontendErrors, result.EngineNotices, result.Sample = scan.Faults, scan.Notices, scan.Sample
	}
	if cursors.UITrace.Path != "" {
		lines, next, _, err := scanNewLines(cursors.UITrace.Path, cursors.UITrace.Start)
		if err != nil {
			return benchFaultReceipt{}, cursors, fmt.Errorf("read run-scoped UI trace: %w", err)
		}
		cursors.UITrace.Start = next
		result.UIOracle = countOracleTriggers(lines)
	}
	return result, cursors, nil
}

type benchSourceReceipt struct {
	Channel        string `json:"channel"`
	Expected       int    `json:"expected"`
	Completions    int    `json:"completions"`
	ObservedEvents int    `json:"observedEvents"`
}

func sourceReceipt(client *harnessclient.Client, expected int) benchSourceReceipt {
	channel := string(eventchan.ProviderTurnCompleted)
	completion := func(ev harnessclient.Event) bool {
		if ev.Gap {
			return false
		}
		var payload struct {
			ThreadID string `json:"threadId"`
		}
		return json.Unmarshal(ev.Data, &payload) == nil && payload.ThreadID != ""
	}
	return benchSourceReceipt{
		Channel: channel, Expected: expected,
		Completions:    client.Count(channel, completion),
		ObservedEvents: client.Count(channel, nil),
	}
}

type benchOverlapReceipt struct {
	SourceStartedAt time.Time `json:"sourceStartedAt,omitempty"`
	DOMFirstAtMs    int64     `json:"domFirstAtMs,omitempty"`
	DOMLastAtMs     int64     `json:"domLastAtMs,omitempty"`
	Overlapped      bool      `json:"overlapped"`
}

func validateBenchOverlap(sourceAt time.Time, progress []benchVisibleProgress) (benchOverlapReceipt, error) {
	if sourceAt.IsZero() || len(progress) == 0 {
		return benchOverlapReceipt{}, errors.New("source and DOM progress do not overlap")
	}
	result := benchOverlapReceipt{SourceStartedAt: sourceAt, DOMFirstAtMs: progress[0].AtMs, DOMLastAtMs: progress[len(progress)-1].AtMs, Overlapped: true}
	return result, nil
}

// benchValidityReceipt is the machine-readable gate parent report assembly
// can embed without knowing how each oracle was collected.
type benchValidityReceipt struct {
	V              int                           `json:"v"`
	Valid          bool                          `json:"valid"`
	Reasons        []string                      `json:"reasons,omitempty"`
	Source         benchSourceReceipt            `json:"source"`
	DOM            benchProgressCadence          `json:"dom"`
	Gaps           []harnessclient.SequenceGap   `json:"gaps,omitempty"`
	SequenceFaults []harnessclient.SequenceFault `json:"sequenceFaults,omitempty"`
	Faults         benchFaultReceipt             `json:"faults"`
	Drain          revealDrain                   `json:"drain"`
	DrainObserved  bool                          `json:"drainObserved"`
	Overlap        benchOverlapReceipt           `json:"overlap"`
}

// finalizeBenchValidity stores the receipt before returning its gate error,
// so failed runs retain the evidence collected before the failure.
func finalizeBenchValidity(run *benchRun) error {
	if run == nil {
		return errors.New("finalize benchmark validity: nil run")
	}
	receipt, err := buildBenchValidityReceipt(
		run.client, len(run.startedThreadIDs), run.progress, run.progressInterval,
		run.sourceStartedAt, run.evidence, run.drain, run.drainObserved,
	)
	if err != nil {
		run.validity = benchValidityReceipt{V: 1, Reasons: []string{err.Error()}, Drain: run.drain, DrainObserved: run.drainObserved}
		return fmt.Errorf("build benchmark validity receipt: %w", err)
	}
	run.validity = receipt
	if !receipt.Valid {
		return fmt.Errorf("benchmark validity failed: %s", strings.Join(receipt.Reasons, "; "))
	}
	return nil
}

func invalidateBenchRun(run *benchRun, phase string, err error) {
	if run == nil || err == nil {
		return
	}
	run.validity.Valid = false
	run.validity.Reasons = append(run.validity.Reasons, fmt.Sprintf("%s: %v", phase, err))
}

// buildBenchValidityReceipt folds the independent source, DOM, file and
// drain observations into one gate. It is deliberately pure with respect to
// the report. A caller can attach it to a run row or preserve it in a
// partial report when drive/stop fails.
func buildBenchValidityReceipt(
	client *harnessclient.Client,
	expectedCompletions int,
	progress []benchVisibleProgress,
	progressInterval time.Duration,
	sourceStartedAt time.Time,
	cursors benchEvidenceCursors,
	drain revealDrain,
	drainObserved bool,
) (benchValidityReceipt, error) {
	if client == nil {
		return benchValidityReceipt{}, errors.New("build benchmark validity receipt: nil harness client")
	}
	receipt := benchValidityReceipt{V: 1, Drain: drain, DrainObserved: drainObserved}
	receipt.Source = sourceReceipt(client, expectedCompletions)
	receipt.Gaps = client.SequenceGaps()
	receipt.SequenceFaults = client.SequenceFaults()
	if progressInterval > 0 {
		if cadence, err := validateBenchProgressCadence(progress, progressInterval); err != nil {
			receipt.Reasons = append(receipt.Reasons, err.Error())
		} else {
			receipt.DOM = cadence
		}
		if overlap, err := validateBenchOverlap(sourceStartedAt, progress); err != nil {
			receipt.Reasons = append(receipt.Reasons, err.Error())
		} else {
			receipt.Overlap = overlap
		}
	}
	faults, err := collectBenchFaultReceipt(cursors)
	if err != nil {
		return receipt, err
	}
	receipt.Faults = faults
	if receipt.Source.Completions != expectedCompletions {
		receipt.Reasons = append(receipt.Reasons, fmt.Sprintf("source completed %d turns, want %d", receipt.Source.Completions, expectedCompletions))
	}
	if len(receipt.Gaps) > 0 {
		receipt.Reasons = append(receipt.Reasons, fmt.Sprintf("source event sequence has %d gap(s)", len(receipt.Gaps)))
	}
	if len(receipt.SequenceFaults) > 0 {
		receipt.Reasons = append(receipt.Reasons, fmt.Sprintf("source event sequence has %d duplicate or rewind fault(s)", len(receipt.SequenceFaults)))
	}
	if receipt.Faults.FrontendErrors > 0 {
		receipt.Reasons = append(receipt.Reasons, fmt.Sprintf("frontend emitted %d uncaught error(s)", receipt.Faults.FrontendErrors))
	}
	for label, count := range receipt.Faults.UIOracle {
		if count > 0 {
			receipt.Reasons = append(receipt.Reasons, fmt.Sprintf("UI oracle %s fired %d time(s)", label, count))
		}
	}
	if !drain.empty() {
		receipt.Reasons = append(receipt.Reasons, fmt.Sprintf("reveal drain remains active (%d smoother(s), %d boundary owner(s))", drain.Smoothers, drain.Boundaries))
	}
	if !drainObserved {
		receipt.Reasons = append(receipt.Reasons, "reveal drain was not observed")
	}
	receipt.Valid = len(receipt.Reasons) == 0
	return receipt, nil
}
