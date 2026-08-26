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
		time.Sleep(250 * time.Millisecond)
		probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := e.queryUI(probeCtx, client, map[string]any{"kind": "element", "selector": "body"})
		probeCancel()
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("the page did not come back after a reload: %w", lastErr)
}

// openThread drives the production thread-open path from outside the
// browser. `notification:activated` is the channel an OS notification
// click rides; the SPA parses the target, resolves the thread, and calls
// `openThreadInPane`, the same function a sidebar click reaches. What it
// does NOT exercise is the sidebar row itself (hit-testing, hover), which
// is why a bench built on it measures timeline mounting rather than
// sidebar interaction.
func openThread(ctx context.Context, run *benchRun, threadID string) error {
	payload, err := json.Marshal(map[string]any{"kind": "thread", "threadId": threadID})
	if err != nil {
		return err
	}
	if _, err := run.client.Call(ctx, "HarnessEmit",
		string(eventchan.NotificationActivated), json.RawMessage(payload)); err != nil {
		return err
	}
	return waitForActiveThread(ctx, run, threadID)
}

func waitForActiveThread(ctx context.Context, run *benchRun, threadID string) error {
	deadline := time.Now().Add(benchBridgeTimeout)
	var last string
	for time.Now().Before(deadline) {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
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
		time.Sleep(150 * time.Millisecond)
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
	time.Sleep(benchSettleMs * time.Millisecond)
	return nil
}

// driveThreadSwitchStorm cycles the pane through every seeded thread, so
// each switch is a real timeline unmount plus a real bounded-window load
// out of SQLite. The dwell is what makes it a storm rather than a queue:
// long enough for the mount to finish and paint, short enough that the
// pane is never idle.
func driveThreadSwitchStorm(ctx context.Context, run *benchRun) error {
	const (
		passes  = 2
		dwellMs = 220
	)
	stormCtx, cancel := context.WithTimeout(ctx, benchTurnTimeout)
	defer cancel()
	for pass := 0; pass < passes; pass++ {
		for _, threadID := range run.threadIDs {
			payload, err := json.Marshal(map[string]any{"kind": "thread", "threadId": threadID})
			if err != nil {
				return err
			}
			if _, err := run.client.Call(stormCtx, "HarnessEmit",
				string(eventchan.NotificationActivated), json.RawMessage(payload)); err != nil {
				return err
			}
			run.switches++
			select {
			case <-stormCtx.Done():
				return stormCtx.Err()
			case <-time.After(dwellMs * time.Millisecond):
			}
		}
	}
	// The storm is only honest if the switches actually landed, so the
	// final thread is confirmed on the page rather than assumed.
	if err := waitForActiveThread(stormCtx, run, run.threadIDs[len(run.threadIDs)-1]); err != nil {
		return err
	}
	time.Sleep(benchSettleMs * time.Millisecond)
	return nil
}
