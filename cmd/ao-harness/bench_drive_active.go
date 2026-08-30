package main

// The active multi-pane driver keeps its long-running progress and cleanup
// loop separate from the short, generic bench drivers.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harnessclient"
)

type benchTurnCompletion struct {
	threadID string
	at       time.Time
	err      error
}

type benchElementAnswer struct {
	Count int `json:"count"`
	First *struct {
		Visible    bool `json:"visible"`
		TextLength int  `json:"textLength"`
		Scroll     *struct {
			Height int `json:"height"`
		} `json:"scroll,omitempty"`
	} `json:"first"`
}

// prepareActiveMultiPaneStream mounts and settles the exact normal-use shape
// before instrumentation. Streaming is released by the measured drive after
// the perf arm, so setup and readiness probes cannot enter the run.
func prepareActiveMultiPaneStream(ctx context.Context, run *benchRun) error {
	if err := openPanesForMultiPaneStream(ctx, run); err != nil {
		return err
	}
	if run.duration < benchActiveMinimumDuration {
		return fmt.Errorf("active-multi-pane duration %s is below the %s minimum", run.duration, benchActiveMinimumDuration)
	}
	if len(run.threadIDs) != benchActivePaneCount {
		return fmt.Errorf("active-multi-pane opened %d threads, want %d", len(run.threadIDs), benchActivePaneCount)
	}
	run.activeIDs = append([]string(nil), run.threadIDs[:benchActiveStreamCount]...)
	if err := waitForActivePaneMount(ctx, run.env, run.client, run.threadIDs, benchBridgeTimeout); err != nil {
		return err
	}
	return nil
}

// startActiveMultiPaneStream releases the exact normal-use shape under load:
// six panes stay mounted, four start rich-Markdown turns back to back, and
// the provider keeps those turns open until the requested duration expires.
// Low-frequency DOM readings prove each active pane continued to paint new
// text throughout the window. A timer alone would call a wedged provider a
// sustained workload and repeat the invalid short-soak result this replaces.
func startActiveMultiPaneStream(ctx context.Context, run *benchRun) error {
	if len(run.activeIDs) != benchActiveStreamCount {
		return errors.New("active-multi-pane stream was not prepared before release")
	}

	waitCtx, cancelWait := context.WithCancel(ctx)
	run.activeWaitCancel = cancelWait
	run.activeCompletions = make(chan benchTurnCompletion, len(run.activeIDs))
	for _, threadID := range run.activeIDs {
		waiting := run.client.Await(string(eventchan.ProviderTurnCompleted), matchTurnCompletion(threadID))
		go func(id string, awaiting *harnessclient.Awaiting) {
			_, err := awaiting.Wait(waitCtx)
			run.activeCompletions <- benchTurnCompletion{threadID: id, at: time.Now(), err: err}
		}(threadID, waiting)
	}

	sendCtx, cancelSend := context.WithTimeout(ctx, benchTurnTimeout)
	type sendResult struct {
		threadID string
		err      error
	}
	results := make(chan sendResult, len(run.activeIDs))
	var sends sync.WaitGroup
	for i, threadID := range run.activeIDs {
		// Once the frame is attempted, cleanup owns this thread. A transport
		// timeout does not prove the backend failed to receive the request.
		run.startedThreadIDs = append(run.startedThreadIDs, threadID)
		sends.Add(1)
		go func(id string, pane int) {
			defer sends.Done()
			_, err := run.client.Call(sendCtx, "SendMessage", id,
				fmt.Sprintf("bench %s run %d active pane %d", run.workload.Name, run.index, pane), nil)
			results <- sendResult{threadID: id, err: err}
		}(threadID, i+1)
	}
	sends.Wait()
	cancelSend()
	close(results)
	var sendErrs []error
	for result := range results {
		if result.err != nil {
			sendErrs = append(sendErrs, fmt.Errorf("start active turn in thread %s: %w", result.threadID, result.err))
		}
	}
	if err := errors.Join(sendErrs...); err != nil {
		return err
	}

	return nil
}

