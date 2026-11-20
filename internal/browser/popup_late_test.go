package browser

import (
	"strings"
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

// closeTabs closes every tab of access's thread.
func closeTabs(t *testing.T, manager *Manager, access Access) {
	t.Helper()
	for _, page := range manager.CompanionState(access.Caller()).Pages {
		if err := manager.CloseCompanionPage(t.Context(), access, PageID(page.ID)); err != nil {
			t.Fatal(err)
		}
	}
}

// lateCarried is a manager on an engine whose downloads are AO's with
// popup "popup" ended unlisted and its opener, holder, closed since, while
// a second tab keeps the jar open.
func lateCarried(t *testing.T) (manager *Manager, engine *adoptionEngine, access Access, opener ids.PageHandle, holder *managedPage) {
	t.Helper()
	manager, engine, access, opener = newAdoptionManagerSaving(t, downloadsAsArtifacts)
	mustOpen(t, manager, access, "https://example.test/other")
	holder = listedPage(t, manager, opener)
	manager.pageEndReceived("popup", true, "", []ids.PageHandle{opener})
	manager.resolveParked("popup", nil)
	if err := manager.CloseCompanionPage(t.Context(), access, holder.id); err != nil {
		t.Fatal(err)
	}
	return manager, engine, access, opener, holder
}

// On an engine whose downloads are AO's, a download a popup starts after
// its fate, whose opener left, is refused when the leave would have
// canceled it or nothing can keep it: its engine has not received its
// end, its workspace has no room, or its jar closed. Its row is canceled
// saying why and it holds nothing of either quota.
func TestALateDownloadOfADepartedOpenerIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ended  bool
		cause  string
		before func(t *testing.T, manager *Manager, engine *adoptionEngine, access Access)
	}{
		{name: "running, with room", cause: "page closed"},
		{name: "ended, its workspace full", ended: true, cause: "workspace downloads exceed", before: func(t *testing.T, manager *Manager, _ *adoptionEngine, access Access) {
			authorityOf(t, manager, access).pool.Pool().SetUsed(maxWorkspaceDownloadBytes)
		}},
		{name: "ended, its jar closed", ended: true, cause: "page closed", before: func(t *testing.T, manager *Manager, engine *adoptionEngine, access Access) {
			engine.mu.Lock()
			profile := engine.profile
			engine.mu.Unlock()
			closeTabs(t, manager, access)
			eventually(t, "the jar to close", func() bool {
				manager.mu.Lock()
				defer manager.mu.Unlock()
				return manager.profileScopeLocked(profile) == nil
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, engine, access, opener, holder := lateCarried(t)
			start := lateStart(engine, "late", opener, tc.ended)
			if tc.before != nil {
				tc.before(t, manager, engine, access)
			}
			var workspace int64
			authority := authorityOf(t, manager, access)
			if authority != nil {
				workspace = authority.pool.Pool().Used()
			}
			artifacts := manager.artifactBytes.Used()
			wantAdmission(t, "a late download of a departed opener", manager.downloadStarted(start), downloadRefused)
			if row := holderRow(t, holder, "late"); row.State != downloadStateCanceled || !strings.Contains(row.Error, tc.cause) {
				t.Fatalf("the late download lists as %+v, want it canceled for %q", row, tc.cause)
			}
			if authority != nil && authority.pool.Pool().Used() != workspace {
				t.Fatalf("the refused download left %d of its workspace's bytes held, want %d", authority.pool.Pool().Used(), workspace)
			}
			if got := manager.artifactBytes.Used(); got != artifacts {
				t.Fatalf("the refused download left %d artifact bytes held, want %d", got, artifacts)
			}
		})
	}
}

// On an engine whose downloads are AO's, a download a popup starts after
// its fate, whose opener left, is kept when its engine had received its
// end, as that leave keeps a row whose end is on its way: it holds its
// workspace's allowance, the workspace's current one when its tabs all
// closed and one opened since, and its end lists it completed at its size.
func TestALateFinishedDownloadOfADepartedOpenerIsKept(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before func(t *testing.T, manager *Manager, access Access)
	}{
		{name: "with room"},
		{name: "its workspace's tabs all closed and one opened since", before: func(t *testing.T, manager *Manager, access Access) {
			closeTabs(t, manager, access)
			eventually(t, "the workspace to lose its authority", func() bool { return authorityOf(t, manager, access) == nil })
			mustOpen(t, manager, access, "https://example.test/again")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, engine, access, opener, holder := lateCarried(t)
			if tc.before != nil {
				tc.before(t, manager, access)
			}
			pool := authorityOf(t, manager, access).pool.Pool()
			workspace, artifacts := pool.Used(), manager.artifactBytes.Used()
			engineArtifact(t, manager, "late")
			wantAdmission(t, "a late finished download of a departed opener", manager.downloadStarted(lateStart(engine, "late", opener, true)), downloadAdmitted)
			if got := pool.Used() - workspace; got != maxDownloadBytes {
				t.Fatalf("the kept download holds %d of its workspace's bytes, want %d", got, maxDownloadBytes)
			}
			manager.downloadProgress(progressOf("late", downloadCompleted, 5))
			if row := holderRow(t, holder, "late"); row.State != downloadStateCompleted || row.Bytes != 5 {
				t.Fatalf("the late download lists as %+v, want it completed at 5 bytes", row)
			}
			if got := pool.Used() - workspace; got != 5 {
				t.Fatalf("the finished download holds %d of its workspace's bytes, want its 5", got)
			}
			if got := manager.artifactBytes.Used() - artifacts; got != 5 {
				t.Fatalf("the finished download holds %d artifact bytes, want its 5", got)
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
