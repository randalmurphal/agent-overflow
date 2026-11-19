//go:build windows

package webview2host

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// A popup's life from its window request to its announcement
// (popup_windows.go) against stand-in WebView2 objects: announced after
// its opener, gone with its opener while unannounced, and gone the same way
// on every path, its downloads never unheard.

// answerTarget answers a popup's pending target read.
func answerTarget(t *testing.T, controller *fakeController, target string) {
	t.Helper()
	controller.view.answerDevTools(t, 0, `{"targetInfo":{"targetId":"`+target+`"}}`)
}

// startDownload starts a download of page under id, on a stand-in
// operation, as WebView2 would report it.
func (r *popupRig) startDownload(page *hostPage, id string) *hostDownload {
	r.t.Helper()
	op := newFakeOp(`C:\Downloads\` + id)
	op.uri = "https://example.test/" + id
	op.refs++ // the reference DownloadStarting hands over
	d := &hostDownload{id: id, page: page, uri: readDownloadAddress(op.uri, nil), path: op.path}
	r.host.attachOperation(d, op.com())
	if page.downloads == nil {
		page.downloads = map[string]*hostDownload{}
	}
	page.downloads[id] = d
	r.host.report(page, d.snapshot())
	return d
}

// finish has WebView2 complete a download startDownload started.
func finish(d *hostDownload) {
	(*fakeOp)(unsafe.Pointer(d.op)).set(downloadStateCompleted, 0)
}

// awaitReports answers the reports that arrive until none has for a moment,
// at least want of them.
func (r *popupRig) awaitReports(want int) []rigReport {
	r.t.Helper()
	var got []rigReport
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		r.pump()
		got = append(got, r.takeReports()...)
		if len(got) >= want {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	r.pump()
	return append(got, r.takeReports()...)
}

// kinds answers the reports as page:kind, a popup's opener and target in
// its popup report, and a download's state.
func kinds(t *testing.T, reports []rigReport) []string {
	t.Helper()
	var out []string
	for _, report := range reports {
		entry := report.page + ":" + string(report.kind)
		switch report.kind {
		case ReportPopup:
			parsed, err := ParsePopupReport(report.detail)
			if err != nil {
				t.Fatal(err)
			}
			entry += fmt.Sprintf("(%s,%s)", parsed.Opener, parsed.Target)
		case ReportDownload:
			parsed, err := ParseDownloadReport(report.detail)
			if err != nil {
				t.Fatal(err)
			}
			entry += "(" + parsed.ID + "," + parsed.State + ")"
		case ReportCreated, ReportCreateFailed, ReportClosed, ReportProcessFailed, ReportCleared,
			ReportClearFailed, ReportAccelerator, ReportFindUnavailable, ReportFocused, ReportFocusFailed:
			// The kind alone.
		}
		out = append(out, entry)
	}
	return out
}

func sameReports(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s reported\n  %v\nwant\n  %v", what, got, want)
	}
}

// A popup whose first download finishes, and which asks to close, before
// its target is read is announced with no target, then its download, then
// closed: the saved file reaches the backend. One that made no report goes
// unheard.
func TestAPopupThatGoesBeforeItsAnnouncementReportsItsDownloads(t *testing.T) {
	r := newPopupRig(t)
	_, opener := r.createPage("opener", "prof", false)
	args := newFakeWindowArgs(popupURL)
	popup, controller := r.openPopup(opener, args)
	d := r.startDownload(popup, "dl-1")
	controller.view.raise("close-requested")
	finish(d)
	sameReports(t, "a popup gone before its target was read", kinds(t, r.awaitReports(3)),
		popup.id+":popup(opener,)", popup.id+":find-unavailable", popup.id+":download(dl-1,completed)", popup.id+":closed")
	settled(t, args, controller.view)
	closed(t, controller)

	args = newFakeWindowArgs(popupURL)
	quiet, controller := r.openPopup(opener, args)
	quiet.popup.held.findUnavailable = false
	controller.view.raise("close-requested")
	if got := r.awaitReports(0); len(got) != 0 {
		t.Fatalf("a popup gone with nothing to keep reported %v", kinds(t, got))
	}
	settled(t, args, controller.view)
	closed(t, controller)
	r.done()
}

// A popup whose download still runs when its opener takes it with it ends
// the download, since its teardown cancels it, before it is reported
// closed.
func TestAPopupGoneWithItsOpenerEndsItsRunningDownloads(t *testing.T) {
	r := newPopupRig(t)
	openerPage, opener := r.createPage("opener", "prof", false)
	popup, controller := r.openPopup(opener, newFakeWindowArgs(popupURL))
	popup.popup.held.findUnavailable = false
	r.startDownload(popup, "dl-1")
	r.host.Apply(Directive{Op: OpClose, PageID: openerPage.id})
	got := r.awaitReports(4)
	sameReports(t, "an opener closing with a downloading popup", kinds(t, got),
		popup.id+":popup(opener,)", popup.id+":download(dl-1,interrupted)", popup.id+":closed", "opener:closed")
	if parsed, _ := ParseDownloadReport(got[1].detail); parsed.Reason != PopupGoneReason {
		t.Errorf("the download ended with %q", parsed.Reason)
	}
	closed(t, controller)
	closed(t, opener)
	r.done()
}

// A popup a popup opened is announced after its opener, whichever target
// read completes first.
func TestPopupsAreAnnouncedAfterTheirOpeners(t *testing.T) {
	orders := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, order := range orders {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			r := newPopupRig(t)
			_, opener := r.createPage("opener", "prof", false)
			var pages [3]*hostPage
			var controllers [3]*fakeController
			parent := opener
			for i := range pages {
				pages[i], controllers[i] = r.openPopup(parent, newFakeWindowArgs(popupURL))
				pages[i].popup.held.findUnavailable = false
				parent = controllers[i]
			}
			for _, i := range order {
				answerTarget(t, controllers[i], fmt.Sprintf("T%d", i))
			}
			sameReports(t, "a chain of three popups", kinds(t, r.takeReports()),
				pages[0].id+":popup(opener,T0)", pages[1].id+":popup("+pages[0].id+",T1)", pages[2].id+":popup("+pages[1].id+",T2)")
			r.done()
			for _, controller := range controllers {
				closed(t, controller)
			}
		})
	}
}

// An opener that goes answers the window requests of its popups still
// being created at once, which frees their places, and each request once:
// a controller completing later is closed, and reports nothing.
func TestAnOpenerThatGoesAnswersItsPopupsRequests(t *testing.T) {
	r := newPopupRig(t)
	openerPage, opener := r.createPage("opener", "prof", false)
	first, second := newFakeWindowArgs(popupURL), newFakeWindowArgs(popupURL)
	opener.view.requestWindow(first)
	opener.view.requestWindow(second)
	if r.host.pendingPopups != 2 {
		t.Fatalf("%d window requests pending, want 2", r.host.pendingPopups)
	}
	r.host.Apply(Directive{Op: OpClose, PageID: openerPage.id})
	settled(t, first, nil)
	settled(t, second, nil)
	if r.host.pendingPopups != 2 {
		t.Fatalf("%d native creates pending after their opener went, want 2", r.host.pendingPopups)
	}
	late := []*fakeController{newFakeController(), newFakeController()}
	for _, controller := range late {
		r.env.complete(t, 0, controller)
	}
	if r.host.pendingPopups != 0 {
		t.Errorf("%d native creates pending after their completion", r.host.pendingPopups)
	}
	sameReports(t, "an opener closing with two popups being created", kinds(t, r.takeReports()), "opener:closed")
	settled(t, first, nil)
	settled(t, second, nil)
	r.done()
	closed(t, opener)
	for _, controller := range late {
		closed(t, controller)
	}
}

// A popup keeps its opener by id: a chain of popups each of whose opener
// closed keeps no page that went.
func TestAPopupKeepsNoOpenerThatWent(t *testing.T) {
	r := newPopupRig(t)
	openerPage, opener := r.createPage("opener", "prof", false)
	var finalized atomic.Int64
	const rounds = 1000
	for i := range rounds {
		popup, controller := r.openPopup(opener, newFakeWindowArgs(popupURL))
		answerTarget(t, controller, fmt.Sprintf("T%d", i))
		r.host.Apply(Directive{Op: OpClose, PageID: openerPage.id})
		runtime.SetFinalizer(openerPage, func(*hostPage) { finalized.Add(1) })
		r.takeReports()
		openerPage, opener = popup, controller
	}
	for range 10 {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
		if finalized.Load() >= rounds-1 {
			break
		}
	}
	if n := finalized.Load(); n < rounds-1 {
		t.Fatalf("%d of %d closed openers were let go", n, rounds)
	}
	r.done()
}

// The host's close settles a popup in every state: its request answered
// once, an announced one reported closed, one with a download announced
// with it and closed, and every other unheard. Every popup is reported
// closed before its opener, whatever order the registry holds them in.
func TestHostCloseSettlesPopupsInEveryState(t *testing.T) {
	for round := range 50 {
		r := newPopupRig(t)
		_, opener := r.createPage("opener", "prof", false)
		announced, announcedController := r.openPopup(opener, newFakeWindowArgs(popupURL))
		answerTarget(t, announcedController, "T-announced")
		grandchild, grandchildController := r.openPopup(announcedController, newFakeWindowArgs(popupURL))
		answerTarget(t, grandchildController, "T-grandchild")
		created, createdController := r.openPopup(opener, newFakeWindowArgs(popupURL))
		created.popup.held.findUnavailable = false
		waiting, waitingController := r.openPopup(createdController, newFakeWindowArgs(popupURL))
		waiting.popup.held.findUnavailable = false
		answerTarget(t, waitingController, "T-waiting")
		downloading, downloadingController := r.openPopup(opener, newFakeWindowArgs(popupURL))
		downloading.popup.held.findUnavailable = false
		r.startDownload(downloading, "dl-1")
		requested := newFakeWindowArgs(popupURL)
		opener.view.requestWindow(requested)
		r.takeReports()

		r.host.Close()
		got := kinds(t, r.takeReports())
		want := map[string]bool{
			downloading.id + ":popup(opener,)":             true,
			downloading.id + ":download(dl-1,interrupted)": true,
			downloading.id + ":closed":                     true,
			announced.id + ":closed":                       true,
			grandchild.id + ":closed":                      true,
			"opener:closed":                                true,
		}
		if len(got) != len(want) {
			t.Fatalf("round %d: the host's close reported %v", round, got)
		}
		at := map[string]int{}
		for i, report := range got {
			if !want[report] {
				t.Fatalf("round %d: the host's close reported %v", round, got)
			}
			at[report] = i
		}
		for popup, opener := range map[string]string{grandchild.id: announced.id, announced.id: "opener", downloading.id: "opener"} {
			if at[popup+":closed"] > at[opener+":closed"] {
				t.Fatalf("round %d: %s was reported closed before its popup %s: %v", round, opener, popup, got)
			}
		}
		settled(t, requested, nil)
		if r.host.pendingPopups != 1 {
			t.Errorf("%d native creates still pending, want 1", r.host.pendingPopups)
		}
		r.done()
		for _, controller := range []*fakeController{opener, announcedController, grandchildController, createdController, waitingController, downloadingController} {
			closed(t, controller)
		}
	}
}

// A popup not announced that goes with a popup of its own that downloaded
// is announced with it, before it, so the backend knows the opener of the
// popup it hears of.
func TestAPopupGoneWithADownloadingPopupOfItsOwnIsAnnounced(t *testing.T) {
	r := newPopupRig(t)
	_, opener := r.createPage("opener", "prof", false)
	parent, parentController := r.openPopup(opener, newFakeWindowArgs(popupURL))
	parent.popup.held.findUnavailable = false
	child, childController := r.openPopup(parentController, newFakeWindowArgs(popupURL))
	child.popup.held.findUnavailable = false
	d := r.startDownload(child, "dl-1")
	finish(d)
	parentController.view.raise("close-requested")
	sameReports(t, "a popup going with its downloading popup", kinds(t, r.awaitReports(5)),
		parent.id+":popup(opener,)", child.id+":popup("+parent.id+",)", child.id+":download(dl-1,completed)",
		child.id+":closed", parent.id+":closed")
	r.done()
	closed(t, parentController)
	closed(t, childController)
}

// A popup's process failures before its announcement all reach the
// backend after it, each kind once in the order it first came, whatever
// else it held: a crash is never replaced by a later failure of another
// kind.
func TestAPopupHoldsEveryKindOfProcessFailure(t *testing.T) {
	r := newPopupRig(t)
	_, opener := r.createPage("opener", "prof", false)
	popup, controller := r.openPopup(opener, newFakeWindowArgs(popupURL))
	popup.popup.held.findUnavailable = false
	for i := range maxHeldPopupDownloads {
		r.startDownload(popup, fmt.Sprintf("dl-%d", i))
	}
	for _, kind := range []string{"render-process-exited", "render-process-unresponsive", "render-process-unresponsive", "render-process-exited"} {
		r.host.pageReport(popup, ReportProcessFailed, kind)
	}
	answerTarget(t, controller, "T")
	var failures []string
	for _, report := range r.takeReports() {
		if report.kind == ReportProcessFailed {
			failures = append(failures, report.detail)
		}
	}
	sameReports(t, "a popup's held process failures", failures, "render-process-exited", "render-process-unresponsive")
	r.done()
}

// A popup keeps a bounded address from its request, and none once it is
// announced.
func TestAPopupKeepsABoundedAddressUntilItsAnnouncement(t *testing.T) {
	r := newPopupRig(t)
	_, opener := r.createPage("opener", "prof", false)
	long := popupURL + "?" + strings.Repeat("a", 64<<10)
	popup, controller := r.openPopup(opener, newFakeWindowArgs(long))
	if n := len(popup.popup.uri); n > MaxPopupURLBytes || !strings.HasPrefix(long, popup.popup.uri) {
		t.Fatalf("a popup keeps %d bytes of its address, want a prefix of at most %d", n, MaxPopupURLBytes)
	}
	answerTarget(t, controller, "T")
	got := r.takeReports()
	parsed, err := ParsePopupReport(got[0].detail)
	if err != nil || !strings.HasPrefix(long, parsed.URL) || parsed.URL == "" {
		t.Fatalf("the popup report's address %.80q: %v", parsed.URL, err)
	}
	if popup.popup.uri != "" {
		t.Fatal("an announced popup still keeps its address")
	}
	r.done()
}

// Expiry answers an open request without a window. An outstanding create
// keeps its slot until completion, which closes any late controller.
func TestAPendingPopupHasADeadline(t *testing.T) {
	r := newPopupRig(t)
	var timers []func()
	r.host.afterFunc = func(wait time.Duration, fn func()) {
		if wait != popupCreateBound {
			t.Errorf("a bound of %v, want %v", wait, popupCreateBound)
		}
		timers = append(timers, fn)
	}
	_, opener := r.createPage("opener", "prof", false)
	args := newFakeWindowArgs(popupURL)
	opener.view.requestWindow(args)
	if len(timers) != 1 {
		t.Fatalf("%d bounds started for one pending popup", len(timers))
	}
	go timers[0]()
	r.awaitQueued()
	r.pump()
	settled(t, args, nil)
	if r.host.pendingPopups != 1 || len(r.host.pages) != 1 {
		t.Fatalf("%d native creates pending and %d pages after expiry", r.host.pendingPopups, len(r.host.pages))
	}
	late := newFakeController()
	r.env.complete(t, 0, late)
	closed(t, late)
	if got := r.takeReports(); len(got) != 0 {
		t.Fatalf("a popup past its bound reported %v", kinds(t, got))
	}

	// One that obtained its target in time is left alone.
	args = newFakeWindowArgs(popupURL)
	popup, controller := r.openPopup(opener, args)
	answerTarget(t, controller, "target-popup")
	go timers[1]()
	r.awaitQueued()
	r.pump()
	settled(t, args, controller.view)
	if r.page(popup.id) != popup || controller.closed != 0 {
		t.Fatal("a bound closed a popup that came up in time")
	}
	// A child with its target can still be waiting for its opener.
	_, waitingController := r.openPopup(opener, newFakeWindowArgs(popupURL))
	child, childController := r.openPopup(waitingController, newFakeWindowArgs(popupURL))
	answerTarget(t, childController, "target-child")
	if child.popup.stage != popupTargeted {
		t.Fatal("child with a target did not wait for its opener")
	}
	timers[3]()
	r.pump()
	if r.page(child.id) != child {
		t.Fatal("expiry closed a child that had its target and was waiting for its opener")
	}

	r.done()
	closed(t, controller)
	closed(t, opener)
}

// A popup starts at most maxHeldPopupDownloads downloads before it is
// announced; a download is held as its newest snapshot.
func TestAPopupHoldsTheNewestSnapshotOfEachDownload(t *testing.T) {
	r := newPopupRig(t)
	_, opener := r.createPage("opener", "prof", false)
	popup, _ := r.openPopup(opener, newFakeWindowArgs(popupURL))
	d := r.startDownload(popup, "dl-0")
	for range 100 {
		r.host.report(popup, d.snapshot())
	}
	finish(d)
	if held := popup.popup.held.downloads; len(held) != 1 || held[0].State != DownloadCompleted {
		t.Fatalf("held %+v, want the download's completion alone", held)
	}
	for i := 1; i < maxHeldPopupDownloads; i++ {
		if popup.popupDownloadRefused() {
			t.Fatalf("download %d refused under the bound", i)
		}
		r.startDownload(popup, fmt.Sprintf("dl-%d", i))
	}
	if !popup.popupDownloadRefused() {
		t.Fatal("a download past the bound would start")
	}
	r.done()
}
