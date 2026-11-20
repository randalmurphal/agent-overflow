package browser

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/webview2host"
)

// A discard of a page Stop took over answers success only once every
// callback the engine owes that page returned, and that it was interrupted
// until then, whatever the page was doing when Stop took over and whenever
// the discard comes: before Stop, while the owed callback runs, or after
// it returned.

// stopDebtState puts a page in a state as Stop takes over and answers the
// page discarded; held is the callback the row holds, the one owed to
// that page unless owes is false, which a retirement under way or the
// page's own lane reaches before Stop (early).
type stopDebtState struct {
	name  string
	owes  bool
	early bool
	held  func(downloadProgress) bool
	setup func(t *testing.T, engine *hostedEngine) ids.PageHandle
}

// blockedCompletion has page1 report dl-1 running and then completed, its
// completion's path conversion held: the completion is the received end
// Stop takes from page1's lane.
func blockedCompletion(t *testing.T, engine *hostedEngine) {
	t.Helper()
	entered, _ := blockPath(t, engine, "slow.bin")
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-1", Seq: 1, State: webview2host.DownloadRunning, Path: `C:\Users\dev\Downloads\slow.bin`, Received: 1, Total: 10})
	sendDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-1", Seq: 2, State: webview2host.DownloadCompleted, Path: `C:\Users\dev\Downloads\slow.bin`, Received: 10, Total: 10})
	awaitClosed(t, entered, "the completion's path conversion")
}

// deliveringCompletion has page1 report dl-1 running and then completed:
// the completion is the received end page1's own lane takes and delivers.
func deliveringCompletion(t *testing.T, engine *hostedEngine) {
	t.Helper()
	reportDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-1", Seq: 1, State: webview2host.DownloadRunning, Path: `C:\Users\dev\Downloads\file.bin`, Received: 1, Total: 10})
	sendDownload(t, engine, "page1", webview2host.DownloadReport{ID: "dl-1", Seq: 2, State: webview2host.DownloadCompleted, Path: `C:\Users\dev\Downloads\file.bin`, Received: 10, Total: 10})
}

func isEnd(id downloadID, state downloadState) func(downloadProgress) bool {
	return func(event downloadProgress) bool { return event.ID == id && event.State == state }
}

var stopDebtStates = []stopDebtState{
	{"open, its completion received", true, false, isEnd("dl-1", downloadCompleted), func(t *testing.T, engine *hostedEngine) ids.PageHandle {
		blockedCompletion(t, engine)
		return "page1"
	}},
	{"closing, its close received behind its completion", true, false, isEnd("dl-1", downloadCompleted), func(t *testing.T, engine *hostedEngine) ids.PageHandle {
		blockedCompletion(t, engine)
		if got := engine.pages.receive("page1", receivedClose()); got.outcome != receiptQueued {
			t.Fatalf("the close behind the completion was %v", got.outcome)
		}
		return "page1"
	}},
	{"open, its lane delivering its completion", true, true, isEnd("dl-1", downloadCompleted), func(t *testing.T, engine *hostedEngine) ids.PageHandle {
		deliveringCompletion(t, engine)
		return "page1"
	}},
	{"closing, its close received behind its lane's delivery", true, true, isEnd("dl-1", downloadCompleted), func(t *testing.T, engine *hostedEngine) ids.PageHandle {
		deliveringCompletion(t, engine)
		if got := engine.pages.receive("page1", receivedClose()); got.outcome != receiptQueued {
			t.Fatalf("the close behind the delivery was %v", got.outcome)
		}
		return "page1"
	}},
	{"retiring, its retirement's last callback running", true, true, isEnd("dl-1", downloadPageGone), func(t *testing.T, engine *hostedEngine) ids.PageHandle {
		reportDownload(t, engine, "page1", snapshot("dl-1", webview2host.DownloadRunning, 10))
		go func() { _ = engine.DiscardPage(context.Background(), "page1") }()
		return "page1"
	}},
	{"a pending popup, its close received", true, false, isEnd("dl-p", downloadPageGone), func(t *testing.T, engine *hostedEngine) ids.PageHandle {
		pendingTestPopup(engine, "popup1", "TARGET-P", "page1")
		reportDownload(t, engine, "popup1", snapshot("dl-p", webview2host.DownloadRunning, 10))
		// Its retirement queues behind the opener's held completion.
		blockedCompletion(t, engine)
		if got := engine.pages.receive("popup1", receivedClose()); got.outcome != receiptQueued {
			t.Fatalf("the popup's close was %v", got.outcome)
		}
		return "popup1"
	}},
	{"open, owed nothing while Stop delivers another page's end", false, false, isEnd("dl-1", downloadCompleted), func(t *testing.T, engine *hostedEngine) ids.PageHandle {
		bindTestPage(t, engine, "page2", "TARGET-2")
		blockedCompletion(t, engine)
		return "page2"
	}},
}

