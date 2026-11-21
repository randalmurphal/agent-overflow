//go:build windows

package webview2host

import (
	"testing"
)

// A page is created only with every handler that keeps its windows,
// downloads and download dialog the launcher's, against stand-in WebView2
// objects.

// A page's controller, a popup's too, is created under the page's clip
// container, its final parent, and is never reparented as it is placed,
// shown and hidden: a live WebView2 controller moved to another parent can
// present stale or blank strips while it scrolls.
func TestAControllerIsCreatedUnderItsFinalParent(t *testing.T) {
	r := newPopupRig(t)
	page, controller := r.createPage("page", "prof", false)
	popup, popupController := r.openPopup(controller, newFakeWindowArgs(popupURL))
	if page.container == 0 || popup.container == 0 {
		t.Fatal("a page has no clip container")
	}
	if len(r.env.parents) != 2 || r.env.parents[0] != page.container || r.env.parents[1] != popup.container {
		t.Fatalf("controllers created under %#x, want the containers %#x and %#x", r.env.parents, page.container, popup.container)
	}
	r.host.Apply(Directive{Op: OpBounds, PageID: "page", X: 10, Y: 20, W: 300, H: 200, DPR: 1, VW: 1000, VH: 800})
	r.host.Apply(Directive{Op: OpShow, PageID: "page"})
	r.host.Apply(Directive{Op: OpHide, PageID: "page"})
	if controller.reparented != 0 || popupController.reparented != 0 {
		t.Fatalf("a live controller was reparented %d and %d times", controller.reparented, popupController.reparented)
	}
	answerTarget(t, popupController, "T-popup")
	r.done()
}

// A page whose new-window, close-request, download or dialog subscription
// fails, or whose dialog state cannot be read, is not created: a backend
// page answers create-failed and a popup's request is answered with no
// window, its controller closed either way.
func TestAPageMissingARequiredHandlerIsNotCreated(t *testing.T) {
	const eFail = 0x80004005
	breaks := map[string]func(*fakeView){
		"new-window":        func(v *fakeView) { v.subErr = map[string]uintptr{"new-window": eFail} },
		"close-requested":   func(v *fakeView) { v.subErr = map[string]uintptr{"close-requested": eFail} },
		"download-starting": func(v *fakeView) { v.subErr = map[string]uintptr{"download-starting": eFail} },
		"dialog":            func(v *fakeView) { v.subErr = map[string]uintptr{"dialog": eFail} },
		"dialog state":      func(v *fakeView) { v.dialogErr = eFail },
	}
	for name, broken := range breaks {
		t.Run(name+" of a backend page", func(t *testing.T) {
			r := newPopupRig(t)
			r.host.Apply(Directive{Op: OpCreate, PageID: "page", ProfileID: "prof"})
			controller := newFakeController()
			broken(controller.view)
			r.env.complete(t, 0, controller)
			sameReports(t, "a page missing a handler", kinds(t, r.takeReports()), "page:create-failed")
			if r.page("page") != nil {
				t.Error("a page missing a handler is registered")
			}
			closed(t, controller)
			r.done()
		})
		t.Run(name+" of a popup", func(t *testing.T) {
			r := newPopupRig(t)
			_, opener := r.createPage("opener", "prof", false)
			args := newFakeWindowArgs(popupURL)
			opener.view.requestWindow(args)
			controller := newFakeController()
			broken(controller.view)
			r.env.complete(t, 0, controller)
			settled(t, args, nil)
			if got := r.takeReports(); len(got) != 0 {
				t.Errorf("a popup missing a handler reported %v", kinds(t, got))
			}
			if len(r.host.pages) != 1 || r.host.pendingPopups != 0 {
				t.Errorf("%d pages and %d pending requests after a popup missing a handler", len(r.host.pages), r.host.pendingPopups)
			}
			closed(t, controller)
			r.done()
		})
	}
}
