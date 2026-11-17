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
					engine.DiscardPage("popup")
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
