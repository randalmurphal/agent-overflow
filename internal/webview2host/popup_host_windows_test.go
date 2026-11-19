//go:build windows

package webview2host

import (
	"slices"
	"testing"
	"unsafe"
)

// A page's popups against stand-in WebView2 objects (popup_fakes_windows_test.go):
// each window request answered exactly once on every path, the popup
// reported before its other reports, and every reference let go.

const popupURL = "https://example.test/popup"

// openPopup raises a window request on opener's view and answers the
// popup's controller. It answers the popup's page and controller, its
// target-info call still pending.
func (r *popupRig) openPopup(opener *fakeController, args *fakeWindowArgs) (*hostPage, *fakeController) {
	r.t.Helper()
	opener.view.requestWindow(args)
	if args.deferral == nil || args.deferral.completed != 0 {
		r.t.Fatal("a window request was not held open while its popup is created")
	}
	controller := newFakeController()
	r.env.complete(r.t, 0, controller)
	var popup *hostPage
	for _, page := range r.host.pages {
		if unsafe.Pointer(page.controller) == unsafe.Pointer(controller) {
			popup = page
		}
	}
	if popup == nil {
		r.t.Fatal("the popup's page is not registered")
	}
	return popup, controller
}

func TestPopupBecomesAPageOfItsOpenersProfile(t *testing.T) {
	r := newPopupRig(t)
	_, opener := r.createPage("opener", "prof", true)
	args := newFakeWindowArgs(popupURL)
	popup, controller := r.openPopup(opener, args)

	if got := r.env.options[len(r.env.options)-1]; got.profile != "prof" || !got.private || got.refs != 0 {
		t.Errorf("popup controller options %+v, want the opener's profile, inprivate, released", got)
	}
	if !slices.Equal(controller.put, []rect{opener.bounds}) {
		t.Errorf("popup bounds %v, want its opener's %v", controller.put, opener.bounds)
	}
	settled(t, args, controller.view)
	if r.host.pendingPopups != 0 {
		t.Errorf("%d window requests pending after the popup's was answered", r.host.pendingPopups)
	}
	// The page's reports wait for its popup report.
	controller.view.processFailed()
	if got := r.takeReports(); len(got) != 0 {
		t.Fatalf("a popup reported %+v before its popup report", got)
	}
	controller.view.answerDevTools(t, 0, `{"targetInfo":{"targetId":"popup-target"}}`)
	got := r.takeReports()
	if len(got) != 3 || got[0].kind != ReportPopup || got[1].kind != ReportFindUnavailable || got[2].kind != ReportProcessFailed {
		t.Fatalf("reports %+v, want popup, find-unavailable, process-failed", got)
	}
	for _, report := range got {
		if report.page != popup.id {
			t.Errorf("report %s under %s, want %s", report.kind, report.page, popup.id)
		}
	}
	parsed, err := ParsePopupReport(got[0].detail)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != (PopupReport{Opener: "opener", Target: "popup-target", URL: popupURL}) {
		t.Errorf("popup report %+v", parsed)
	}
	// From now on its reports go out at once, and it closes as any page.
	controller.view.processFailed()
	if got := r.takeReports(); len(got) != 1 || got[0].kind != ReportProcessFailed {
		t.Errorf("an announced popup reported %+v", got)
	}
	r.host.Apply(Directive{Op: OpClose, PageID: popup.id})
	if got := r.takeReports(); len(got) != 1 || got[0] != (rigReport{popup.id, ReportClosed, ""}) {
		t.Errorf("closing the popup reported %+v", got)
	}
	closed(t, controller)
	r.done()
	closed(t, opener)
}

// A popup's own popup is a page of the same profile, opened by the popup.
func TestPopupOfAPopup(t *testing.T) {
	r := newPopupRig(t)
	_, opener := r.createPage("opener", "prof", false)
	popup, controller := r.openPopup(opener, newFakeWindowArgs(popupURL))
	controller.view.answerDevTools(t, 0, `{"targetInfo":{"targetId":"popup-target"}}`)
	r.takeReports()

	args := newFakeWindowArgs(popupURL + "/2")
	second, secondController := r.openPopup(controller, args)
	settled(t, args, secondController.view)
	secondController.view.answerDevTools(t, 0, `{"targetInfo":{"targetId":"second-target"}}`)
	sameReports(t, "the popup's popup", kinds(t, r.takeReports()),
		second.id+":popup("+popup.id+",second-target)", second.id+":find-unavailable")
	if second.profileID != "prof" || second.id == popup.id {
		t.Errorf("second popup %s in %s, want a new page in prof", second.id, second.profileID)
	}
	r.done()
	closed(t, opener)
	closed(t, controller)
	closed(t, secondController)
}

