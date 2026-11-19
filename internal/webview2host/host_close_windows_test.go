//go:build windows

package webview2host

import (
	"fmt"
	"testing"
)

// A page is looked up and used, or taken from the registry and torn down,
// in one UI-thread turn (host_apply_windows.go, host_close_windows.go),
// against stand-in WebView2 objects.

// A directive whose page a close it asked for took first touches nothing,
// whichever of the directive's UI-thread turns the close runs before: it
// finds its page in the turn that uses it.
func TestADirectiveAfterARequestedCloseTouchesNothing(t *testing.T) {
	for _, directive := range []Directive{
		{Op: OpBounds, PageID: "page", W: 200, H: 100},
		{Op: OpHide, PageID: "page"},
		{Op: OpCancelDownload, PageID: "page", Download: "dl-t-1", Cancel: 1},
	} {
		for turn := 1; turn <= 2; turn++ {
			t.Run(fmt.Sprintf("%s before turn %d", directive.Op, turn), func(t *testing.T) {
				r := newPopupRig(t)
				_, controller := r.createPage("page", "prof", false)
				r.host.Apply(Directive{Op: OpBounds, PageID: "page", W: 100, H: 100})
				r.host.Apply(Directive{Op: OpShow, PageID: "page"})
				turns := 0
				r.beforeMain = func() bool {
					if turns++; turns < turn {
						return false
					}
					controller.view.raise("close-requested")
					r.awaitReport("page", ReportClosed)
					return true
				}
				r.host.Apply(directive)
				if r.beforeMain != nil {
					// The directive took fewer turns.
					r.beforeMain = nil
					r.done()
					return
				}
				if got := r.takeReports(); len(got) != 0 {
					t.Errorf("a directive for a closed page reported %+v", got)
				}
				closed(t, controller)
				r.done()
			})
		}
	}
}

// The host's close returns with no controller live, a page that asked to
// close with its close still queued among them, and reports it closed once.
func TestHostCloseClosesAPageWhoseCloseIsQueued(t *testing.T) {
	r := newPopupRig(t)
	_, controller := r.createPage("page", "prof", false)
	other, otherController := r.createPage("other", "other-prof", false)
	controller.view.raise("close-requested")
	otherController.view.raise("close-requested")
	r.awaitQueued()
	r.host.Apply(Directive{Op: OpCloseProfile, ProfileID: other.profileID})
	if otherController.closed != 1 {
		t.Error("close-profile returned with the controller of a page whose close was queued live")
	}
	r.host.Close()
	if controller.closed != 1 {
		t.Error("the host's close returned with the controller of a page whose close was queued live")
	}
	r.pump()
	sameReports(t, "the host's close", kinds(t, r.awaitReports(2)), "other:closed", "page:closed")
	closed(t, controller)
	closed(t, otherController)
	r.done()
}

// One admission check holds for a backend create and a popup alike: past
// MaxHostPages a create answers create-failed and starts no controller,
// and after the host's close nothing is admitted.
func TestEveryPageIsAdmittedUnderOneBound(t *testing.T) {
	r := newPopupRig(t)
	_, opener := r.createPage("page", "prof", false)
	for i := len(r.host.pages); i < MaxHostPages-1; i++ {
		id := popupPageID("filler", uint64(i))
		r.host.pages[id] = &hostPage{id: id}
	}
	args := newFakeWindowArgs(popupURL)
	popup, popupController := r.openPopup(opener, args)
	r.host.Apply(Directive{Op: OpCreate, PageID: "past", ProfileID: "prof"})
	sameReports(t, "a create past the bound", kinds(t, r.takeReports()), "past:create-failed")
	if len(r.env.pending) != 0 || r.page("past") != nil {
		t.Error("a create past the bound started a controller")
	}
	r.host.Apply(Directive{Op: OpClose, PageID: popup.id})
	closed(t, popupController)
	r.host.Apply(Directive{Op: OpCreate, PageID: "under", ProfileID: "prof"})
	if len(r.env.pending) != 1 {
		t.Fatal("a create under the bound started no controller")
	}
	r.env.complete(t, 0x80004005, nil)
	r.takeReports()
	for id, page := range r.host.pages {
		if page.controller == nil {
			delete(r.host.pages, id)
		}
	}

	r.host.Close()
	r.host.Apply(Directive{Op: OpCreate, PageID: "late", ProfileID: "prof"})
	sameReports(t, "a create after the host's close", kinds(t, r.takeReports()), "page:closed", "late:create-failed")
	if len(r.env.pending) != 0 {
		t.Error("a create after the host's close started a controller")
	}
	r.done()
	closed(t, opener)
}

// A page that asked to close reads its downloads when its close runs: one
// that started after the request keeps the page open until it ends.
func TestARequestedCloseWaitsForADownloadThatStartedAfterIt(t *testing.T) {
	r := newPopupRig(t)
	page, controller := r.createPage("page", "prof", false)
	controller.view.raise("close-requested")
	r.awaitQueued()
	d := r.startDownload(page, "dl-1")
	r.pump()
	if r.page("page") == nil || controller.closed != 0 {
		t.Fatal("a close requested before a download started closed the page under it")
	}
	r.host.endDownload(d, d.snapshot(DownloadCompleted, ""))
	sameReports(t, "the download's end", kinds(t, r.awaitReport("page", ReportClosed)),
		"page:download(dl-1,running)", "page:download(dl-1,completed)", "page:closed")
	closed(t, controller)
	r.done()
}
