package browser

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// A pane resize lays out only the page the pane shows. Every other page of
// the thread lags until it is needed, and then takes the size before anything
// sees it: before it is shown, and before a tool operates on it.

func TestPaneResizeLaysOutOnlyThePresentedPage(t *testing.T) {
	f := newPaneFollowFixture(t)
	mount := f.attach(t)
	hidden := f.pages[0]
	before := len(hidden.viewports)
	if err := f.manager.SetPaneRect(mount, paneRect(800, 600)); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	f.awaitActiveAt(t, 800, 600)
	f.awaitFollowIdle(t)
	hidden.mu.Lock()
	after := len(hidden.viewports)
	hidden.mu.Unlock()
	if after != before {
		t.Fatalf("a resize laid out the hidden page %d times", after-before)
	}
}

// A tool call on a lagging page lays it out first, so its coordinates and
// screenshots mean the viewport the agent was told.
func TestALaggingPageTakesTheViewportBeforeAToolOperation(t *testing.T) {
	f := newPaneFollowFixture(t)
	mount := f.attach(t)
	if err := f.manager.SetPaneRect(mount, paneRect(800, 600)); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	f.awaitActiveAt(t, 800, 600)
	f.awaitFollowIdle(t)

	hidden := f.pages[0]
	var seenW, seenH int
	hidden.mu.Lock()
	hidden.screenshot = func(context.Context) ([]byte, error) {
		seenW, seenH = hidden.lastViewport()
		return []byte("png"), nil
	}
	hidden.mu.Unlock()
	hiddenID := f.ids[0]
	if _, err := f.manager.Screenshot(t.Context(), f.access, ScreenshotOptions{PageID: hiddenID}); err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	if seenW != 800 || seenH != 600 {
		t.Fatalf("the screenshot ran at %dx%d, want the thread viewport 800x600", seenW, seenH)
	}

	// A page that cannot take the size fails the operation instead of
	// answering at the wrong size.
	if err := f.manager.SetPaneRect(mount, paneRect(900, 700)); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	f.awaitActiveAt(t, 900, 700)
	f.awaitFollowIdle(t)
	hidden.mu.Lock()
	hidden.viewportErr = errors.New("engine refused")
	hidden.mu.Unlock()
	if _, err := f.manager.Screenshot(t.Context(), f.access, ScreenshotOptions{PageID: hiddenID}); err == nil || !strings.Contains(err.Error(), "engine refused") {
		t.Fatalf("an operation on a page that refused the viewport = %v", err)
	}
}

// Activating a lagging page lays it out before it is shown, through the
// user's tab selection and through a selection that skips resolvePage (the
// companion shown on a page) alike.
func TestALaggingPageIsLaidOutBeforeItIsShown(t *testing.T) {
	for _, tc := range []struct {
		name     string
		activate func(f *paneFollowFixture, pageID string) error
	}{
		{"tab selection", func(f *paneFollowFixture, pageID string) error {
			return f.manager.ActivateCompanionPage(context.Background(), f.access, pageID)
		}},
		{"visibility", func(f *paneFollowFixture, pageID string) error {
			show := true
			_, err := f.manager.Visibility(context.Background(), f.access, &show, pageID)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPaneFollowFixture(t)
			mount := f.attach(t)
			if err := f.manager.SetPaneRect(mount, paneRect(800, 600)); err != nil {
				t.Fatalf("set pane rect: %v", err)
			}
			f.awaitActiveAt(t, 800, 600)
			f.awaitFollowIdle(t)

			hidden := f.pages[0]
			hiddenHandle := hidden.Handle()
			var mu sync.Mutex
			var shownAt [][2]int
			f.engine.mu.Lock()
			f.engine.calls = nil
			f.engine.mu.Unlock()
			showRecorder := &showSizes{page: hidden, handle: hiddenHandle, mu: &mu, sizes: &shownAt}
			f.engine.mu.Lock()
			f.engine.onShow = showRecorder.record
			f.engine.mu.Unlock()

			if err := tc.activate(f, f.ids[0]); err != nil {
				t.Fatalf("activate: %v", err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				mu.Lock()
				n := len(shownAt)
				mu.Unlock()
				if n > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the page was never shown; engine calls %v", f.engine.take())
				}
				time.Sleep(time.Millisecond)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, size := range shownAt {
				if size != [2]int{800, 600} {
					t.Fatalf("the page was shown at %dx%d before it took the viewport", size[0], size[1])
				}
			}
		})
	}
}

// showSizes records the size a page had when the engine was told to show it.
type showSizes struct {
	page   *fakePage
	handle string
	mu     *sync.Mutex
	sizes  *[][2]int
}

func (s *showSizes) record(handle string) {
	if handle != s.handle {
		return
	}
	w, h := s.page.lastViewport()
	s.mu.Lock()
	*s.sizes = append(*s.sizes, [2]int{w, h})
	s.mu.Unlock()
}

// A page that refuses the size is shown anyway, under the error the refusal
// raised, rather than held hidden and retried forever.
func TestAPageThatRefusesTheSizeIsStillShown(t *testing.T) {
	f := newPaneFollowFixture(t)
	f.pages[1].mu.Lock()
	f.pages[1].viewportErr = errors.New("engine refused")
	f.pages[1].mu.Unlock()
	mount := f.attach(t)
	f.engine.take()
	if err := f.manager.SetPaneRect(mount, paneRect(800, 600)); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	f.awaitFollowIdle(t)
	handle := f.pages[1].Handle()
	if calls := strings.Join(f.engine.take(), " "); !strings.Contains(calls, "show:"+handle) {
		t.Fatalf("the refusing page was never shown: %s", calls)
	}
	f.pages[1].mu.Lock()
	attempts := len(f.pages[1].viewports)
	f.pages[1].mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	f.pages[1].mu.Lock()
	later := len(f.pages[1].viewports)
	f.pages[1].mu.Unlock()
	if later != attempts {
		t.Fatalf("the refused size kept being retried: %d more attempts", later-attempts)
	}
}

// A drag's reports collapse into the passes the drain runs, and the thread's
// state is pushed once when the drain settles, not once per pass.
func TestADrainPushesStateOnceWhenItSettles(t *testing.T) {
	f := newPaneFollowFixture(t)
	mount := f.attach(t)
	active := f.pages[1]
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	active.mu.Lock()
	active.viewportHook = func(w, h int) {
		if w == 700 {
			entered <- struct{}{}
			<-release
		}
	}
	active.mu.Unlock()
	for {
		select {
		case <-f.events:
			continue
		default:
		}
		break
	}
	if err := f.manager.SetPaneRect(mount, paneRect(700, 500)); err != nil {
		t.Fatalf("set pane rect: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first pass never started")
	}
	// Two more sizes while the first pass is held: one more pass applies
	// the newest.
	for _, w := range []float64{750, 800} {
		if err := f.manager.SetPaneRect(mount, paneRect(w, 600)); err != nil {
			t.Fatalf("set pane rect: %v", err)
		}
	}
	close(release)
	f.awaitActiveAt(t, 800, 600)
	f.awaitFollowIdle(t)
	states := 0
	for {
		select {
		case event := <-f.events:
			if event.Kind == "state" {
				states++
				if event.ViewportWidth != 800 {
					t.Fatalf("the settled push carries %d, want the final 800", event.ViewportWidth)
				}
			}
			continue
		default:
		}
		break
	}
	if states != 1 {
		t.Fatalf("a two-pass drain pushed %d states, want 1", states)
	}
}
