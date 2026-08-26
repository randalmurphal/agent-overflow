package main

// The bench drivers: everything that happens between arming the meters and
// stopping them, plus the two page-side preconditions a bench depends on.
// Split from the runner so cmd_bench.go stays about the run sequence and
// this file stays about what a workload DOES to the app.

import (
	"context"
	"encoding/json"
	"fmt"
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
	// than a wedged one. Past this the window ends anyway, with a note —
	// see waitForRevealDrain.
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
	// revealDrainGlobal is the page-side probe the drain wait polls. It is
	// installed by main.ts in every build (unlike the UI_TRACE diagnostics
	// globals) and whitelisted in frontend/src/lib/harness/globals.ts.
	revealDrainGlobal = "__aoRevealDrain"
)

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
// Three degradations, none of them a failed run. A page whose bridge has
// no such global (an older build) answers `unavailable` and the wait
// returns immediately with a note. So does a bridge whose whitelist has
// never heard of it, which arrives as a query ERROR rather than an answer.
// And a drain still going at the timeout ends the window anyway, with a
// note naming what was still outstanding — a slow drain is a finding to
// read in the report, not a reason to throw away the run that produced it.
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
			e.drainNote("the page could not answer %s (%v); the window ends at turn completion",
				revealDrainGlobal, err)
			return nil
		}
		var answer struct {
			Unavailable bool        `json:"unavailable"`
			Value       revealDrain `json:"value"`
		}
		if err := json.Unmarshal(raw, &answer); err != nil {
			e.drainNote("the page's %s answer did not decode (%v); the window ends at turn completion",
				revealDrainGlobal, err)
			return nil
		}
		if answer.Unavailable {
			e.drainNote("this page does not install %s; the window ends at turn completion",
				revealDrainGlobal)
			return nil
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
			e.drainNote("the reveal queue was still draining after %s (%d pane(s), %d smoother(s)); the window ends here",
				timeout, last.Draining, last.Smoothers)
			return nil
		}
		if err := sleepCtx(ctx, benchDrainPollInterval); err != nil {
			return err
		}
	}
}

// drainNote is one operator sentence about a measurement window, never an
// error. Silent under -o json, where the document is the answer and a
// stray line would corrupt it.
func (e *env) drainNote(format string, args ...any) {
	if e.jsonOutput() {
		return
	}
	e.printf("note: "+format+"\n", args...)
}

// awaitTurnCompletion parks on `provider:turn_completed` for one thread.
// Split out because the multi-pane workload waits on several of them and
// must not reimplement the payload match.
func awaitTurnCompletion(ctx context.Context, client *harnessclient.Client, threadID string) error {
	channel := string(eventchan.ProviderTurnCompleted)
	if _, err := client.WaitForEvent(ctx, channel, func(ev harnessclient.Event) bool {
		if ev.Gap {
			return false
		}
		var payload struct {
			ThreadID string `json:"threadId"`
		}
		return json.Unmarshal(ev.Data, &payload) == nil && payload.ThreadID == threadID
	}); err != nil {
		return fmt.Errorf("the turn never completed: %w", err)
	}
	return nil
}

// driveOneTurn sends the workload's message, waits for the app to close the
// turn, waits for the reveal queue to hand the result to the reader, then
// lets the meters run one more settle window.
func driveOneTurn(ctx context.Context, run *benchRun) error {
	threadID := run.threadIDs[0]
	// The turn keeps its own budget. The drain that follows is bounded
	// separately (and answers with a note rather than an error), so folding
	// the two into one deadline would let a slow drain report itself as a
	// turn that never completed.
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
