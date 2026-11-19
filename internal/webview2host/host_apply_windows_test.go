//go:build windows

package webview2host

import (
	"testing"
)

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
	if page.shown.bounds == bounds.paneBounds() {
		t.Fatal("a refused bounds is recorded as applied")
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