// stopDebtArrival is when the row's discard comes.
type stopDebtArrival string

const (
	arrivesBeforeStop     stopDebtArrival = "before Stop"
	arrivesDuringCallback stopDebtArrival = "while the owed callback runs"
	arrivesAfterCallback  stopDebtArrival = "after the owed callback returned"
)

// awaitForgotten waits until the engine has no record of page.
func awaitForgotten(t *testing.T, engine *hostedEngine, page ids.PageHandle) {
	t.Helper()
	for deadline := time.Now().Add(hostedTestDeadline); ; time.Sleep(time.Millisecond) {
		engine.pages.mu.Lock()
		_, kept := engine.pages.records[page]
		engine.pages.mu.Unlock()
		if !kept {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Stop never forgot page %s", page)
		}
	}
}

// The cross of the page's state as Stop takes over and when its discard
// comes: 7 states and 3 arrivals (21 rows).
func TestADiscardSucceedsOnlyOnceEveryCallbackOwedToItsPageReturned(t *testing.T) {
	for _, state := range stopDebtStates {
		for _, arrival := range []stopDebtArrival{arrivesBeforeStop, arrivesDuringCallback, arrivesAfterCallback} {
			t.Run(fmt.Sprintf("%s, its discard %s", state.name, arrival), func(t *testing.T) {
				engine, _, _ := newDownloadHostedEngine(t)
				bindTestPage(t, engine, "page1", "TARGET-1")
				inHeld, releaseHeld := make(chan struct{}), make(chan struct{})
				var once, freed sync.Once
				release := func() { freed.Do(func() { close(releaseHeld) }) }
				t.Cleanup(release)
				progress := engine.events.DownloadProgress
				engine.events.DownloadProgress = func(event downloadProgress) {
					if state.held(event) {
						once.Do(func() { close(inHeld) })
						<-releaseHeld
					}
					progress(event)
				}
				page := state.setup(t, engine)
				if state.early {
					awaitClosed(t, inHeld, "the owed callback")
				}
				var got error
				answer := func(r receipt) error {
					t.Helper()
					ctx, cancel := context.WithTimeout(t.Context(), hostedTestDeadline)
					defer cancel()
					return r.wait(ctx)
				}
				var before receipt
				if arrival == arrivesBeforeStop {
					before = engine.pages.receive(page, receivedDiscard())
					if !state.owes {
						// Its lane runs nothing owed: it retires before Stop.
						got = answer(before)
					}
				}
				stopped := make(chan struct{})
				go func() {
					engine.Stop()
					close(stopped)
				}()
				awaitClosed(t, inHeld, "the owed callback")
				awaitForgotten(t, engine, page)
				discard := func() error {
					t.Helper()
					done := make(chan error, 1)
					go func() { done <- engine.DiscardPage(context.Background(), page) }()
					return awaitAnswer(t, done, "the discard")
				}
				switch arrival {
				case arrivesBeforeStop:
					if state.owes {
						got = answer(before)
					}
				case arrivesDuringCallback:
					got = discard()
				case arrivesAfterCallback:
				}
				release()
				awaitClosed(t, stopped, "Stop")
				if arrival == arrivesAfterCallback {
					got = discard()
				}
				interrupted := state.owes && arrival != arrivesAfterCallback
				if interrupted && !errors.Is(got, errDiscardInterrupted) || !interrupted && got != nil {
					t.Fatalf("the discard answered %v, want interrupted %v", got, interrupted)
				}
				engine.pages.mu.Lock()
				owed := len(engine.pages.debts)
				engine.pages.mu.Unlock()
				if owed != 0 {
					t.Fatalf("the engine still owes %d pages callbacks after Stop delivered them", owed)
				}
			})
		}
	}
}