// Every way a popup's create can end answers its window request once, with
// no window unless the popup took it, and the backend hears nothing of a
// popup it was never told of.
func TestPopupRequestIsAnsweredOnceOnEveryPath(t *testing.T) {
	type outcome struct {
		// controller is the popup's, if one came up; window says the
		// request was given its view.
		controller *fakeController
		window     bool
	}
	cases := []struct {
		name string
		run  func(r *popupRig, opener *hostPage, openerController *fakeController, args *fakeWindowArgs) outcome
	}{
		{"the host closes while the popup's controller is created", func(r *popupRig, _ *hostPage, opener *fakeController, args *fakeWindowArgs) outcome {
			opener.view.requestWindow(args)
			r.host.Close()
			// The late completion closes the controller that outlived its page.
			controller := newFakeController()
			r.env.complete(r.t, 0, controller)
			return outcome{controller: controller}
		}},
		{"its opener closes while its controller is created", func(r *popupRig, openerPage *hostPage, opener *fakeController, args *fakeWindowArgs) outcome {
			opener.view.requestWindow(args)
			r.host.Apply(Directive{Op: OpClose, PageID: openerPage.id})
			controller := newFakeController()
			r.env.complete(r.t, 0, controller)
			return outcome{controller: controller}
		}},
		{"the controller create fails at once", func(r *popupRig, _ *hostPage, opener *fakeController, args *fakeWindowArgs) outcome {
			r.env.createHR = 0x80004005 // E_FAIL
			opener.view.requestWindow(args)
			return outcome{}
		}},
		{"the controller create completes with a failure", func(r *popupRig, _ *hostPage, opener *fakeController, args *fakeWindowArgs) outcome {
			opener.view.requestWindow(args)
			r.env.complete(r.t, 0x80004005, nil)
			return outcome{}
		}},
		{"put_NewWindow refuses the view", func(r *popupRig, _ *hostPage, opener *fakeController, args *fakeWindowArgs) outcome {
			args.putErr = 0x80070057 // E_INVALIDARG
			opener.view.requestWindow(args)
			controller := newFakeController()
			r.env.complete(r.t, 0, controller)
			return outcome{controller: controller}
		}},
		{"its target cannot be read", func(r *popupRig, _ *hostPage, opener *fakeController, args *fakeWindowArgs) outcome {
			opener.view.requestWindow(args)
			controller := newFakeController()
			r.env.complete(r.t, 0, controller)
			controller.view.answerDevTools(r.t, 0x80004005, "")
			return outcome{controller: controller, window: true}
		}},
		{"the host closes before the popup's target is read", func(r *popupRig, _ *hostPage, opener *fakeController, args *fakeWindowArgs) outcome {
			opener.view.requestWindow(args)
			controller := newFakeController()
			r.env.complete(r.t, 0, controller)
			r.host.Close()
			return outcome{controller: controller, window: true}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPopupRig(t)
			openerPage, opener := r.createPage("opener", "prof", false)
			args := newFakeWindowArgs(popupURL)
			got := tc.run(r, openerPage, opener, args)
			var window *fakeView
			if got.window {
				window = got.controller.view
			}
			settled(t, args, window)
			if r.host.pendingPopups != 0 {
				t.Errorf("%d window requests pending", r.host.pendingPopups)
			}
			for _, report := range r.takeReports() {
				if report.page != openerPage.id {
					t.Errorf("the backend heard %s of a popup it was never told of", report.kind)
				}
			}
			r.done()
			if got.controller != nil {
				closed(t, got.controller)
			}
		})
	}
}

