//go:build windows

package webview2host

import (
	"errors"
	"testing"
	"unsafe"
)

// windowRect is a window's rect in screen coordinates.
func windowRect(t *testing.T, hwnd uintptr) rect {
	t.Helper()
	var r rect
	if ok, _, err := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r))); ok == 0 {
		t.Fatalf("GetWindowRect: %v", err)
	}
	return r
}

// shownWindow reports whether a window's own style is shown, whatever its
// parent's: the rig's parent is a message-only window, never visible.
func shownWindow(hwnd uintptr) bool { return styleVisible(hwnd) }

// A bounds the controller refused is not the page's: the same geometry
// again moves it, where it would otherwise be taken as already applied and
// leave the controller at its old place inside the moved clip container.
func TestARefusedBoundsIsAppliedAgain(t *testing.T) {
	r := newPopupRig(t)
	defer r.done()
	page, controller := r.createPage("page-1", "jar", false)
	bounds := Directive{Op: OpBounds, PageID: "page-1", X: 10, Y: 20, W: 300, H: 200, DPR: 1, VW: 1000, VH: 800}
	r.host.Apply(Directive{Op: OpShow, PageID: "page-1"})
	controller.put = nil

	controller.putErr = 0x80004005 // E_FAIL
	r.host.Apply(bounds)
	if page.shown.placement.known {
		t.Fatal("a refused bounds left the page at a known place")
	}
	controller.putErr = 0
	r.host.Apply(bounds)
	if len(controller.put) != 1 {
		t.Fatalf("the same geometry after a refusal put %d bounds, want 1", len(controller.put))
	}
	applied := controller.put[0]
	r.host.Apply(bounds)
	if len(controller.put) != 1 || !page.shown.placement.at(bounds.paneBounds()) {
		t.Fatalf("applied geometry put again: %d puts", len(controller.put))
	}
	if applied.width() != 300 || applied.height() != 200 {
		t.Fatalf("the controller is at %+v", applied)
	}
	r.host.Apply(Directive{Op: OpClose, PageID: "page-1"})
	r.pump()
	closed(t, controller)
}

// A hide the native call refused leaves the page shown in the host's view,
// so the same hide again hides it, on the clipped path (the container's
// ShowWindow) and the directly parented fallback (the controller's
// IsVisible) alike; so does the hide of a page whose move failed.
func TestARefusedHideIsAppliedAgain(t *testing.T) {
	for _, path := range []string{"clipped", "parented"} {
		t.Run(path, func(t *testing.T) {
			r := newPopupRig(t)
			defer r.done()
			page, controller := r.createPage("page-1", "jar", false)
			if path == "parented" {
				if err := destroyWindow(page.container); err != nil {
					t.Fatal(err)
				}
				page.container = 0
			}
			hidden := func() bool {
				if path == "clipped" {
					return !shownWindow(page.container)
				}
				return !controller.visible
			}
			refuse := func(refused bool) {
				switch path {
				case "clipped":
					r.host.showClipFunc = nil
					if refused {
						r.host.showClipFunc = func(uintptr, bool) error { return errors.New("ShowWindow refused") }
					}
				case "parented":
					controller.visibleErr = 0
					if refused {
						controller.visibleErr = 0x80004005 // E_FAIL
					}
				}
			}
			a := Directive{Op: OpBounds, PageID: "page-1", X: 10, Y: 20, W: 300, H: 200, DPR: 1, VW: 1000, VH: 800}
			r.host.Apply(a)
			r.host.Apply(Directive{Op: OpShow, PageID: "page-1"})
			if !page.shown.visible || hidden() {
				t.Fatalf("the show: host view %+v, hidden %v", page.shown, hidden())
			}

			refuse(true)
			r.host.Apply(Directive{Op: OpHide, PageID: "page-1"})
			if !page.shown.visible || hidden() {
				t.Fatalf("a refused hide: host view %+v, hidden %v, want shown in both", page.shown, hidden())
			}
			refuse(false)
			r.host.Apply(Directive{Op: OpHide, PageID: "page-1"})
			if page.shown.visible || !hidden() {
				t.Fatalf("the hide again: host view %+v, hidden %v, want hidden in both", page.shown, hidden())
			}

			r.host.Apply(Directive{Op: OpShow, PageID: "page-1"})
			b := a
			b.X = 200
			controller.putErr = 0x80004005 // E_FAIL
			refuse(true)
			r.host.Apply(b)
			if !page.shown.visible || hidden() {
				t.Fatalf("a failed move whose hide was refused: host view %+v, hidden %v, want shown in both", page.shown, hidden())
			}
			refuse(false)
			r.host.Apply(b)
			if page.shown.visible || !hidden() {
				t.Fatalf("the failed move again: host view %+v, hidden %v, want hidden in both", page.shown, hidden())
			}
			controller.putErr = 0
			r.host.Apply(Directive{Op: OpClose, PageID: "page-1"})
			r.pump()
			closed(t, controller)
		})
	}
}

