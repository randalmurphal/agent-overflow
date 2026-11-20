package browser

import (
	"testing"

	"agent-overflow/internal/browser/ids"
)

// lateCarriedPopup leaves popup "popup" of opener ended, never listed and
// carried by opener's leave, with another tab keeping the jar open, and
// answers the opener's page, where the popup's downloads are listed.
func lateCarriedPopup(t *testing.T, manager *Manager, access Access, opener ids.PageHandle) *managedPage {
	t.Helper()
	mustOpen(t, manager, access, "https://example.test/other")
	holder := listedPage(t, manager, opener)
	manager.pageEndReceived("popup", true, "", []ids.PageHandle{opener})
	manager.resolveParked("popup", nil)
	if err := manager.CloseCompanionPage(t.Context(), access, holder.id); err != nil {
		t.Fatal(err)
	}
	return holder
}

// lateStart is a download popup "popup" of opener starts after its fate,
// whose end its engine has received when ended.
func lateStart(e *adoptionEngine, id downloadID, opener ids.PageHandle, ended bool) downloadStart {
	e.mu.Lock()
	profile := e.profile
	e.mu.Unlock()
	return downloadStart{
		Frame: "popup", ID: id, URL: "https://example.test/" + string(id), SuggestedName: string(id),
		Popup: "popup", Openers: []ids.PageHandle{opener}, Profile: profile,
		EndReported: func() bool { return ended },
	}
}

// On an engine whose downloads are AO's, a download a popup starts after
// its fate is admitted against its workspace wherever that fate puts it,
// a departed opener's log included: one whose end its engine received
// keeps its file, so it holds its bytes of the workspace until then, and
// one past the workspace's quota is refused with a row that says why.
func TestALateDownloadInACarriersLogHoldsItsWorkspacesAllowance(t *testing.T) {
	manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	authority := authorityOf(t, manager, access)
	holder := lateCarriedPopup(t, manager, access, opener)
	before := authority.downloadBytes.Used()
	wantAdmission(t, "a late download whose end was received", manager.downloadStarted(lateStart(engine, "kept", opener, true)), downloadAdmitted)
	if got := authority.downloadBytes.Used() - before; got != maxDownloadBytes {
		t.Fatalf("the late download holds %d of its workspace's bytes, want %d", got, maxDownloadBytes)
	}
	waiting := false
	for _, row := range holder.downloads.list() {
		waiting = waiting || row.ID == "kept" && row.State == downloadStateRunning
	}
	if !waiting {
		t.Fatalf("the departed opener lists %+v, want the late download waiting for its end", holder.downloads.list())
	}
	authority.downloadBytes.SetUsed(maxWorkspaceDownloadBytes)
	wantAdmission(t, "a late download past its workspace's quota", manager.downloadStarted(lateStart(engine, "full", opener, true)), downloadRefused)
	if row := holderRow(t, holder, "full"); row.State != downloadStateCanceled || row.Error == "" {
		t.Fatalf("the refused late download lists as %+v, want it canceled with the reason", row)
	}
}

// With its jar closed there is no directory AO keeps the file in: a late
// download is nobody's, and its engine ends it and removes its file.
func TestALateDownloadOfAClosedJarIsUnowned(t *testing.T) {
	manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	holder := listedPage(t, manager, opener)
	manager.pageEndReceived("popup", true, "", []ids.PageHandle{opener})
	manager.resolveParked("popup", nil)
	if err := manager.CloseThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	wantAdmission(t, "a late download of a closed jar", manager.downloadStarted(lateStart(engine, "late", opener, true)), downloadUnowned)
	for _, row := range holder.downloads.list() {
		if row.ID == "late" {
			t.Fatalf("the unowned download was listed as %+v", row)
		}
	}
}

// A download's source is the address its page had as the download
// started, whatever the page navigates to while its admission waits.
func TestADownloadsSourceIsItsPagesAddressAsItStarted(t *testing.T) {
	manager, _, access, opener := newAdoptionManager(t)
	page := listedPage(t, manager, opener)
	started := page.cachedInfo().URL
	manager.downloadAdmitting = func() {
		page.metaMu.Lock()
		page.info.URL = "https://example.test/next"
		page.metaMu.Unlock()
	}
	wantAdmission(t, "a download of a listed page", manager.downloadStarted(downloadStart{Frame: frameID(opener), ID: "dl", URL: "https://example.test/file"}), downloadAdmitted)
	history := threadDownloads(manager, access)
	if len(history) != 1 || history[0].Source != started {
		t.Fatalf("the download's history is %+v, want its source %q", history, started)
	}
}
