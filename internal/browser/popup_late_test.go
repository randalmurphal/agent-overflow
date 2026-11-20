package browser

import (
	"testing"

	"agent-overflow/internal/browser/ids"
)

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
// its fate is not kept for an opener that left: whether its engine
// received its end or not, whether the opener's workspace has room, has
// none, or has had its last tab closed and another opened since, it is
// refused with a row canceled as the leave canceled its others, and holds
// nothing of either quota.
func TestALateDownloadOfADepartedOpenerIsNotKept(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ended  bool
		before func(t *testing.T, manager *Manager, access Access)
	}{
		{name: "running, with room"},
		{name: "ended, with room", ended: true},
		{name: "ended, its workspace full", ended: true, before: func(t *testing.T, manager *Manager, access Access) {
			authorityOf(t, manager, access).downloadBytes.SetUsed(maxWorkspaceDownloadBytes)
		}},
		{name: "ended, its workspace's tabs all closed and one opened since", ended: true, before: func(t *testing.T, manager *Manager, access Access) {
			for _, page := range manager.CompanionState(access.Caller()).Pages {
				if err := manager.CloseCompanionPage(t.Context(), access, PageID(page.ID)); err != nil {
					t.Fatal(err)
				}
			}
			mustOpen(t, manager, access, "https://example.test/again")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
			mustOpen(t, manager, access, "https://example.test/other")
			holder := listedPage(t, manager, opener)
			manager.pageEndReceived("popup", true, "", []ids.PageHandle{opener})
			manager.resolveParked("popup", nil)
			if err := manager.CloseCompanionPage(t.Context(), access, holder.id); err != nil {
				t.Fatal(err)
			}
			if tc.before != nil {
				tc.before(t, manager, access)
			}
			workspace, artifacts := authorityOf(t, manager, access).downloadBytes.Used(), manager.artifactBytes.Used()
			wantAdmission(t, "a late download of a departed opener", manager.downloadStarted(lateStart(engine, "late", opener, tc.ended)), downloadRefused)
			if row := holderRow(t, holder, "late"); row.State != downloadStateCanceled || row.Error != "page closed" {
				t.Fatalf("the late download lists as %+v, want it canceled with its opener's leave", row)
			}
			if got := authorityOf(t, manager, access).downloadBytes.Used(); got != workspace {
				t.Fatalf("the refused download left %d of its workspace's bytes held, want %d", got, workspace)
			}
			if got := manager.artifactBytes.Used(); got != artifacts {
				t.Fatalf("the refused download left %d artifact bytes held, want %d", got, artifacts)
			}
		})
	}
}

// A download's source is the address its page had as the download started
// or, for one a popup starts after its fate, as its page was chosen,
// whatever the page navigates to while its admission waits.
func TestADownloadsSourceIsItsPagesAddressAsItStarted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(manager *Manager, engine *adoptionEngine, opener ids.PageHandle) downloadStart
	}{
		{"a download of a listed page", func(_ *Manager, _ *adoptionEngine, opener ids.PageHandle) downloadStart {
			return downloadStart{Frame: frameID(opener), ID: "dl", URL: "https://example.test/file"}
		}},
		{"a download a popup starts after its fate", func(manager *Manager, engine *adoptionEngine, opener ids.PageHandle) downloadStart {
			manager.resolveParked("popup", nil, opener)
			return lateStart(engine, "dl", opener, false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, engine, access, opener := newAdoptionManager(t)
			page := listedPage(t, manager, opener)
			started := page.cachedInfo().URL
			start := tc.start(manager, engine, opener)
			manager.downloadAdmitting = func() {
				page.metaMu.Lock()
				page.info.URL = "https://example.test/next"
				page.metaMu.Unlock()
			}
			wantAdmission(t, tc.name, manager.downloadStarted(start), downloadAdmitted)
			history := threadDownloads(manager, access)
			if len(history) != 1 || history[0].Source != started {
				t.Fatalf("the download's history is %+v, want its source %q", history, started)
			}
		})
	}
}