// A, then B with one native write refused, then A, for each write in turn:
// A show whose raise failed leaves the page shown, as its native show
// made it, with the raise owed: the next hide hides it, and the next show
// shows and raises it again.
func TestAShowWhoseRaiseFailedIsAppliedAgain(t *testing.T) {
	r := newPopupRig(t)
	defer r.done()
	page, controller := r.createPage("page-1", "jar", false)
	if err := destroyWindow(page.container); err != nil {
		t.Fatal(err)
	}
	page.container = 0
	gone, err := createClipContainer(r.host.config.HostWindow())
	if err != nil {
		t.Fatal(err)
	}
	if err := destroyWindow(gone); err != nil {
		t.Fatal(err)
	}
	page.child = gone
	r.host.Apply(Directive{Op: OpBounds, PageID: "page-1", X: 10, Y: 20, W: 300, H: 200, DPR: 1, VW: 1000, VH: 800})
	r.host.Apply(Directive{Op: OpShow, PageID: "page-1"})
	if !page.shown.visible || !page.shown.raiseFailed || !controller.visible {
		t.Fatalf("a show whose raise failed: host view %+v, natively shown %v, want shown with its raise owed", page.shown, controller.visible)
	}
	r.host.Apply(Directive{Op: OpHide, PageID: "page-1"})
	if page.shown.visible || controller.visible {
		t.Fatalf("the hide after a failed raise: host view %+v, natively shown %v, want hidden in both", page.shown, controller.visible)
	}
	r.host.Apply(Directive{Op: OpShow, PageID: "page-1"})
	if !page.shown.raiseFailed {
		t.Fatal("a show whose raise failed again recorded a raise")
	}
	page.child = 0
	r.host.Apply(Directive{Op: OpShow, PageID: "page-1"})
	if !page.shown.visible || page.shown.raiseFailed || !controller.visible {
		t.Fatalf("the show again: host view %+v, natively shown %v, want shown and raised", page.shown, controller.visible)
	}
}

// from B's first write the page is at no known place, so it is hidden
// there, whichever of its clip and controller moved, and a focus asked
// meanwhile waits; A again moves both back, shows the page and applies the
// focus once. A page not shown yet whose first move fails is not shown.
func TestAFailedMoveHidesThePageUntilItIsPlaced(t *testing.T) {
	for _, refused := range []string{"clip", "controller"} {
		t.Run(refused, func(t *testing.T) {
			r := newPopupRig(t)
			defer r.done()
			r.host.foreground = func(uintptr) bool { return true }
			r.host.config.Focus = NewFocusIntents()
			page, controller := r.createPage("page-1", "jar", false)
			a := Directive{Op: OpBounds, PageID: "page-1", Mount: "mount-1", X: 10, Y: 20, W: 300, H: 200, DPR: 1, VW: 1000, VH: 800}
			b := a
			b.X, b.Y = 200, 150
			r.host.Apply(a)
			r.host.Apply(Directive{Op: OpShow, PageID: "page-1"})
			clipA, controllerA := windowRect(t, page.container), controller.bounds
			if !shownWindow(page.container) || len(controller.put) != 1 {
				t.Fatalf("A: clip shown %v, %d controller puts", shownWindow(page.container), len(controller.put))
			}

			switch refused {
			case "clip":
				r.host.moveClipFunc = func(uintptr, rect) error { return errors.New("SetWindowPos refused") }
			case "controller":
				controller.putErr = 0x80004005 // E_FAIL
			}
			r.host.Apply(b)
			if moved := windowRect(t, page.container) != clipA; moved != (refused == "controller") {
				t.Fatalf("B with the %s refused: the clip moved %v", refused, moved)
			}
			if controller.bounds != controllerA || len(controller.put) != 1 {
				t.Fatalf("B with the %s refused: the controller is at %+v after %d puts", refused, controller.bounds, len(controller.put))
			}
			if shownWindow(page.container) || page.shown.visible || page.shown.placement.known {
				t.Fatalf("B with the %s refused: clip shown %v, host view %+v", refused, shownWindow(page.container), page.shown)
			}
			r.host.config.Focus.Note(FocusIntentMessage{Mount: "mount-1", Intent: 1})
			r.host.Apply(Directive{Op: OpFocus, PageID: "page-1", Mount: "mount-1", Intent: 1})
			if controller.focused != 0 {
				t.Fatal("a page at no known place took the keyboard")
			}

			r.host.moveClipFunc, controller.putErr = nil, 0
			r.host.Apply(a)
			if windowRect(t, page.container) != clipA || controller.bounds != controllerA || len(controller.put) != 2 {
				t.Fatalf("A again: clip at %+v, controller at %+v after %d puts, want %+v and %+v after 2", windowRect(t, page.container), controller.bounds, len(controller.put), clipA, controllerA)
			}
			if !shownWindow(page.container) || !page.shown.visible || !page.shown.placement.at(a.paneBounds()) {
				t.Fatalf("A again: clip shown %v, host view %+v", shownWindow(page.container), page.shown)
			}
			if got := r.takeReports(); controller.focused != 1 || len(got) != 1 || got[0].kind != ReportFocused {
				t.Fatalf("the waiting focus moved %d times at the show and reported %+v, want once", controller.focused, got)
			}
			r.host.Apply(a)
			if controller.focused != 1 || len(controller.put) != 2 {
				t.Fatalf("A once more: %d focus moves, %d puts", controller.focused, len(controller.put))
			}

			hidden, hiddenController := r.createPage("page-2", "jar", false)
			r.host.Apply(Directive{Op: OpBounds, PageID: "page-2", X: 10, Y: 20, W: 300, H: 200, DPR: 1, VW: 1000, VH: 800})
			switch refused {
			case "clip":
				r.host.moveClipFunc = func(uintptr, rect) error { return errors.New("SetWindowPos refused") }
			case "controller":
				hiddenController.putErr = 0x80004005 // E_FAIL
			}
			r.host.Apply(Directive{Op: OpShow, PageID: "page-2"})
			if shownWindow(hidden.container) || hidden.shown.visible || hidden.shown.placement.known {
				t.Fatalf("a page whose first move failed shows: clip shown %v, %+v", shownWindow(hidden.container), hidden.shown)
			}
			r.host.moveClipFunc, hiddenController.putErr = nil, 0
			r.host.Apply(Directive{Op: OpShow, PageID: "page-2"})
			if !shownWindow(hidden.container) || !hidden.shown.visible || !hidden.shown.placement.known || len(hiddenController.put) != 1 {
				t.Fatalf("the next show did not place and show it: %+v, %d puts", hidden.shown, len(hiddenController.put))
			}

			r.host.Apply(Directive{Op: OpClose, PageID: "page-1"})
			r.host.Apply(Directive{Op: OpClose, PageID: "page-2"})
			r.pump()
			closed(t, controller)
			closed(t, hiddenController)
		})
	}
}

