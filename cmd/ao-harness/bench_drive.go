package main

// The bench drivers: everything that happens between arming the meters and
// stopping them, plus the two page-side preconditions a bench depends on.
// Split from the runner so cmd_bench.go stays about the run sequence and
// this file stays about what a workload DOES to the app.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harnessclient"
)

const (
	// benchProbeTimeout bounds ONE bridge probe, inside the longer
	// benchBridgeTimeout budget a poll loop spends retrying them. A page
	// that has not answered in five seconds is not mid-render; it is gone,
	// and the loop's next attempt is the useful move.
	benchProbeTimeout = 5 * time.Second
	// benchReloadPollInterval is how long the reload loop waits between
	// probes. Long enough that a navigating page is not hammered with
	// queries it will drop, short enough that a fast reload is not paid for.
	benchReloadPollInterval = 250 * time.Millisecond
	// benchActivationPollInterval paces the wait for a thread open. The page
	// answers a viewport query in single-digit milliseconds, so this is
	// about giving the mount a beat rather than about query cost.
	benchActivationPollInterval = 150 * time.Millisecond
	// benchDrainTimeout bounds the wait for the reveal queue to empty after
	// a turn closes. Generous on purpose: the mock finishes a burst-stream
	// turn in about a second and the reveal drain runs for ten or more, and
	// a workload three times that size is still a legitimate run rather
	// than a wedged one. Past this the measurement is incomplete and fails.
	benchDrainTimeout = 60 * time.Second
	// benchDrainPollInterval paces the drain probe. The answer is three
	// integers off store state, so this is about not becoming part of the
	// load being measured rather than about query cost.
	benchDrainPollInterval = 250 * time.Millisecond
	// benchDrainConfirmations is how many consecutive empty readings end
	// the wait. Two, because a drain empties BETWEEN rows: the last
	// smoother of one item is disposed a frame before the next item's is
	// created, and a single empty reading taken in that gap would close the
	// window in the middle of the stream.
	benchDrainConfirmations = 2
	// A clean interrupt should return as soon as the mock acknowledges it.
	// This bound exists for the failure path, where every started pane still
	// has to receive an interrupt even if one provider is wedged.
	benchInterruptTimeout = 20 * time.Second
	// revealDrainGlobal is the page-side probe the drain wait polls. It is
	// installed by main.ts in every build (unlike the UI_TRACE diagnostics
	// globals) and whitelisted in frontend/src/lib/harness/globals.ts.
	revealDrainGlobal = "__aoRevealDrain"
)

var benchThreadIDPattern = regexp.MustCompile(`\A[A-Za-z0-9_-]+\z`)

// benchSubscribedChannels is what the bench connection narrows itself to.
// A bench is an INSTRUMENT: burst-stream and giant-turn push thousands of
// item deltas, and a CLI still holding the default all-channel
// subscription makes the backend serialise every one of them onto a
// second socket during the exact window the numbers are taken from.
// Only the completion signal the drivers await is needed here; a workload
// that awaits nothing (many-threads polls the page instead) is happy with
// the same narrow set.
func benchSubscribedChannels() []string {
	return []string{string(eventchan.ProviderTurnCompleted)}
}

func narrowBenchSubscription(ctx context.Context, client *harnessclient.Client) error {
	if err := client.Subscribe(ctx, benchSubscribedChannels()...); err != nil {
		return fmt.Errorf("narrow the bench connection to %v: %w", benchSubscribedChannels(), err)
	}
	return nil
}

// probeBridge answers "is a page attached" once, up front, with the error
// that names the fix.
func probeBridge(ctx context.Context, e *env, client *harnessclient.Client) error {
	probeCtx, cancel := context.WithTimeout(ctx, benchBridgeTimeout)
	defer cancel()
	if _, err := e.queryUI(probeCtx, client, map[string]any{"kind": "element", "selector": "body"}); err != nil {
		return err
	}
	return nil
}

