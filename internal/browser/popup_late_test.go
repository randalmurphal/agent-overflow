package browser

import (
	"strings"
	"testing"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/browser/quota"
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

// lateCarrier is a manager on an engine whose downloads are AO's, with two
// worktrees' threads in one project jar: access's, whose tab opener
// opened popup "popup", which ended unlisted before opener closed (holder
// is its page), and which keeps another tab; and other's, whose tab keeps
// the jar open whatever access's workspace closes. start is the report of
// a download the popup started, made while all of that was open.
type lateCarrier struct {
	manager       *Manager
	engine        *adoptionEngine
	access, other Access
	holder        *managedPage
	profile       profileHandle
	opener        ids.PageHandle
}

func newLateCarrier(t *testing.T) *lateCarrier {
	t.Helper()
	manager, engine := newAdoptionBrowser(t, downloadsAsArtifacts)
	proj := newProject(t, "P")
	c := &lateCarrier{manager: manager, engine: engine, access: proj.access(manager, "a", proj.one), other: proj.access(manager, "b", proj.two)}
	setJar(t, manager, c.access, JarProject)
	setJar(t, manager, c.other, JarProject)
	page := mustOpen(t, manager, c.access, "https://example.test/")
	mustOpen(t, manager, c.access, "https://example.test/beside")
	mustOpen(t, manager, c.other, "https://example.test/other")
	c.opener = driverHandle(manager, c.access, page.ID)
	c.holder = listedPage(t, manager, c.opener)
	engine.mu.Lock()
	c.profile = engine.profile
	engine.mu.Unlock()
	manager.pageEndReceived("popup", true, "", []ids.PageHandle{c.opener})
	manager.resolveParked("popup", nil)
	if err := manager.CloseCompanionPage(t.Context(), c.access, c.holder.id); err != nil {
		t.Fatal(err)
	}
	return c
}

// start is the report of download "late" the popup started, whose end its
// engine has received when ended.
func (c *lateCarrier) start(ended bool) downloadStart {
	start := lateStart(c.engine, "late", c.opener, ended)
	start.Profile = c.profile
	return start
}

// closeJar closes every tab of the jar and waits for the jar to close.
func (c *lateCarrier) closeJar(t *testing.T) {
	t.Helper()
	closeTabs(t, c.manager, c.access)
	closeTabs(t, c.manager, c.other)
	eventually(t, "the jar to close", func() bool {
		c.manager.mu.Lock()
		defer c.manager.mu.Unlock()
		return c.manager.profileScopeLocked(c.profile) == nil
	})
}

// On an engine whose downloads are AO's, a download a popup starts after
// its fate, whose opener left, is refused when the leave would have
// canceled it or nothing can keep it: its engine has not received its
// end, its workspace has no room, or its jar closed, opened again since
// or not. Its row is canceled saying why and it holds nothing of either
// quota.
func TestALateDownloadOfADepartedOpenerIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ended  bool
		cause  string
		before func(t *testing.T, c *lateCarrier)
	}{
		{name: "running, with room", cause: "page closed"},
		{name: "ended, its workspace full", ended: true, cause: "workspace downloads exceed", before: func(t *testing.T, c *lateCarrier) {
			authorityOf(t, c.manager, c.access).pool.Pool().SetUsed(maxWorkspaceDownloadBytes)
		}},
		{name: "ended, its jar closed", ended: true, cause: "page closed", before: func(t *testing.T, c *lateCarrier) {
			c.closeJar(t)
		}},
		{name: "ended, its jar closed and opened again", ended: true, cause: "page closed", before: func(t *testing.T, c *lateCarrier) {
			c.closeJar(t)
			mustOpen(t, c.manager, c.access, "https://example.test/again")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newLateCarrier(t)
			if tc.before != nil {
				tc.before(t, c)
			}
			var pool *quota.Pool
			var workspace int64
			if authority := authorityOf(t, c.manager, c.access); authority != nil {
				pool = authority.pool.Pool()
				workspace = pool.Used()
			}
			artifacts := c.manager.artifactBytes.Used()
			wantAdmission(t, "a late download of a departed opener", c.manager.downloadStarted(c.start(tc.ended)), downloadRefused)
			if row := holderRow(t, c.holder, "late"); row.State != downloadStateCanceled || !strings.Contains(row.Error, tc.cause) {
				t.Fatalf("the late download lists as %+v, want it canceled for %q", row, tc.cause)
			}
			if pool != nil && pool.Used() != workspace {
				t.Fatalf("the refused download left %d of its workspace's bytes held, want %d", pool.Used(), workspace)
			}
			if got := c.manager.artifactBytes.Used(); got != artifacts {
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
		before func(t *testing.T, c *lateCarrier)
	}{
		{name: "with room"},
		{name: "its workspace's tabs all closed and one opened since", before: func(t *testing.T, c *lateCarrier) {
			closeTabs(t, c.manager, c.access)
			eventually(t, "the workspace to lose its authority", func() bool { return authorityOf(t, c.manager, c.access) == nil })
			mustOpen(t, c.manager, c.access, "https://example.test/again")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newLateCarrier(t)
			if tc.before != nil {
				tc.before(t, c)
			}
			pool := authorityOf(t, c.manager, c.access).pool.Pool()
			workspace, artifacts := pool.Used(), c.manager.artifactBytes.Used()
			engineArtifact(t, c.manager, "late")
			wantAdmission(t, "a late finished download of a departed opener", c.manager.downloadStarted(c.start(true)), downloadAdmitted)
			if got := pool.Used() - workspace; got != maxDownloadBytes {
				t.Fatalf("the kept download holds %d of its workspace's bytes, want %d", got, maxDownloadBytes)
			}
			c.manager.downloadProgress(progressOf("late", downloadCompleted, 5))
			if row := holderRow(t, c.holder, "late"); row.State != downloadStateCompleted || row.Bytes != 5 {
				t.Fatalf("the late download lists as %+v, want it completed at 5 bytes", row)
			}
			if got := pool.Used() - workspace; got != 5 {
				t.Fatalf("the finished download holds %d of its workspace's bytes, want its 5", got)
			}
			if got := c.manager.artifactBytes.Used() - artifacts; got != 5 {
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