// A numbered DevTools request is answered with what the native call did:
// opened, refused with the view's error, or failed for a page that is
// gone. An unnumbered one, from a backend that predates the answer, opens
// the window and answers nothing.
func TestADevToolsRequestIsAnsweredWithItsOutcome(t *testing.T) {
	r := newPopupRig(t)
	defer r.done()
	_, controller := r.createPage("page-1", "jar", false)
	view := controller.view
	answer := func(request uint64) rigReport {
		t.Helper()
		r.host.Apply(Directive{Op: OpDevTools, PageID: "page-1", Request: request})
		got := r.takeReports()
		if len(got) != 1 || got[0].page != "page-1" {
			t.Fatalf("request %d reported %+v, want one answer of page-1", request, got)
		}
		return got[0]
	}
	opened := answer(1)
	if opened.kind != ReportDevToolsOpened || opened.detail != (DevToolsReport{Request: 1}).Encode() || view.inspectorOpens != 1 {
		t.Fatalf("an opened DevTools window answered %+v after %d opens", opened, view.inspectorOpens)
	}
	view.inspectorErr = 0x80004005 // E_FAIL
	failed := answer(2)
	report, err := ParseDevToolsReport(failed.detail)
	if failed.kind != ReportDevToolsFailed || err != nil || report.Request != 2 || report.Error != hresult(0x80004005).Error() {
		t.Fatalf("a refused DevTools window answered %+v (%+v, %v)", failed, report, err)
	}
	view.inspectorErr = 0
	r.host.Apply(Directive{Op: OpDevTools, PageID: "page-1"})
	if got := r.takeReports(); len(got) != 0 || view.inspectorOpens != 3 {
		t.Fatalf("an unnumbered request reported %+v after %d opens, want the window and no answer", got, view.inspectorOpens)
	}
	r.host.Apply(Directive{Op: OpDevTools, PageID: "page-gone", Request: 3})
	got := r.takeReports()
	if len(got) != 1 || got[0].page != "page-gone" || got[0].kind != ReportDevToolsFailed {
		t.Fatalf("a request for a page that is gone reported %+v", got)
	}
	if report, err := ParseDevToolsReport(got[0].detail); err != nil || report.Request != 3 || report.Error == "" {
		t.Fatalf("a request for a page that is gone answered %+v, %v", report, err)
	}
}