// A request the host cannot take opens no window and is never deferred.
func TestPopupRequestRefusals(t *testing.T) {
	refused := func(t *testing.T, args *fakeWindowArgs) {
		t.Helper()
		if !args.handled || args.newWindow != nil || args.deferral != nil || args.refs != 0 {
			t.Errorf("a refused request: handled %v, window %v, deferral %v, %d references; want handled with nothing else",
				args.handled, args.newWindow, args.deferral, args.refs)
		}
	}
	t.Run("past the pending bound", func(t *testing.T) {
		r := newPopupRig(t)
		_, opener := r.createPage("opener", "prof", false)
		var pending []*fakeWindowArgs
		for range MaxPendingPopups {
			args := newFakeWindowArgs(popupURL)
			opener.view.requestWindow(args)
			pending = append(pending, args)
		}
		args := newFakeWindowArgs(popupURL)
		opener.view.requestWindow(args)
		refused(t, args)
		// One answered frees a place.
		r.env.complete(t, 0x80004005, nil)
		args = newFakeWindowArgs(popupURL)
		opener.view.requestWindow(args)
		if args.deferral == nil {
			t.Error("a request under the bound was refused")
		}
		pending = append(pending, args)
		r.done()
		for _, args := range pending {
			settled(t, args, nil)
		}
	})
	t.Run("past the page bound", func(t *testing.T) {
		r := newPopupRig(t)
		_, opener := r.createPage("opener", "prof", false)
		for i := len(r.host.pages); i < MaxHostPages; i++ {
			id := popupPageID("filler", uint64(i))
			r.host.pages[id] = &hostPage{id: id}
		}
		args := newFakeWindowArgs(popupURL)
		opener.view.requestWindow(args)
		// The deferral is taken before the bound is read under the lock.
		settled(t, args, nil)
		if len(r.env.pending) != 0 {
			t.Error("a controller was created past the page bound")
		}
		r.done()
	})
	t.Run("with no environment", func(t *testing.T) {
		r := newPopupRig(t)
		_, opener := r.createPage("opener", "prof", false)
		r.host.env.release()
		args := newFakeWindowArgs(popupURL)
		opener.view.requestWindow(args)
		refused(t, args)
		r.done()
	})
	t.Run("from a page leaving the registry", func(t *testing.T) {
		r := newPopupRig(t)
		openerPage, opener := r.createPage("opener", "prof", false)
		delete(r.host.pages, openerPage.id)
		args := newFakeWindowArgs(popupURL)
		opener.view.requestWindow(args)
		settled(t, args, nil)
		if len(r.env.pending) != 0 {
			t.Error("a controller was created for a page that is going")
		}
		r.host.pages[openerPage.id] = openerPage
		r.done()
	})
	t.Run("from a closed page", func(t *testing.T) {
		r := newPopupRig(t)
		openerPage, opener := r.createPage("opener", "prof", false)
		// WebView2 may still hold the handler; keep a reference to raise it.
		handlers := slices.Clone(opener.view.subs["new-window"])
		for _, handler := range handlers {
			addRefObject(handler)
		}
		r.host.Apply(Directive{Op: OpClose, PageID: openerPage.id})
		opener.view.subs["new-window"] = handlers
		args := newFakeWindowArgs(popupURL)
		opener.view.requestWindow(args)
		refused(t, args)
		opener.view.destroy()
		r.done()
	})
}

// window.close() closes the page that asked; a page downloading closes
// once its last download ends.
func TestWindowCloseRequestClosesThePage(t *testing.T) {
	r := newPopupRig(t)
	page, controller := r.createPage("page", "prof", false)
	awaitClosed := func(what string) {
		t.Helper()
		if got := r.awaitReport("page", ReportClosed); len(got) != 1 {
			t.Fatalf("%s reported %+v, want closed", what, got)
		}
	}
	first := r.startDownload(page, "dl-1")
	second := r.startDownload(page, "dl-2")
	r.takeReports()
	controller.view.raise("close-requested")
	r.pump()
	if got := r.takeReports(); len(got) != 0 || r.page("page") == nil {
		t.Fatalf("a downloading page closed on its close request: %+v", got)
	}
	finish(first)
	r.pump()
	if got := r.takeReports(); len(got) != 1 || got[0].kind != ReportDownload || r.page("page") == nil {
		t.Fatalf("a page closed with a download running: %+v", got)
	}
	finish(second)
	if got := r.takeReports(); len(got) == 0 || got[0].kind != ReportDownload {
		t.Fatalf("the last download's end reported %+v", got)
	}
	awaitClosed("the last download's end")
	closed(t, controller)

	_, controller = r.createPage("page", "prof", false)
	controller.view.raise("close-requested")
	awaitClosed("a close request")
	closed(t, controller)
	r.done()
}

// The download dialog WebView2 opens on its own is closed when it opens.
func TestDownloadDialogIsClosedWhenItOpens(t *testing.T) {
	r := newPopupRig(t)
	_, controller := r.createPage("page", "prof", false)
	controller.view.raise("dialog")
	if controller.view.dialogShut != 0 {
		t.Error("a closed dialog was closed")
	}
	controller.view.dialogOpen = true
	controller.view.raise("dialog")
	if controller.view.dialogOpen || controller.view.dialogShut != 1 {
		t.Error("an open dialog stayed open")
	}
	// A dialog whose state cannot be read is closed anyway.
	controller.view.dialogOpen, controller.view.dialogErr = true, 0x80004005
	controller.view.raise("dialog")
	if controller.view.dialogOpen || controller.view.dialogShut != 2 {
		t.Error("a dialog whose state could not be read stayed open")
	}
	r.done()
	closed(t, controller)
}
