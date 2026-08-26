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
// the two never drift into emitting subtly different payloads.
func activateThread(ctx context.Context, run *benchRun, threadID string) error {
	payload, err := json.Marshal(map[string]any{"kind": "thread", "threadId": threadID})
	if err != nil {
		return err
	}
	_, err = run.client.Call(ctx, "HarnessEmit",
		string(eventchan.NotificationActivated), json.RawMessage(payload))
	return err
}

// openThread activates a thread and waits for the page to show it.
func openThread(ctx context.Context, run *benchRun, threadID string) error {
	if err := activateThread(ctx, run, threadID); err != nil {
		return err
	}
	return waitForActiveThread(ctx, run, threadID)
}

func waitForActiveThread(ctx context.Context, run *benchRun, threadID string) error {
	deadline := time.Now().Add(benchBridgeTimeout)
	var last string
	for time.Now().Before(deadline) {
		probeCtx, cancel := context.WithTimeout(ctx, benchProbeTimeout)
		view, _, err := run.env.takeViewport(probeCtx, run.client, 0, 0)
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

// driveOneTurn sends the workload's message and waits for the app to close
// the turn, then lets the meters run one more settle window.
func driveOneTurn(ctx context.Context, run *benchRun) error {
	threadID := run.threadIDs[0]
	turnCtx, cancel := context.WithTimeout(ctx, benchTurnTimeout)
	defer cancel()
	if _, err := run.client.Call(turnCtx, "SendMessage", threadID,
		fmt.Sprintf("bench %s run %d", run.workload.Name, run.index), nil); err != nil {
		return err
	}
	channel := string(eventchan.ProviderTurnCompleted)
	if _, err := run.client.WaitForEvent(turnCtx, channel, func(ev harnessclient.Event) bool {
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
	return sleepCtx(turnCtx, benchSettleMs*time.Millisecond)
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
			if err := activateThread(stormCtx, run, threadID); err != nil {
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
	if err := waitForActiveThread(stormCtx, run, run.threadIDs[len(run.threadIDs)-1]); err != nil {
		return err
	}
	return sleepCtx(stormCtx, benchSettleMs*time.Millisecond)
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
