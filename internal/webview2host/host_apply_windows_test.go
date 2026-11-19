//go:build windows

package webview2host

import (
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

// deadWindow answers the handle of a window that no longer exists, which
// SetWindowPos refuses.
func deadWindow(t *testing.T, parent uintptr) uintptr {
	t.Helper()
	hwnd, err := createClipContainer(parent)
	if err != nil {
		t.Fatal(err)
	}
	destroyWindow(hwnd)
	return hwnd
}

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
	if page.shown.hasBounds {
		t.Fatal("a refused bounds left the page at a known place")
	}
	controller.putErr = 0
	r.host.Apply(bounds)
	if len(controller.put) != 1 {
		t.Fatalf("the same geometry after a refusal put %d bounds, want 1", len(controller.put))
	}
	applied := controller.put[0]
	r.host.Apply(bounds)
	if len(controller.put) != 1 || page.shown.bounds != bounds.paneBounds() {
		t.Fatalf("applied geometry put again: %d puts", len(controller.put))
	}
	if applied.width() != 300 || applied.height() != 200 {
		t.Fatalf("the controller is at %+v", applied)
	}
	r.host.Apply(Directive{Op: OpClose, PageID: "page-1"})
	r.pump()
	closed(t, controller)
}

// A move that fails partway leaves no cached place. The controller refusing
// B after the clip moved to B, then A: both move back to A. The clip
// refusing B, then B again: both move to B. A page not shown yet whose
// first move fails is not shown at a place it never reached.
func TestAPartlyAppliedMoveIsMadeAgain(t *testing.T) {
	r := newPopupRig(t)
	defer r.done()
	page, controller := r.createPage("page-1", "jar", false)
	a := Directive{Op: OpBounds, PageID: "page-1", X: 10, Y: 20, W: 300, H: 200, DPR: 1, VW: 1000, VH: 800}
	b := a
	b.X, b.Y = 200, 150
	r.host.Apply(a)
	r.host.Apply(Directive{Op: OpShow, PageID: "page-1"})
	clipA := windowRect(t, page.container)
	controller.put = nil

	controller.putErr = 0x80004005 // E_FAIL
	r.host.Apply(b)
	if windowRect(t, page.container) == clipA {
		t.Fatal("the clip container did not move to B")
	}
	controller.putErr = 0
	r.host.Apply(a)
	if len(controller.put) != 1 || windowRect(t, page.container) != clipA {
		t.Fatalf("back at A after a refused B: %d controller puts, clip at %+v, want 1 and %+v", len(controller.put), windowRect(t, page.container), clipA)
	}

	container := page.container
	page.container = deadWindow(t, r.host.config.HostWindow())
	r.host.Apply(b)
	if page.shown.hasBounds || len(controller.put) != 1 {
		t.Fatalf("a clip that did not move: known place %v, %d controller puts", page.shown.hasBounds, len(controller.put))
	}
	page.container = container
	r.host.Apply(b)
	if len(controller.put) != 2 || windowRect(t, page.container) == clipA || page.shown.bounds != b.paneBounds() || !page.shown.hasBounds {
		t.Fatalf("B again after its clip failed: %d controller puts, clip at %+v", len(controller.put), windowRect(t, page.container))
	}

	hidden, hiddenController := r.createPage("page-2", "jar", false)
	r.host.Apply(Directive{Op: OpBounds, PageID: "page-2", X: 10, Y: 20, W: 300, H: 200, DPR: 1, VW: 1000, VH: 800})
	hiddenContainer := hidden.container
	hidden.container = deadWindow(t, r.host.config.HostWindow())
	r.host.Apply(Directive{Op: OpShow, PageID: "page-2"})
	if hidden.shown.visible || hidden.shown.hasBounds {
		t.Fatalf("a page whose first move failed shows: %+v", hidden.shown)
	}
	hidden.container = hiddenContainer
	r.host.Apply(Directive{Op: OpShow, PageID: "page-2"})
	if !hidden.shown.visible || !hidden.shown.hasBounds || len(hiddenController.put) != 1 {
		t.Fatalf("the next show did not place and show it: %+v, %d puts", hidden.shown, len(hiddenController.put))
	}

	r.host.Apply(Directive{Op: OpClose, PageID: "page-1"})
	r.host.Apply(Directive{Op: OpClose, PageID: "page-2"})
	r.pump()
	closed(t, controller)
	closed(t, hiddenController)
}