func driveActiveMultiPaneStream(ctx context.Context, run *benchRun) (returnErr error) {
	if run.activeCompletions == nil {
		if err := startActiveMultiPaneStream(ctx, run); err != nil {
			return err
		}
	}
	activeIDs := run.activeIDs
	completions := run.activeCompletions
	cleanupNeeded := true
	defer func() {
		if cleanupNeeded && len(run.startedThreadIDs) > 0 {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), benchInterruptTimeout)
			cleanupErr := interruptBenchTurns(cleanupCtx, run.client, run.startedThreadIDs)
			cancel()
			if cleanupErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("clean up active bench turns: %w", cleanupErr))
			}
		}
		if run.activeWaitCancel != nil {
			run.activeWaitCancel()
			run.activeWaitCancel = nil
		}
	}()

	startedAt := time.Now()
	interval := activeBenchProgressInterval(run.duration)
	run.sourceStartedAt = startedAt
	run.progressInterval = interval
	deadline := time.NewTimer(run.duration)
	defer deadline.Stop()
	progressTick := time.NewTicker(interval)
	defer progressTick.Stop()
	var previous *benchVisibleProgress
	var first *benchVisibleProgress
	lastSampleAt := time.Time{}

measure:
	for {
		select {
		case completion := <-completions:
			if completion.err != nil {
				return fmt.Errorf("watch active thread %s: %w", completion.threadID, completion.err)
			}
			return fmt.Errorf("active thread %s completed after %s, before the requested %s",
				completion.threadID, completion.at.Sub(startedAt).Round(time.Millisecond), run.duration)
		case <-progressTick.C:
			sample, err := collectActiveBenchProgress(ctx, run, activeIDs, startedAt)
			if err != nil {
				return err
			}
			if previous != nil {
				if err := validateActiveTextGrowth(*previous, sample, activeIDs); err != nil {
					return err
				}
			} else {
				copy := sample
				first = &copy
			}
			run.progress = append(run.progress, sample)
			copy := sample
			previous = &copy
			lastSampleAt = time.Now()
		case <-deadline.C:
			break measure
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Capture the end only when it is meaningfully later than the last tick.
	// Timer and ticker can become runnable together at an exact multiple. A
	// duplicate sample would fail the strict-growth assertion for no product
	// reason and make scheduling order part of the benchmark result.
	if lastSampleAt.IsZero() || time.Since(lastSampleAt) >= interval/2 {
		sample, err := collectActiveBenchProgress(ctx, run, activeIDs, startedAt)
		if err != nil {
			return err
		}
		if previous != nil {
			if err := validateActiveTextGrowth(*previous, sample, activeIDs); err != nil {
				return err
			}
		} else {
			copy := sample
			first = &copy
		}
		run.progress = append(run.progress, sample)
		copy := sample
		previous = &copy
	}
	if len(run.progress) < 3 || first == nil || previous == nil {
		return fmt.Errorf("active-multi-pane recorded %d progress samples, want at least 3", len(run.progress))
	}
	if _, err := validateBenchProgressCadence(run.progress, interval); err != nil {
		return err
	}
	if err := validateActiveScrollGrowth(*first, *previous, activeIDs); err != nil {
		return err
	}

	interruptAt := time.Now()
	interruptCtx, cancelInterrupt := context.WithTimeout(ctx, benchInterruptTimeout)
	err := interruptBenchTurns(interruptCtx, run.client, activeIDs)
	cancelInterrupt()
	if err != nil {
		return err
	}
	cleanupNeeded = false

	completionCtx, cancelCompletion := context.WithTimeout(ctx, benchTurnTimeout)
	defer cancelCompletion()
	seen := make(map[string]bool, len(activeIDs))
	for len(seen) < len(activeIDs) {
		select {
		case completion := <-completions:
			if completion.err != nil {
				return fmt.Errorf("wait for interrupted thread %s: %w", completion.threadID, completion.err)
			}
			if completion.at.Before(interruptAt) {
				return fmt.Errorf("active thread %s completed before the benchmark interrupted it", completion.threadID)
			}
			if seen[completion.threadID] {
				return fmt.Errorf("active thread %s emitted two completion events", completion.threadID)
			}
			seen[completion.threadID] = true
		case <-completionCtx.Done():
			return fmt.Errorf("wait for %d interrupted active turns: %w", len(activeIDs)-len(seen), completionCtx.Err())
		}
	}

	if err := waitForRunRevealDrain(ctx, run, benchDrainTimeout); err != nil {
		return err
	}
	return sleepCtx(ctx, benchSettleMs*time.Millisecond)
}

func activeBenchProgressInterval(duration time.Duration) time.Duration {
	interval := duration / 4
	if interval > 30*time.Second {
		return 30 * time.Second
	}
	return interval
}

func collectActiveBenchProgress(
	ctx context.Context,
	run *benchRun,
	threadIDs []string,
	startedAt time.Time,
) (benchVisibleProgress, error) {
	// One deadline covers the complete observation. If every element query
	// owns an independent five-second timeout, a stalled page can turn a
	// single sample into four times the cadence budget and hide the gap.
	queryCtx, cancel := context.WithTimeout(ctx, benchProbeTimeout)
	defer cancel()
	sample := benchVisibleProgress{
		AtMs:        time.Since(startedAt).Milliseconds(),
		TextLengths: make(map[string]int, len(threadIDs)),
		ScrollPx:    make(map[string]int, len(threadIDs)),
	}
	for _, threadID := range threadIDs {
		assistantSelector, err := activeBenchSelector(threadID,
			`[data-item-role="assistant"][data-item-status="streaming"]`)
		if err != nil {
			return benchVisibleProgress{}, err
		}
		assistant, err := queryBenchElementWithContext(queryCtx, run, assistantSelector, false)
		if err != nil {
			return benchVisibleProgress{}, fmt.Errorf("read active assistant in thread %s: %w", threadID, err)
		}
		if assistant.Count != 1 || assistant.First == nil || !assistant.First.Visible {
			return benchVisibleProgress{}, fmt.Errorf(
				"thread %s has %d visible streaming assistant rows, want exactly 1", threadID, assistant.Count)
		}
		if assistant.First.TextLength <= 0 {
			return benchVisibleProgress{}, fmt.Errorf("thread %s streaming assistant has no rendered text", threadID)
		}
		sample.TextLengths[threadID] = assistant.First.TextLength

		scrollerSelector, err := activeBenchSelector(threadID, `[data-testid="message-timeline-scroll"]`)
		if err != nil {
			return benchVisibleProgress{}, err
		}
		scroller, err := queryBenchElementWithContext(queryCtx, run, scrollerSelector, true)
		if err != nil {
			return benchVisibleProgress{}, fmt.Errorf("read timeline scroller in thread %s: %w", threadID, err)
		}
		if scroller.Count != 1 || scroller.First == nil || !scroller.First.Visible || scroller.First.Scroll == nil {
			return benchVisibleProgress{}, fmt.Errorf(
				"thread %s has %d visible timeline scrollers with geometry, want exactly 1", threadID, scroller.Count)
		}
		sample.ScrollPx[threadID] = scroller.First.Scroll.Height
	}
	return sample, nil
}

func queryBenchElement(ctx context.Context, run *benchRun, selector string, includeScroll bool) (benchElementAnswer, error) {
	return queryBenchElementDeadlineSafe(ctx, run, selector, includeScroll)
}

func activeBenchSelector(threadID, descendant string) (string, error) {
	if !benchThreadIDPattern.MatchString(threadID) {
		return "", fmt.Errorf("thread id %q cannot be embedded in a harness selector", threadID)
	}
	return fmt.Sprintf(`[data-ui-surface="chat"][data-thread-id="%s"] %s`, threadID, descendant), nil
}

func validateActiveTextGrowth(previous, current benchVisibleProgress, threadIDs []string) error {
	for _, threadID := range threadIDs {
		before, beforeOK := previous.TextLengths[threadID]
		after, afterOK := current.TextLengths[threadID]
		if !beforeOK || !afterOK {
			return fmt.Errorf("visible-progress sample omitted active thread %s", threadID)
		}
		if after <= before {
			return fmt.Errorf("active thread %s rendered text did not grow between %dms and %dms (%d to %d UTF-16 units)",
				threadID, previous.AtMs, current.AtMs, before, after)
		}
	}
	return nil
}

func validateActiveScrollGrowth(first, last benchVisibleProgress, threadIDs []string) error {
	for _, threadID := range threadIDs {
		before, beforeOK := first.ScrollPx[threadID]
		after, afterOK := last.ScrollPx[threadID]
		if !beforeOK || !afterOK {
			return fmt.Errorf("scroll-progress sample omitted active thread %s", threadID)
		}
		if after <= before {
			return fmt.Errorf("active thread %s timeline did not grow during the workload (%d to %d px)",
				threadID, before, after)
		}
	}
	return nil
}

func interruptBenchTurns(ctx context.Context, client *harnessclient.Client, threadIDs []string) error {
	type result struct {
		threadID string
		err      error
	}
	results := make(chan result, len(threadIDs))
	for _, threadID := range threadIDs {
		go func(id string) {
			_, err := client.Call(ctx, "InterruptTurn", id)
			results <- result{threadID: id, err: err}
		}(threadID)
	}
	var errs []error
	for range threadIDs {
		result := <-results
		if result.err != nil {
			errs = append(errs, fmt.Errorf("interrupt thread %s: %w", result.threadID, result.err))
		}
	}
	return errors.Join(errs...)
}

// sleepCtx is the only wait shape in this file. A bare time.Sleep would
// keep a cancelled bench pacing a workload against an instance nobody is
// waiting on any more — a Ctrl-C during a settle would sit there for a
// second and change the numbers of a run that is already abandoned.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
