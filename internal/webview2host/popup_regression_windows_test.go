//go:build windows

package webview2host

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDepartingChildWaitsForItsLiveUnannouncedOpener(t *testing.T) {
	r := newPopupRig(t)
	_, opener := r.createPage("opener", "prof", false)
	parent, parentController := r.openPopup(opener, newFakeWindowArgs(popupURL))
	child, childController := r.openPopup(parentController, newFakeWindowArgs(popupURL))
	answerTarget(t, childController, "child-target")
	completed := r.startDownload(child, "dl-completed")
	finish(completed)
	childController.view.raise("close-requested")
	r.pump()
	if reports := r.takeReports(); len(reports) != 0 {
		t.Errorf("child reported before its opener: %v", kinds(t, reports))
	}
	closed(t, childController)
	answerTarget(t, parentController, "parent-target")
	sameReports(t, "parent announcement", kinds(t, r.takeReports()),
		parent.id+":popup(opener,parent-target)", parent.id+":find-unavailable", parent.id+":download(dl-completed,completed)")
	r.done()
}

func TestDepartingChildMergesTerminalSnapshotsWithinOpenerBound(t *testing.T) {
	r := newPopupRig(t)
	_, opener := r.createPage("opener", "prof", false)
	parent, parentController := r.openPopup(opener, newFakeWindowArgs(popupURL))
	for i := range maxHeldPopupDownloads - 1 {
		d := r.startDownload(parent, fmt.Sprintf("dl-parent-%d", i))
		finish(d)
	}
	child, _ := r.openPopup(parentController, newFakeWindowArgs(popupURL))
	r.startDownload(child, "dl-running")
	r.startDownload(child, "dl-extra")
	r.host.Apply(Directive{Op: OpClose, PageID: child.id})
	if reports := r.takeReports(); len(reports) != 0 {
		t.Errorf("child reported before its opener: %v", kinds(t, reports))
	}
	held := parent.popup.held.downloads
	if len(held) != maxHeldPopupDownloads {
		t.Errorf("parent holds %d downloads, want %d", len(held), maxHeldPopupDownloads)
	} else if kept := held[len(held)-1]; kept.ID != "dl-running" || kept.State != DownloadInterrupted || kept.Reason != PopupGoneReason {
		t.Errorf("last held snapshot = %+v, want the first child's interrupted download", kept)
	}
	r.done()
}

func TestDepartingSubtreeLeavesDownloadsWithItsLiveUnannouncedOpener(t *testing.T) {
	r := newPopupRig(t)
	_, opener := r.createPage("opener", "prof", false)
	parent, parentController := r.openPopup(opener, newFakeWindowArgs(popupURL))
	child, childController := r.openPopup(parentController, newFakeWindowArgs(popupURL))
	grandchild, _ := r.openPopup(childController, newFakeWindowArgs(popupURL))
	d := r.startDownload(grandchild, "dl-completed")
	finish(d)
	r.host.Apply(Directive{Op: OpClose, PageID: child.id})
	sameReports(t, "departing subtree", kinds(t, r.takeReports()))

	r.host.Apply(Directive{Op: OpClose, PageID: parent.id})
	sameReports(t, "parent departure", kinds(t, r.takeReports()),
		parent.id+":popup(opener,)", parent.id+":find-unavailable", parent.id+":download(dl-completed,completed)", parent.id+":closed")
	r.done()
}

func TestPopupDeadlineAlsoBoundsTargetDiscovery(t *testing.T) {
	r := newPopupRig(t)
	var expire func()
	r.host.afterFunc = func(_ time.Duration, fn func()) { expire = fn }
	_, opener := r.createPage("opener", "prof", false)
	popup, controller := r.openPopup(opener, newFakeWindowArgs(popupURL))
	controller.view.processFailed()
	expire()
	r.pump()
	if r.page(popup.id) != nil {
		t.Error("popup with no target survived the deadline")
	}
	closed(t, controller)
	r.done()
}

func TestExpiredPopupHoldsNativeCreateSlotUntilCompletion(t *testing.T) {
	r := newPopupRig(t)
	var timers []func()
	r.host.afterFunc = func(_ time.Duration, fn func()) { timers = append(timers, fn) }
	_, opener := r.createPage("opener", "prof", false)
	for range MaxPendingPopups {
		opener.view.requestWindow(newFakeWindowArgs(popupURL))
	}
	for _, expire := range timers {
		expire()
	}
	r.pump()
	for range MaxPendingPopups {
		args := newFakeWindowArgs(popupURL)
		opener.view.requestWindow(args)
		if !args.handled || args.deferral != nil {
			t.Error("a request was admitted with all native creates outstanding")
		}
	}
	if got := len(r.env.pending); got != MaxPendingPopups {
		t.Errorf("%d outstanding creates, want %d", got, MaxPendingPopups)
	}
	r.env.complete(t, 0x80004004, nil)
	args := newFakeWindowArgs(popupURL)
	opener.view.requestWindow(args)
	if args.deferral == nil {
		t.Error("a failed late completion did not free its slot")
	}
	r.done()
}

func TestPopupAtHeldDownloadBoundCanResume(t *testing.T) {
	r := newPopupRig(t)
	_, opener := r.createPage("opener", "prof", false)
	popup, controller := r.openPopup(opener, newFakeWindowArgs(popupURL))
	first := newFakeOp(`C:\D\first.bin`)
	start(t, popup, first)
	for i := 1; i < maxHeldPopupDownloads; i++ {
		d := r.startDownload(popup, fmt.Sprintf("dl-ended-%d", i))
		finish(d)
	}
	drop(first)
	controller.view.raise("close-requested")
	r.pump()
	continued := newFakeOp(`C:\D\first (1).bin`)
	// The URI stays the same even though WebView2 proposes another path.
	continued.uri = first.uri
	if got := resume(t, popup, continued); got.put != first.path {
		t.Errorf("resume did not keep its running download's path: %q", got.put)
	}
	continued.received = 100
	continued.set(downloadStateCompleted, 0)
	r.pump()
	if r.page(popup.id) != nil {
		t.Error("popup was left waiting on the replaced operation")
	}
	r.done()
}

func TestPopupURIPrefixDoesNotRetainItsOriginalAllocation(t *testing.T) {
	r := newPopupRig(t)
	_, opener := r.createPage("opener", "prof", false)
	retained := func() uint64 {
		runtime.GC()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return stats.HeapAlloc
	}
	before := retained()
	const uriSize = 4 << 20
	popup, controller := r.openPopup(opener, newFakeWindowArgs("https://example.test/"+strings.Repeat("a", uriSize)))
	after := retained()
	if len(popup.popup.uri) != MaxPopupURLBytes || after > before+uriSize/2 {
		t.Errorf("prefix is %d bytes and live heap changed from %d to %d; it must not pin a 4 MiB allocation",
			len(popup.popup.uri), before, after)
	}
	answerTarget(t, controller, "popup-target")
	r.done()
}
