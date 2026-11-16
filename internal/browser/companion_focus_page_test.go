package browser

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/webview2host"
)

// focusCalls answers the engine's focus requests since the last take.
func focusCalls(e *recordingPaneHost) []string {
	var calls []string
	for _, call := range e.take() {
		if strings.HasPrefix(call, "focus:") {
			calls = append(calls, call)
		}
	}
	return calls
}

// The address bar's Enter and Escape hand the keyboard to the page the
// thread's pane presents, and to no other thread's page.
func TestFocusCompanionPageFocusesThePresentedPage(t *testing.T) {
	w := newFocusWorld(t)
	w.e.take()
	if err := w.m.FocusCompanionPage(w.a); err != nil {
		t.Fatalf("focus thread a's page: %v", err)
	}
	if focus, _ := w.e.state(); focus != w.handle(w.a, w.aPage) {
		t.Fatalf("focus is with %q, want thread a's presented page", focus)
	}
	if calls := focusCalls(w.e.recordingPaneHost); !slices.Equal(calls, []string{"focus:" + w.handle(w.a, w.aPage)}) {
		t.Fatalf("focus requests %v, want thread a's presented page alone", calls)
	}
	if err := w.m.FocusCompanionPage(w.b); err != nil {
		t.Fatalf("focus thread b's page: %v", err)
	}
	if focus, _ := w.e.state(); focus != w.handle(w.b, w.bPage) {
		t.Fatalf("focus is with %q, want thread b's presented page", focus)
	}
}

// With nothing presented the request asks the engine for nothing and is no
// error: the pane closed, its page hidden, the pane off screen.
func TestFocusCompanionPageWithNothingPresentedIsANoOp(t *testing.T) {
	for _, tc := range []struct {
		name string
		hide func(t *testing.T, w *focusWorld)
	}{
		{"the pane closed", func(t *testing.T, w *focusWorld) { w.m.DetachPane(w.aMount) }},
		{"the companion hidden", func(t *testing.T, w *focusWorld) {
			hide := false
			if _, err := w.m.Visibility(t.Context(), w.a, &hide, ""); err != nil {
				t.Fatalf("hide the companion: %v", err)
			}
		}},
		{"the pane off screen", func(t *testing.T, w *focusWorld) {
			rect := visiblePaneRect
			rect.Visible = false
			if err := w.m.SetPaneRect(w.aMount, rect); err != nil {
				t.Fatalf("report the pane off screen: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFocusWorld(t)
			tc.hide(t, w)
			w.e.take()
			if err := w.m.FocusCompanionPage(w.a); err != nil {
				t.Fatalf("focus with nothing presented: %v", err)
			}
			if calls := focusCalls(w.e.recordingPaneHost); len(calls) != 0 {
				t.Fatalf("focus requests %v with nothing presented", calls)
			}
			if focus, _ := w.e.state(); focus != focusHeldByTheApp {
				t.Fatalf("focus is with %q, want the app", focus)
			}
		})
	}

	// An engine with no native views presents nothing.
	m := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { _ = m.Close() })
	if err := m.FocusCompanionPage(Access{ThreadID: "thread", Workspace: t.TempDir()}); err != nil {
		t.Fatalf("focus on an engine with no views: %v", err)
	}
}

// A page that is closing is never focused, even while the pass that hides
// it still has it recorded as presented.
func TestAClosingPageIsNeverFocused(t *testing.T) {
	w := newFocusWorld(t)
	closing := w.handle(w.a, w.aPage)
	asked := make(chan error, 1)
	w.e.mu.Lock()
	w.e.onHide = func(handle string) {
		if handle == closing {
			select {
			case asked <- w.m.FocusCompanionPage(w.a):
			default:
			}
		}
	}
	w.e.mu.Unlock()
	w.e.take()
	if err := w.m.CloseCompanionPage(t.Context(), w.a, w.aPage); err != nil {
		t.Fatalf("close the page: %v", err)
	}
	select {
	case err := <-asked:
		if err != nil {
			t.Fatalf("focus while the page closes: %v", err)
		}
	default:
		t.Fatal("the closing page was never hidden")
	}
	for _, call := range focusCalls(w.e.recordingPaneHost) {
		if call == "focus:"+closing {
			t.Fatal("the closing page was asked to take focus")
		}
	}
	if focus, _ := w.e.state(); focus == closing {
		t.Fatal("the closing page holds the focus")
	}
}

// An engine that refuses the focus fails the request, naming the page.
func TestFocusCompanionPageReportsTheEnginesRefusal(t *testing.T) {
	w := newFocusWorld(t)
	w.e.mu.Lock()
	w.e.focusErr = errors.New("view refused")
	w.e.mu.Unlock()
	err := w.m.FocusCompanionPage(w.a)
	if err == nil || !strings.Contains(err.Error(), "view refused") || !strings.Contains(err.Error(), w.aPage) {
		t.Fatalf("refused focus = %v", err)
	}
}

// The hosted engine asks the launcher to focus only a page it shows, on the
// page's own lane, and a refusal the launcher reports reaches the page's
// thread as a pane error.
func TestHostedFocusPageDirective(t *testing.T) {
	var failed []string
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{
		PageFocusFailed: func(handle string, err error) { failed = append(failed, handle+": "+err.Error()) },
	})
	engine.bind("page1", "TARGET-1")
	engine.mu.Lock()
	engine.drivers["page1"] = &cdpPage{ctx: t.Context()}
	engine.readPageRatio = func(context.Context, *cdpPage) (float64, error) { return 1, nil }
	engine.mu.Unlock()

	if err := engine.FocusPage("page1"); err != nil {
		t.Fatalf("focus a hidden page: %v", err)
	}
	if ops := sink.ops(); slices.Contains(ops, webview2host.OpFocus) {
		t.Fatalf("a hidden page was asked to focus: %v", ops)
	}
	engine.ShowPage("page1")
	if err := engine.FocusPage("page1"); err != nil {
		t.Fatalf("focus a shown page: %v", err)
	}
	var focus *webview2host.Directive
	sink.mu.Lock()
	all := slices.Clone(sink.all)
	sink.mu.Unlock()
	for _, d := range all {
		if d.Op == webview2host.OpFocus {
			focus = &d
		}
	}
	if focus == nil || focus.PageID != "page1" {
		t.Fatalf("directives %v, want a focus for page1", sink.ops())
	}
	if err := focus.Validate(); err != nil {
		t.Fatalf("the focus directive is invalid: %v", err)
	}

	engine.Report("page1", webview2host.ReportFocusFailed, "E_FAIL")
	if len(failed) != 1 || !strings.Contains(failed[0], "page1: ") || !strings.Contains(failed[0], "E_FAIL") {
		t.Fatalf("a refused focus reported %v", failed)
	}
}

// A focus the launcher could not make reaches the owning thread's pane.
func TestAFocusFailureReachesThePane(t *testing.T) {
	w := newFocusWorld(t)
	events := make(chan CompanionEvent, 16)
	w.m.SetEventSink(func(event CompanionEvent) {
		if event.Kind == "error" {
			events <- event
		}
	})
	w.m.pageFocusFailed(w.handle(w.a, w.aPage), errors.New("browser: the page did not take keyboard focus: E_FAIL"))
	select {
	case event := <-events:
		if event.ThreadID != w.a.ThreadID || !strings.Contains(event.Error, "E_FAIL") {
			t.Fatalf("error event %+v", event)
		}
	default:
		t.Fatal("no error reached the pane")
	}
}