// reloadPage navigates the attached page and waits for its bridge to come
// back. The reload query's OWN answer is best-effort by design: the page
// is about to drop the socket the reply rides on, so a failure here proves
// nothing either way and the re-probe is what decides.
func reloadPage(ctx context.Context, e *env, client *harnessclient.Client) error {
	reloadCtx, cancel := context.WithTimeout(ctx, benchBridgeTimeout)
	defer cancel()
	_, _ = e.queryUI(reloadCtx, client, map[string]any{"kind": "reload"})

	deadline := time.Now().Add(benchBridgeTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := sleepCtx(ctx, benchReloadPollInterval); err != nil {
			return err
		}
		probeCtx, probeCancel := context.WithTimeout(ctx, benchProbeTimeout)
		_, err := e.queryUI(probeCtx, client, map[string]any{"kind": "element", "selector": "body"})
		probeCancel()
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("the page did not come back after a reload: %w", lastErr)
}

// activateThread drives the production thread-open path from outside the
// browser. `notification:activated` is the channel an OS notification
// click rides; the SPA parses the target, resolves the thread, and calls
// `openThreadInPane`, the same function a sidebar click reaches. What it
// does NOT exercise is the sidebar row itself (hit-testing, hover), which
// is why a bench built on it measures timeline mounting rather than
// sidebar interaction.
//
// Both the single-thread open and the switch storm go through here, so
// the two never drift into emitting subtly different payloads — and so
// does `ui open`, which is the same move typed by a person.
func activateThread(ctx context.Context, client *harnessclient.Client, threadID string) error {
	payload, err := json.Marshal(map[string]any{"kind": "thread", "threadId": threadID})
	if err != nil {
		return err
	}
	_, err = client.Call(ctx, "HarnessEmit",
		string(eventchan.NotificationActivated), json.RawMessage(payload))
	return err
}

// openThreadOnPage activates a thread and waits for the page to show it.
func openThreadOnPage(ctx context.Context, e *env, client *harnessclient.Client, threadID string) error {
	if err := activateThread(ctx, client, threadID); err != nil {
		return err
	}
	return waitForActiveThread(ctx, e, client, threadID)
}

// openThreadInNewPaneOnPage drives the app's OWN new-pane door from outside
// the browser and waits for the pane to appear.
//
// The plain open above rides `notification:activated`, an event channel the
// backend already publishes, so it exercises a production path end to end
// with no bridge involved. The new-pane door has no such channel: it is
// reached in-page only (ctrl-click on a sidebar row, the thread context
// menu, a builtin command), and reimplementing pane minting out here would
// measure a pane nobody ships. So this asks the bridge to call the same
// `openThreadInNewPane` those three gestures call.
func openThreadInNewPaneOnPage(ctx context.Context, e *env, client *harnessclient.Client, threadID string) error {
	openCtx, cancel := context.WithTimeout(ctx, benchBridgeTimeout)
	_, err := e.queryUI(openCtx, client, map[string]any{
		"kind": "open", "threadId": threadID, "newPane": true,
	})
	cancel()
	if err != nil {
		return err
	}
	return waitForActiveThread(ctx, e, client, threadID)
}

// waitForActiveThread polls until the thread is mounted in SOME pane. Pane
// membership, not the focused pane: a new-pane open and a thread-switch
// open both land here, and only one of them moves the active thread.
func waitForActiveThread(ctx context.Context, e *env, client *harnessclient.Client, threadID string) error {
	deadline := time.Now().Add(benchBridgeTimeout)
	var last string
	for time.Now().Before(deadline) {
		probeCtx, cancel := context.WithTimeout(ctx, benchProbeTimeout)
		view, _, err := e.takeViewport(probeCtx, client, 0, 0)
		cancel()
		if err != nil {
			return err
		}
		last = view.ActiveThreadID
		for _, pane := range view.Panes {
			if pane.ThreadID == threadID {
				return nil
			}
		}
		if err := sleepCtx(ctx, benchActivationPollInterval); err != nil {
			return err
		}
	}
	return fmt.Errorf("the page never opened thread %s (active thread is %q)", threadID, last)
}

// revealDrain is one reading of the page's reveal-queue drain.
type revealDrain struct {
	Panes      int `json:"panes"`
	Draining   int `json:"draining"`
	Smoothers  int `json:"smoothers"`
	Boundaries int `json:"boundaries"`
}

func (d revealDrain) empty() bool {
	return d.Draining == 0 && d.Smoothers == 0
}

// waitForRevealDrain blocks until the page has finished REVEALING what the
// turn produced, which is a later moment than `provider:turn_completed` and
// is the one a reader experiences.
//
// WHY THE MEASUREMENT WINDOW HAD TO MOVE. The wire closes a turn when the
// provider stops writing; the reveal queue then hands the text to the
// reader at a readable rate, and under the mock providers that outlives the
// turn by an order of magnitude. Every number a bench took therefore
// covered the flood and excluded the drain — the half of a stream a human
// spends the most time watching.
//
// WHY NOT THE MUTATION CLOCK. The bridge's settledness observer is
// force-disarmed at perf start and stop precisely so a run measures a
// renderer with no document-wide observer on it. Re-arming one to detect
// quiet would perturb the experiment it is timing, so the signal is cheap
// store state instead: live smoothers and standing reveal boundaries, per
// pane.
//
// Every missing signal is a failed measurement. This command drives the
// current attached build, not an older compatibility target. Ending at wire
// completion after a bridge error or timeout would produce a green report
// that silently excluded the main-thread reveal work it claims to measure.
func waitForRevealDrain(ctx context.Context, e *env, client *harnessclient.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	confirmations := 0
	var last revealDrain
	for {
		probeCtx, cancel := context.WithTimeout(ctx, benchProbeTimeout)
		raw, err := e.queryUI(probeCtx, client, map[string]any{
			"kind": "globals", "name": revealDrainGlobal,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("query reveal drain through %s: %w", revealDrainGlobal, err)
		}
		var answer struct {
			Unavailable bool        `json:"unavailable"`
			Value       revealDrain `json:"value"`
		}
		if err := json.Unmarshal(raw, &answer); err != nil {
			return fmt.Errorf("decode reveal drain from %s: %w", revealDrainGlobal, err)
		}
		if answer.Unavailable {
			return fmt.Errorf("the attached page does not install %s", revealDrainGlobal)
		}
		last = answer.Value
		if last.empty() {
			confirmations++
			if confirmations >= benchDrainConfirmations {
				return nil
			}
		} else {
			confirmations = 0
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf(
				"reveal queue still draining after %s (%d pane(s), %d smoother(s), %d boundary owner(s))",
				timeout, last.Draining, last.Smoothers, last.Boundaries,
			)
		}
		if err := sleepCtx(ctx, benchDrainPollInterval); err != nil {
			return err
		}
	}
}

// awaitTurnCompletion parks on `provider:turn_completed` for one thread.
// Split out because the multi-pane workload waits on several of them and
// must not reimplement the payload match.
func awaitTurnCompletion(ctx context.Context, client *harnessclient.Client, threadID string) error {
	channel := string(eventchan.ProviderTurnCompleted)
	if _, err := client.WaitForEvent(ctx, channel, matchTurnCompletion(threadID)); err != nil {
		return fmt.Errorf("the turn never completed: %w", err)
	}
	return nil
}

func matchTurnCompletion(threadID string) func(harnessclient.Event) bool {
	return func(ev harnessclient.Event) bool {
		if ev.Gap {
			return false
		}
		var payload struct {
			ThreadID string `json:"threadId"`
		}
		return json.Unmarshal(ev.Data, &payload) == nil && payload.ThreadID == threadID
	}
}

// driveOneTurn sends the workload's message, waits for the app to close the
// turn, waits for the reveal queue to hand the result to the reader, then
// lets the meters run one more settle window.
func driveOneTurn(ctx context.Context, run *benchRun) error {
	threadID := run.threadIDs[0]
	// The turn keeps its own budget. The drain that follows is bounded
	// separately, so folding the two into one deadline would let a slow drain
	// report itself as a turn that never completed instead of an incomplete
	// renderer measurement.
	turnCtx, cancel := context.WithTimeout(ctx, benchTurnTimeout)
	if _, err := run.client.Call(turnCtx, "SendMessage", threadID,
		fmt.Sprintf("bench %s run %d", run.workload.Name, run.index), nil); err != nil {
		cancel()
		return err
	}
	err := awaitTurnCompletion(turnCtx, run.client, threadID)
	cancel()
	if err != nil {
		return err
	}
	if err := waitForRevealDrain(ctx, run.env, run.client, benchDrainTimeout); err != nil {
		return err
	}
	return sleepCtx(ctx, benchSettleMs*time.Millisecond)
}

// driveThreadSwitchStorm cycles the pane through every seeded thread, so
// each switch is a real timeline unmount plus a real bounded-window load
// out of SQLite. The dwell is what makes it a storm rather than a queue:
// long enough for the mount to finish and paint, short enough that the
// pane is never idle.
func driveThreadSwitchStorm(ctx context.Context, run *benchRun) error {
	const (
		passes = 2
		dwell  = 220 * time.Millisecond
	)
	stormCtx, cancel := context.WithTimeout(ctx, benchTurnTimeout)
	defer cancel()
	for pass := 0; pass < passes; pass++ {
		for _, threadID := range run.threadIDs {
			if err := activateThread(stormCtx, run.client, threadID); err != nil {
				return err
			}
			run.switches++
			if err := sleepCtx(stormCtx, dwell); err != nil {
				return err
			}
		}
	}
	// The storm is only honest if the switches actually landed, so the
	// final thread is confirmed on the page rather than assumed.
	if err := waitForActiveThread(stormCtx, run.env, run.client, run.threadIDs[len(run.threadIDs)-1]); err != nil {
		return err
	}
	// A switch storm streams nothing, so this normally returns on its first
	// reading. It runs anyway, uniformly with every other workload: a
	// switch INTO a thread whose last turn was still revealing is exactly
	// the case where the assumption would be wrong.
	if err := waitForRevealDrain(stormCtx, run.env, run.client, benchDrainTimeout); err != nil {
		return err
	}
	return sleepCtx(stormCtx, benchSettleMs*time.Millisecond)
}

// openPanesForMultiPaneStream is the multi-pane workload's PREPARE step:
// every seeded thread gets its own pane before the meters are armed.
//
// It runs outside the measured window on purpose. Mounting three timelines
// is a cost this workload is not about — the question is what CONCURRENT
// streaming into three live panes costs — and a mount folded into the
// window would dominate the first second of every repeat.
//
// The first thread is already open (executeBenchRun opens threadIDs[0] the
// same way every workload does, over `notification:activated`). The rest go
// through the bridge's `open` kind with `newPane`, which calls the app's
// own `openThreadInNewPane` — the function a ctrl-click on a sidebar row
// reaches. There is no event channel for that door, which is why this is
// the one page move a bench makes through the bridge rather than the wire.
func openPanesForMultiPaneStream(ctx context.Context, run *benchRun) error {
	for _, threadID := range run.threadIDs[1:] {
		if err := openThreadInNewPaneOnPage(ctx, run.env, run.client, threadID); err != nil {
			return fmt.Errorf("open thread %s in a new pane: %w", threadID, err)
		}
	}
	return nil
}

// driveMultiPaneStream starts every seeded thread's turn back to back and
// then waits for all of them.
//
// BACK TO BACK, NOT ONE AT A TIME. The workload exists because a single
// streaming pane is not the shape a heavy session takes: three panes
// revealing at once share one main thread, one style/layout pass and one
// scroll frame budget, and the per-pane work that looks free in isolation
// is what saturates a tick here. Sending serially would measure three
// sequential turns.
func driveMultiPaneStream(ctx context.Context, run *benchRun) error {
	turnCtx, cancel := context.WithTimeout(ctx, benchTurnTimeout)
	for i, threadID := range run.threadIDs {
		if _, err := run.client.Call(turnCtx, "SendMessage", threadID,
			fmt.Sprintf("bench %s run %d pane %d", run.workload.Name, run.index, i+1), nil); err != nil {
			cancel()
			return err
		}
	}
	// Waited per thread rather than by counting completions: N events on
	// the channel could be one thread completing N times if a workload ever
	// grew a second turn, and the wait would pass having measured a third
	// of what it claims. The client's log keeps events that arrived during
	// an earlier wait, so the order these are asked for does not matter.
	for _, threadID := range run.threadIDs {
		if err := awaitTurnCompletion(turnCtx, run.client, threadID); err != nil {
			cancel()
			return fmt.Errorf("thread %s: %w", threadID, err)
		}
	}
	cancel()
	if err := waitForRevealDrain(ctx, run.env, run.client, benchDrainTimeout); err != nil {
		return err
	}
	return sleepCtx(ctx, benchSettleMs*time.Millisecond)
}

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

// driveActiveMultiPaneStream holds the exact normal-use shape under load:
// six panes stay mounted, four start rich-Markdown turns back to back, and
// the provider keeps those turns open until the requested duration expires.
// Low-frequency DOM readings prove each active pane continued to paint new
// text throughout the window. A timer alone would call a wedged provider a
// sustained workload and repeat the invalid short-soak result this replaces.
func driveActiveMultiPaneStream(ctx context.Context, run *benchRun) (returnErr error) {
	if run.duration < benchActiveMinimumDuration {
		return fmt.Errorf("active-multi-pane duration %s is below the %s minimum", run.duration, benchActiveMinimumDuration)
	}
	if len(run.threadIDs) != benchActivePaneCount {
		return fmt.Errorf("active-multi-pane opened %d threads, want %d", len(run.threadIDs), benchActivePaneCount)
	}
	activeIDs := run.threadIDs[:benchActiveStreamCount]

	waitCtx, cancelWait := context.WithCancel(ctx)
	completions := make(chan benchTurnCompletion, len(activeIDs))
	for _, threadID := range activeIDs {
		waiting := run.client.Await(string(eventchan.ProviderTurnCompleted), matchTurnCompletion(threadID))
		go func(id string, awaiting *harnessclient.Awaiting) {
			_, err := awaiting.Wait(waitCtx)
			completions <- benchTurnCompletion{threadID: id, at: time.Now(), err: err}
		}(threadID, waiting)
	}

	started := make([]string, 0, len(activeIDs))
	cleanupNeeded := true
	defer func() {
		if cleanupNeeded && len(started) > 0 {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), benchInterruptTimeout)
			cleanupErr := interruptBenchTurns(cleanupCtx, run.client, started)
			cancel()
			if cleanupErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("clean up active bench turns: %w", cleanupErr))
			}
		}
		cancelWait()
	}()

	sendCtx, cancelSend := context.WithTimeout(ctx, benchTurnTimeout)
	for i, threadID := range activeIDs {
		// Once the frame is attempted, cleanup owns this thread. A transport
		// timeout does not prove the backend failed to receive the request.
		started = append(started, threadID)
		if _, err := run.client.Call(sendCtx, "SendMessage", threadID,
			fmt.Sprintf("bench %s run %d active pane %d", run.workload.Name, run.index, i+1), nil); err != nil {
			cancelSend()
			return fmt.Errorf("start active turn in thread %s: %w", threadID, err)
		}
	}
	cancelSend()

	startedAt := time.Now()
	deadline := time.NewTimer(run.duration)
	defer deadline.Stop()
	interval := activeBenchProgressInterval(run.duration)
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

	if err := waitForRevealDrain(ctx, run.env, run.client, benchDrainTimeout); err != nil {
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
		assistant, err := queryBenchElement(ctx, run, assistantSelector, false)
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
		scroller, err := queryBenchElement(ctx, run, scrollerSelector, true)
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
	queryCtx, cancel := context.WithTimeout(ctx, benchProbeTimeout)
	defer cancel()
	spec := map[string]any{
		"kind":     "element",
		"selector": selector,
		"textCap":  1,
	}
	if includeScroll {
		spec["includeScroll"] = true
	}
	raw, err := run.env.queryUI(queryCtx, run.client, spec)
	if err != nil {
		return benchElementAnswer{}, err
	}
	var answer benchElementAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return benchElementAnswer{}, fmt.Errorf("decode element query for %s: %w", selector, err)
	}
	return answer, nil
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
