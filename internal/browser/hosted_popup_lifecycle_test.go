package browser

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/browser/popupadmit"
	"agent-overflow/internal/webview2host"
)

// A popup's close or discard received while its attach is under way only
// signals the attach to end: with the attached target's detach held at the
// fake CDP socket, the close's acknowledgment, the discard's deadline and
// an unrelated page's report all return at once, and the attach's caller
// joins its teardown once the detach is answered. Nothing is listed.
func TestAPopupsEndAtReceiptWaitsForNoDetach(t *testing.T) {
	for _, how := range []string{"close", "discard"} {
		t.Run(how, func(t *testing.T) {
			l := newPopupLauncher(t)
			access := l.access("thread")
			_, opener := l.open(access)
			l.stallFrameTree.Store(true)
			releaseDetach := l.holdDetach()
			defer releaseDetach()
			popupID, _ := l.popup(opener, "https://example.test/popup")
			awaitSessionCall(t, l.endpoint, "S-PT-1", "Page.getFrameTree")
			switch how {
			case "close":
				returnsWithin(t, time.Second, "the close's acknowledgment", func() {
					l.report(popupID, webview2host.ReportClosed, "")
				})
			case "discard":
				returnsWithin(t, time.Second, "the discard past its deadline", func() {
					ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
					defer cancel()
					_ = l.engine.DiscardPage(ctx, ids.PageHandle(popupID))
				})
			}
			returnsWithin(t, time.Second, "an unrelated page's report", func() {
				sendDownload(t, l.engine, opener, snapshot("dl-opener", webview2host.DownloadRunning, 1))
			})
			l.awaitHeldDetach()
			releaseDetach()
			l.stallFrameTree.Store(false)
			if d, _ := l.manager.admissions.Disposition(ids.PageHandle(popupID)); d == popupadmit.Listed {
				t.Fatal("the popup whose end was received was listed")
			}
			l.settledEngine()
			if _, listed := l.tabs(string(access.ThreadID()))[ids.PageHandle(popupID)]; listed {
				t.Fatal("the popup whose end was received is a tab")
			}
		})
	}
}

// A popup's close received while the engine's lock is held, as the
// Manager reads its history before listing it, is in the Manager's
// admission before the page is seen sealed: the listing that follows is
// refused, and the receipt waited for no engine lock.
func TestAPopupSealedAtReceiptIsNeverListed(t *testing.T) {
	l := newPopupLauncher(t)
	access := l.access("thread")
	_, opener := l.open(access)
	releaseHistory := l.gateHistory("PT-1")
	defer releaseHistory()
	popupID, _ := l.popup(opener, "https://example.test/popup")
	l.awaitAttach("history PT-1")
	l.engine.mu.Lock()
	locked := true
	defer func() {
		if locked {
			l.engine.mu.Unlock()
		}
	}()
	received := make(chan struct{})
	go func() {
		defer close(received)
		l.report(popupID, webview2host.ReportClosed, "")
	}()
	for deadline := time.Now().Add(hostedTestDeadline); ; time.Sleep(time.Millisecond) {
		if state, _ := l.engine.pages.state(ids.PageHandle(popupID)); state == hostedPopupClosing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the popup's close was never received")
		}
	}
	releaseHistory()
	for deadline := time.Now().Add(hostedTestDeadline); ; time.Sleep(time.Millisecond) {
		if d, _ := l.manager.admissions.Disposition(ids.PageHandle(popupID)); d != popupadmit.Pending {
			if d == popupadmit.Listed {
				t.Fatal("the Manager listed a popup sealed at receipt")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the popup's listing never committed")
		}
	}
	awaitClosed(t, received, "the close's receipt under the engine's lock")
	l.engine.mu.Unlock()
	locked = false
	settleReports(t, l.engine, "")
	if _, listed := l.tabs(string(access.ThreadID()))[ids.PageHandle(popupID)]; listed {
		t.Fatal("the closed popup is a tab")
	}
}

// awaitSessionCall waits until the fake endpoint received method on
// session.
func awaitSessionCall(t *testing.T, endpoint *fakeCDPEndpoint, session, method string) {
	t.Helper()
	deadline := time.Now().Add(hostedTestDeadline)
	for {
		for _, call := range endpoint.calls() {
			if call.Method == method && call.SessionID == session {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the endpoint never received %s on %s", method, session)
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitCall waits until the fake endpoint received a command of method.
func awaitCall(t *testing.T, calls func() []fakeCDPCall, method string) {
	t.Helper()
	deadline := time.Now().Add(hostedTestDeadline)
	for {
		for _, call := range calls() {
			if call.Method == method {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the endpoint never received %s", method)
		}
		time.Sleep(time.Millisecond)
	}
}

// returnsWithin runs fn and fails unless it returns within limit.
func returnsWithin(t *testing.T, limit time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("%s did not return within %s", what, limit)
	}
}

// A page whose attach the pane browser leaves unanswered, its connection
// still open, fails at the attach bound, ends when its creation is
// canceled, and does not hold up the Manager's shutdown.
func TestHostedPageAttachThatStallsEnds(t *testing.T) {
	t.Run("bound", func(t *testing.T) {
		l := newPopupLauncher(t)
		l.engine.attachTimeout = 200 * time.Millisecond
		l.stallFrameTree.Store(true)
		returnsWithin(t, hostedTestDeadline, "the stalled creation", func() {
			if _, err := l.manager.createPage(t.Context(), l.access("thread")); err == nil {
				t.Error("a page whose attach never finished was created")
			}
		})
	})
	t.Run("canceled", func(t *testing.T) {
		l := newPopupLauncher(t)
		l.engine.attachTimeout = time.Hour
		l.stallFrameTree.Store(true)
		ctx, cancel := context.WithCancel(t.Context())
		created := make(chan error, 1)
		go func() {
			_, err := l.manager.createPage(ctx, l.access("thread"))
			created <- err
		}()
		awaitCall(t, l.endpoint.calls, "Page.getFrameTree")
		cancel()
		select {
		case err := <-created:
			if err == nil {
				t.Fatal("a canceled creation created its page")
			}
		case <-time.After(hostedTestDeadline):
			t.Fatal("the canceled creation never returned")
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		l := newPopupLauncher(t)
		l.engine.attachTimeout = time.Hour
		l.stallFrameTree.Store(true)
		go func() { _, _ = l.manager.createPage(t.Context(), l.access("thread")) }()
		awaitCall(t, l.endpoint.calls, "Page.getFrameTree")
		returnsWithin(t, hostedTestDeadline, "the shutdown", func() { _ = l.manager.Close() })
	})
}
