package browser

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/webview2host"
)

// A popup's attach is canceled outside the engine's lock: chromedp's cancel
// waits for the target's cleanup, and no other engine call waits for it,
// whether the popup's close, its discard or Stop cancels it.
func TestHostedPopupAttachCancelHoldsNoEngineLock(t *testing.T) {
	for _, how := range []string{"close", "discard", "stop"} {
		t.Run(how, func(t *testing.T) {
			engine, _ := newTestHostedEngine(t, stubRelay{}, engineEvents{PageClosed: func(string) {}})
			if err := engine.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(engine.Stop)
			entered, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			engine.mu.Lock()
			engine.popups["popup"] = &hostedPopup{target: "T-popup", profile: "prof", opener: "opener", cancelAttach: func() {
				close(entered)
				<-release
			}}
			engine.pageProfile["popup"] = "prof"
			engine.mu.Unlock()
			go func() {
				switch how {
				case "close":
					engine.Report("popup", webview2host.ReportClosed, "")
				case "discard":
					if err := engine.DiscardPage(t.Context(), "popup"); err != nil {
						t.Errorf("discard: %v", err)
					}
				case "stop":
					engine.Stop()
				}
			}()
			select {
			case <-entered:
			case <-time.After(hostedTestDeadline):
				t.Fatal("the attach was never canceled")
			}
			answered := make(chan struct{})
			go func() {
				engine.Running()
				close(answered)
			}()
			select {
			case <-answered:
			case <-time.After(time.Second):
				t.Fatal("an engine call waited for an attach's cancel")
			}
		})
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
			if _, err := l.manager.createPage(t.Context(), popupTestAccess(t, "thread")); err == nil {
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
			_, err := l.manager.createPage(ctx, popupTestAccess(t, "thread"))
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
		go func() { _, _ = l.manager.createPage(t.Context(), popupTestAccess(t, "thread")) }()
		awaitCall(t, l.endpoint.calls, "Page.getFrameTree")
		returnsWithin(t, hostedTestDeadline, "the shutdown", func() { _ = l.manager.Close() })
	})
}
