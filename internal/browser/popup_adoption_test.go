package browser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
)

// adoptionEngine attaches each popup as a fake page under the popup's own
// handle, once the popup's gate, if it has one, opens. It records the
// popups the Manager discards and the downloads it cancels. It saves
// downloads itself, as WebView2 does.
type adoptionEngine struct {
	*fakeEngine
	mu      sync.Mutex
	gates   map[ids.PageHandle]chan struct{}
	reached chan ids.PageHandle
	failing map[ids.PageHandle]bool
	// histories is each popup's history as its attach finds it, newest last.
	histories map[ids.PageHandle][]string
	discarded []ids.PageHandle
	canceled  []downloadID
	profile   profileHandle
	// stallDiscard makes a discard wait for its deadline, as an engine
	// whose browser stopped answering does.
	stallDiscard bool
	// wrap, when set, answers the driver an attach returns.
	wrap func(pageDriver) pageDriver
}

func newAdoptionManager(t *testing.T) (*Manager, *adoptionEngine, Access, ids.PageHandle) {
	t.Helper()
	return newAdoptionManagerSaving(t, downloadsSavedByEngine)
}

// newAdoptionManagerSaving is newAdoptionManager with an engine whose
// downloads go where saving says, from the jar's creation on.
func newAdoptionManagerSaving(t *testing.T, saving downloadSaving) (*Manager, *adoptionEngine, Access, ids.PageHandle) {
	t.Helper()
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { _ = manager.Close() })
	engine := &adoptionEngine{
		fakeEngine: newFakeEngine(), gates: map[ids.PageHandle]chan struct{}{},
		reached: make(chan ids.PageHandle, 16), failing: map[ids.PageHandle]bool{}, histories: map[ids.PageHandle][]string{},
	}
	engine.saving = saving
	manager.engine = engine
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page, err := manager.Open(t.Context(), access, "https://example.test/", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return manager, engine, access, driverHandle(manager, access, page.ID)
}

func (e *adoptionEngine) NewProfile(ctx context.Context, opts profileOptions) (engineProfile, error) {
	profile, err := e.fakeEngine.NewProfile(ctx, opts)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.profile = profile.Handle()
	e.mu.Unlock()
	return &adoptionProfile{fakeProfile: profile.(*fakeProfile), engine: e}, nil
}

func (e *adoptionEngine) DiscardPage(ctx context.Context, handle ids.PageHandle) error {
	e.mu.Lock()
	e.discarded = append(e.discarded, handle)
	stall := e.stallDiscard
	e.mu.Unlock()
	if stall {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// gate holds the attach of handle until the answered release.
func (e *adoptionEngine) gate(handle ids.PageHandle) (release func()) {
	ch := make(chan struct{})
	e.mu.Lock()
	e.gates[handle] = ch
	e.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

func (e *adoptionEngine) awaitAttach(t *testing.T, handle ids.PageHandle) {
	t.Helper()
	select {
	case got := <-e.reached:
		if got != handle {
			t.Fatalf("attach of %s, want %s", got, handle)
		}
	case <-time.After(hostedTestDeadline):
		t.Fatalf("%s was never attached", handle)
	}
}

func (e *adoptionEngine) discards() []ids.PageHandle {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.discarded)
}

func (e *adoptionEngine) cancels() []downloadID {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.canceled)
}

func (e *adoptionEngine) popup(opener, handle ids.PageHandle) enginePopup {
	e.mu.Lock()
	defer e.mu.Unlock()
	return enginePopup{Profile: e.profile, Opener: opener, Handle: handle}
}

type adoptionProfile struct {
	*fakeProfile
	engine *adoptionEngine
}

func (p *adoptionProfile) AttachPage(ctx context.Context, handle ids.PageHandle, hooks pageHooks) (pageDriver, error) {
	p.engine.mu.Lock()
	gate, failing := p.engine.gates[handle], p.engine.failing[handle]
	p.engine.mu.Unlock()
	if gate != nil {
		p.engine.reached <- handle
		<-gate
	}
	if failing {
		return nil, errors.New("attach failed")
	}
	driver, err := p.NewPage(ctx, hooks)
	if err != nil {
		return nil, err
	}
	p.engine.mu.Lock()
	history := p.engine.histories[handle]
	p.engine.mu.Unlock()
	if page, ok := driver.(*fakePage); ok && len(history) > 0 {
		page.mu.Lock()
		page.history, page.index = history, len(history)-1
		page.mu.Unlock()
	}
	page := &handledPage{pageDriver: driver, handle: handle}
	if p.engine.wrap != nil {
		return p.engine.wrap(page), nil
	}
	return page, nil
}

func (p *adoptionProfile) CancelDownload(id downloadID) error {
	p.engine.mu.Lock()
	p.engine.canceled = append(p.engine.canceled, id)
	p.engine.mu.Unlock()
	return nil
}

// handledPage is a fake page under the handle its popup was reported with.
type handledPage struct {
	pageDriver
	handle ids.PageHandle
}

func (p *handledPage) Handle() ids.PageHandle       { return p.handle }
func (p *handledPage) OwnsFrame(frame frameID) bool { return frame == frameID(p.handle) }

// popupDownload starts download id in popup, opened by openers, nearest
// first, and answers whether the Manager took it.
func popupDownload(m *Manager, e *adoptionEngine, popup ids.PageHandle, id downloadID, openers ...ids.PageHandle) bool {
	e.mu.Lock()
	profile := e.profile
	e.mu.Unlock()
	return m.downloadStarted(downloadStart{
		Frame: frameID(popup), ID: id, URL: "https://example.test/" + string(id), SuggestedName: string(id),
		Popup: popup, Openers: openers, Profile: profile,
	}) == downloadAdmitted
}

func progressOf(id downloadID, state downloadState, received float64) downloadProgress {
	progress := downloadProgress{ID: id, Received: received, Total: 100, State: state}
	if state == downloadCompleted {
		// Saved where the engine could not name it: completed with no file.
		progress.Error = "saved elsewhere"
	}
	return progress
}

// downloadsOf answers the downloads listed in the tab whose driver has
// handle, as "id state bytes".
func downloadsOf(m *Manager, handle ids.PageHandle) []string {
	m.mu.Lock()
	p := m.listedPageLocked(handle)
	m.mu.Unlock()
	if p == nil {
		return nil
	}
	var out []string
	for _, d := range p.downloads.list() {
		out = append(out, fmt.Sprintf("%s %s %d", d.ID, d.State, d.Bytes))
	}
	return out
}

func wantDownloads(t *testing.T, m *Manager, handle ids.PageHandle, want ...string) {
	t.Helper()
	if got := downloadsOf(m, handle); !slices.Equal(got, want) {
		t.Fatalf("tab %s lists %q, want %q", handle, got, want)
	}
}

func listed(m *Manager, handle ids.PageHandle) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listedPageLocked(handle) != nil
}

// A download a popup started and finished before its adoption began is
// listed in the popup's tab, finished.
func TestCompletedDownloadBeforeAdoptionStarts(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("the popup's download was refused")
	}
	manager.downloadProgress(progressOf("dl-1", downloadInProgress, 40))
	manager.downloadProgress(progressOf("dl-1", downloadCompleted, 100))
	manager.adoptPopup(engine.popup(opener, "popup"))
	wantDownloads(t, manager, "popup", "dl-1 completed 100")
	wantDownloads(t, manager, opener)
	if len(manager.parked) != 0 || len(manager.parkedIDs) != 0 {
		t.Fatalf("the Manager still parks %d popups, %d downloads", len(manager.parked), len(manager.parkedIDs))
	}
}

// A popup that closes before its tab is listed leaves its downloads in its
// opener's tab, a running one running, and a download it starts after goes
// there at once.
func TestAPopupClosedBeforeItsListingLeavesItsDownloadsWithItsOpener(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	popupDownload(manager, engine, "popup", "dl-1", opener)
	manager.downloadProgress(progressOf("dl-1", downloadInProgress, 10))
	popupDownload(manager, engine, "popup", "dl-2", opener)
	manager.downloadProgress(progressOf("dl-2", downloadCompleted, 100))
	manager.removeClosedPage("popup")
	wantDownloads(t, manager, opener, "dl-1 in_progress 10", "dl-2 completed 100")
	manager.downloadProgress(progressOf("dl-1", downloadCompleted, 100))
	if !popupDownload(manager, engine, "popup", "dl-3", opener) {
		t.Fatal("a download of the closed popup was refused")
	}
	wantDownloads(t, manager, opener, "dl-1 completed 100", "dl-2 completed 100", "dl-3 in_progress 0")
}

// A popup whose attach fails is discarded, and its downloads go to its
// opener, whose handle the adoption knew even when the download did not.
func TestAPopupThatIsNotAdoptedLeavesItsDownloadsWithItsOpener(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	engine.failing["popup"] = true
	popupDownload(manager, engine, "popup", "dl-1")
	manager.adoptPopup(engine.popup(opener, "popup"))
	if got := engine.discards(); !slices.Equal(got, []ids.PageHandle{"popup"}) {
		t.Fatalf("discarded %v", got)
	}
	wantDownloads(t, manager, opener, "dl-1 in_progress 0")
}

// A popup opened by a popup still being adopted waits for that adoption
// and joins its tabs; a download it started meanwhile is in its own tab.
func TestNestedPopupBeforeParentListed(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	release := engine.gate("parent")
	defer release()
	parentDone := make(chan struct{})
	go func() { manager.adoptPopup(engine.popup(opener, "parent")); close(parentDone) }()
	engine.awaitAttach(t, "parent")
	popupDownload(manager, engine, "child", "dl-1", "parent", opener)
	childDone := make(chan struct{})
	go func() { manager.adoptPopup(engine.popup("parent", "child")); close(childDone) }()
	select {
	case <-childDone:
		t.Fatal("the child's adoption ended before its opener's")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	<-parentDone
	<-childDone
	if !listed(manager, "parent") || !listed(manager, "child") {
		t.Fatalf("listed parent %v child %v", listed(manager, "parent"), listed(manager, "child"))
	}
	if n := len(manager.ownedPages(access.tb)); n != 3 {
		t.Fatalf("the thread has %d tabs, want 3", n)
	}
	wantDownloads(t, manager, "child", "dl-1 in_progress 0")
	if got := engine.discards(); len(got) != 0 {
		t.Fatalf("discarded %v", got)
	}
}

// A popup opened by a popup the Manager discards is discarded too, and its
// downloads go to the nearest opener still listed.
func TestAPopupOfADiscardedPopupIsDiscarded(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	engine.failing["parent"] = true
	release := engine.gate("parent")
	defer release()
	parentDone := make(chan struct{})
	go func() { manager.adoptPopup(engine.popup(opener, "parent")); close(parentDone) }()
	engine.awaitAttach(t, "parent")
	popupDownload(manager, engine, "child", "dl-1", "parent", opener)
	childDone := make(chan struct{})
	go func() { manager.adoptPopup(engine.popup("parent", "child")); close(childDone) }()
	release()
	<-parentDone
	// At the parent's discard, not after popupOpenerWait.
	awaitPromptly(t, childDone)
	if got := engine.discards(); !slices.Equal(got, []ids.PageHandle{"parent", "child"}) {
		t.Fatalf("discarded %v, want parent then child", got)
	}
	wantDownloads(t, manager, opener, "dl-1 in_progress 0")
}

// awaitPromptly waits for done well within popupOpenerWait.
func awaitPromptly(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(popupOpenerWait / 4):
		t.Fatal("the adoption waited for an opener that will not be listed")
	}
}

// A popup's download is listed with its terminal state before the popup's
// close takes its tab, however the close, the download's last progress
// and the park timeout fall around the popup's listing: before it, between
// its tab's listing and its downloads' (popupListing), or after.
func TestAPopupsDownloadEndsBeforeItsCloseInEveryOrder(t *testing.T) {
	type step func(m *Manager, e *adoptionEngine)
	complete := func(m *Manager, _ *adoptionEngine) { m.downloadProgress(progressOf("dl-1", downloadCompleted, 100)) }
	closePopup := func(m *Manager, _ *adoptionEngine) { m.removeClosedPage("popup") }
	expire := func(m *Manager, _ *adoptionEngine) { m.resolveParked("popup", nil) }
	for _, tc := range []struct {
		name                  string
		before, during, after []step
		// inTab says the download is still listed in the popup's tab.
		inTab bool
	}{
		{name: "all before", before: []step{complete, closePopup}},
		{name: "complete before, close during", before: []step{complete}, during: []step{closePopup}},
		{name: "both during", during: []step{complete, closePopup}},
		{name: "complete during, close after", during: []step{complete}, after: []step{closePopup}},
		{name: "complete before, close after", before: []step{complete}, after: []step{closePopup}},
		{name: "expiry during", before: []step{complete}, during: []step{expire}, inTab: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, engine, access, opener := newAdoptionManager(t)
			if !popupDownload(manager, engine, "popup", "dl-1", opener) {
				t.Fatal("the popup's download was refused")
			}
			manager.downloadProgress(progressOf("dl-1", downloadInProgress, 10))
			for _, s := range tc.before {
				s(manager, engine)
			}
			var wg sync.WaitGroup
			manager.popupListing = func() {
				// On a goroutine, since the listing may hold what they wait
				// for; the pause gives them the chance to run in the window.
				wg.Go(func() {
					for _, s := range tc.during {
						s(manager, engine)
					}
				})
				time.Sleep(50 * time.Millisecond)
			}
			manager.adoptPopup(engine.popup(opener, "popup"))
			wg.Wait()
			for _, s := range tc.after {
				s(manager, engine)
			}
			var got []string
			for _, d := range manager.CompanionState(access.Caller()).Downloads {
				got = append(got, d.State)
			}
			if !slices.Equal(got, []string{downloadStateCompleted}) {
				t.Fatalf("the thread lists downloads %v, want one completed", got)
			}
			if tc.inTab {
				wantDownloads(t, manager, "popup", "dl-1 completed 100")
			}
			if got := engine.cancels(); len(got) != 0 {
				t.Errorf("canceled %v", got)
			}
		})
	}
}

// A popup's tab closed by the user between its listing and the listing of
// its downloads leaves them to its nearest listed opener, as a popup never
// listed does: a fate naming a tab that is gone falls back to the openers.
func TestAPopupClosedPubliclyBeforeItsDownloadsAreListedLeavesThemWithItsOpener(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("the popup's download was refused")
	}
	manager.downloadProgress(progressOf("dl-1", downloadCompleted, 100))
	var wg sync.WaitGroup
	manager.popupListing = func() {
		var id string
		for _, p := range manager.ownedPages(access.tb) {
			if p.driver.Handle() == "popup" {
				id = string(p.id)
			}
		}
		wg.Go(func() {
			if err := manager.ClosePage(context.Background(), access, PageID(id)); err != nil {
				t.Errorf("close the popup's tab: %v", err)
			}
		})
		time.Sleep(50 * time.Millisecond)
	}
	manager.adoptPopup(engine.popup(opener, "popup"))
	wg.Wait()
	if listed(manager, "popup") {
		t.Fatal("the closed popup is still listed")
	}
	wantDownloads(t, manager, opener, "dl-1 completed 100")
}

// A popup whose opener is still being created or restored waits for its
// opener's listing and goes on as it is listed, or is discarded as its
// opener's creation or restore closes, not at the opener wait's bound: any
// page joining the tabs, or a creation or restore that will not, wakes the
// waiters, not only a finished adoption.
func TestAPopupOfAnOpenerNotListedYetGoesOnAtItsListing(t *testing.T) {
	for _, name := range []string{"created", "restored", "closed while created", "closed while restored"} {
		restored := name == "restored" || name == "closed while restored"
		closing := name == "closed while created" || name == "closed while restored"
		t.Run(name, func(t *testing.T) {
			manager, engine := newLifecycleManager(t)
			access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
			var pageID string
			if restored {
				page := mustOpen(t, manager, access, "https://example.test/")
				p, _, err := manager.lookupOwnedPage(access, PageID(page.ID))
				if err != nil {
					t.Fatal(err)
				}
				if moved, err := manager.suspendPage(t.Context(), p); err != nil || !moved {
					t.Fatalf("suspend: %v, %v", moved, err)
				}
				pageID = page.ID
			}
			handles := make(chan string, 1)
			hold := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(hold) }) }
			defer release()
			// The engine page exists and the tab is not listed yet.
			engine.onNewFakePage = func(page *fakePage) {
				handles <- string(page.handle)
				<-hold
			}
			opened := make(chan error, 1)
			go func() {
				var err error
				if restored {
					_, err = manager.Pages(context.Background(), access)
					if err == nil {
						_, _, err = manager.Evaluate(context.Background(), access, PageID(pageID), "1", nil)
					}
				} else {
					_, err = manager.Open(context.Background(), access, "https://example.test/", OpenOptions{})
				}
				opened <- err
			}()
			var opener string
			select {
			case opener = <-handles:
			case <-time.After(hostedTestDeadline):
				t.Fatal("the opener's engine page was never made")
			}
			listed := make(chan bool, 1)
			go func() { listed <- manager.awaitOpener(ids.PageHandle(opener)) }()
			time.Sleep(50 * time.Millisecond)
			if closing && restored {
				// The restore finds its record gone and abandons the page.
				if err := manager.ClosePage(context.Background(), access, PageID(pageID)); err != nil {
					t.Fatalf("close the restoring tab: %v", err)
				}
			} else if closing {
				tabs := manager.CompanionState(access.Caller()).Pages
				if len(tabs) != 1 {
					t.Fatalf("tabs %+v", tabs)
				}
				// The close waits for the creation it marks.
				go func() { _ = manager.ClosePage(context.Background(), access, PageID(tabs[0].ID)) }()
				for deadline := time.Now().Add(hostedTestDeadline); ; time.Sleep(time.Millisecond) {
					manager.mu.Lock()
					pending := manager.pending[PageID(tabs[0].ID)]
					marked := pending != nil && pending.closed
					manager.mu.Unlock()
					if marked {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("the creating tab was never closed")
					}
				}
			}
			start := time.Now()
			release()
			if err := <-opened; (err != nil) != closing {
				t.Fatalf("open: %v", err)
			}
			select {
			case ok := <-listed:
				if ok == closing {
					t.Fatalf("the popup's opener was taken as listed %v", ok)
				}
			case <-time.After(popupOpenerWait / 4):
				t.Fatalf("the popup still waited for its opener %v after the opener was listed or closed", time.Since(start))
			}
		})
	}
}

// A popup whose opener left before the popup's report is handled is
// discarded at once.
func TestAPopupOfAnOpenerThatLeftIsDiscardedAtOnce(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	manager.removeClosedPage(opener)
	done := make(chan struct{})
	go func() { manager.adoptPopup(engine.popup(opener, "popup")); close(done) }()
	awaitPromptly(t, done)
	if got := engine.discards(); !slices.Equal(got, []ids.PageHandle{"popup"}) {
		t.Fatalf("discarded %v", got)
	}
}

// A finished download listed only at its popup's adoption is held to the
// per-download cap of an engine whose downloads are AO's.
func TestAParkedFinishedDownloadOverTheCapIsNotKept(t *testing.T) {
	manager, engine, _, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	file := engineArtifact(t, manager, "dl-1")
	popupDownload(manager, engine, "popup", "dl-1", opener)
	manager.downloadProgress(downloadProgress{ID: "dl-1", Received: maxDownloadBytes + 1, State: downloadCompleted})
	manager.adoptPopup(engine.popup(opener, "popup"))
	wantDownloads(t, manager, "popup", fmt.Sprintf("dl-1 canceled %d", maxDownloadBytes+1))
	awaitRemoved(t, file)
}

// A popup's download not listed yet holds its bytes against the artifact
// quota from its start, and one passing the per-download cap is stopped,
// its file gone, before its popup's fate; the fate lists it canceled.
func TestAParkedDownloadIsBoundedBeforeItsFate(t *testing.T) {
	manager, engine, _, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	before := manager.artifactBytes.Used()
	file := engineArtifact(t, manager, "dl-1")
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("the popup's download was refused")
	}
	if got := manager.artifactBytes.Used() - before; got != maxDownloadBytes {
		t.Fatalf("a parked download reserved %d artifact bytes, want %d", got, maxDownloadBytes)
	}
	manager.downloadProgress(progressOf("dl-1", downloadInProgress, maxDownloadBytes+1))
	awaitRemoved(t, file)
	if got := engine.cancels(); !slices.Equal(got, []downloadID{"dl-1"}) {
		t.Fatalf("the engine canceled %q before the popup's fate, want dl-1", got)
	}
	manager.adoptPopup(engine.popup(opener, "popup"))
	wantDownloads(t, manager, "popup", fmt.Sprintf("dl-1 canceled %d", maxDownloadBytes+1))
	// Its row's discard settles the reservation once its file is gone.
	for deadline := time.Now().Add(hostedTestDeadline); manager.artifactBytes.Used() != before; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("after the fate %d artifact bytes are reserved, want %d", manager.artifactBytes.Used(), before)
		}
	}
}

// Parked downloads past the artifact quota are refused as they start.
func TestParkedDownloadsStopAtTheArtifactQuota(t *testing.T) {
	manager, engine, _, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	manager.artifactBytes.SetUsed(maxArtifactRootBytes - maxDownloadBytes)
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("a parked download within the quota was refused")
	}
	if popupDownload(manager, engine, "popup", "dl-2", opener) {
		t.Fatal("a parked download past the artifact quota was admitted")
	}
}

// A popup's download refused at its fate (no page that opened it is still
// a tab) is listed in no tab, and its file in the jar's download directory
// goes with it, and its reservation: nothing keeps it, so it would sit
// outside every quota.
func TestAParkedDownloadRefusedAtItsFateLeavesNoFile(t *testing.T) {
	for _, state := range []downloadState{downloadCompleted, downloadInProgress} {
		t.Run(state.String(), func(t *testing.T) {
			manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
			// Another tab keeps the jar open.
			openPage(t, manager, access, "https://example.test/other")
			before := manager.artifactBytes.Used()
			file := engineArtifact(t, manager, "dl-1")
			if !popupDownload(manager, engine, "popup", "dl-1", opener) {
				t.Fatal("the popup's download was refused")
			}
			manager.downloadProgress(progressOf("dl-1", state, 100))
			manager.removeClosedPage(opener)
			manager.removeClosedPage("popup")
			awaitRemoved(t, file)
			// The discard settles the reservation once the file is gone.
			for deadline := time.Now().Add(hostedTestDeadline); manager.artifactBytes.Used() != before; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("%d artifact bytes are reserved, want %d", manager.artifactBytes.Used(), before)
				}
			}
		})
	}
}

// A popup's download holds its workspace's allowance from its start, as a
// listed one does: one past the workspace's quota is refused before the
// engine writes it, whatever the artifact quota has left.
func TestAParkedDownloadHoldsItsWorkspacesAllowance(t *testing.T) {
	manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	authority := authorityOf(t, manager, access)
	before := authority.downloadBytes.Used()
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("a parked download within the quotas was refused")
	}
	if got := authority.downloadBytes.Used() - before; got != maxDownloadBytes {
		t.Fatalf("a parked download holds %d of its workspace's bytes, want %d", got, maxDownloadBytes)
	}
	authority.downloadBytes.SetUsed(maxWorkspaceDownloadBytes)
	if popupDownload(manager, engine, "popup", "dl-2", opener) {
		t.Fatal("a parked download past its workspace's quota was admitted")
	}
}

// A parked download's listing takes its reservation over: with the artifact
// quota taken up meanwhile, the download its popup finished is listed as it
// is, not canceled over quota, and holds its size of the quotas after.
func TestAParkedDownloadsListingKeepsItsReservation(t *testing.T) {
	manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	engineArtifact(t, manager, "dl-1")
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("the popup's download was refused")
	}
	manager.downloadProgress(progressOf("dl-1", downloadCompleted, 5))
	// Another download takes everything the artifact quota has left as the
	// listing begins.
	manager.downloadAdmitting = func() {
		if _, err := manager.reserveArtifacts(maxArtifactRootBytes - manager.artifactBytes.Used()); err != nil {
			t.Error("the competing reservation was refused")
		}
	}
	manager.adoptPopup(engine.popup(opener, "popup"))
	wantDownloads(t, manager, "popup", "dl-1 completed 5")
	if got := manager.artifactBytes.Used(); got != maxArtifactRootBytes-maxDownloadBytes+5 {
		t.Fatalf("%d artifact bytes are reserved, want the competitor's and the file's", got)
	}
	if got := authorityOf(t, manager, access).downloadBytes.Used(); got != 5 {
		t.Fatalf("the workspace holds %d download bytes, want the file's 5", got)
	}
}

// A finished parked download whose jar closed with its last tab is still
// removed with the browser: the download keeps the directory it was
// written in.
func TestAParkedDownloadOfAClosedJarIsRemovedWithTheBrowser(t *testing.T) {
	manager, engine, access, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
	before := manager.artifactBytes.Used()
	file := engineArtifact(t, manager, "dl-1")
	if !popupDownload(manager, engine, "popup", "dl-1", opener) {
		t.Fatal("the popup's download was refused")
	}
	manager.downloadProgress(progressOf("dl-1", downloadCompleted, 5))
	if err := manager.ClosePage(t.Context(), access, PageID(manager.CompanionState(access.Caller()).Pages[0].ID)); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	open := len(manager.scopes)
	manager.mu.Unlock()
	if open != 0 {
		t.Fatalf("%d jars are open after the last tab closed", open)
	}
	if err := manager.closeBrowser(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the parked download's file survives the browser's close: %v", err)
	}
	if got := manager.artifactBytes.Used(); got != before {
		t.Fatalf("after the close %d artifact bytes are reserved, want %d", got, before)
	}
}

// engineArtifact makes the file an engine keeping downloads as artifacts
// writes for download id in the thread's jar, and answers its path.
func engineArtifact(t *testing.T, m *Manager, id string) string {
	t.Helper()
	m.mu.Lock()
	var dir string
	for _, scope := range m.scopes {
		dir = scope.downloadDir
	}
	m.mu.Unlock()
	if dir == "" {
		t.Fatal("the jar keeps no download directory")
	}
	path := filepath.Join(dir, id)
	if err := os.WriteFile(path, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// awaitRemoved waits for path to be gone.
func awaitRemoved(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(hostedTestDeadline); ; time.Sleep(time.Millisecond) {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is still on disk", path)
		}
	}
}

// A popup reported before its opener, as goroutines may deliver them,
// waits for its opener to be reported and adopted.
func TestAPopupReportedBeforeItsOpenerWaitsForIt(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	childDone := make(chan struct{})
	go func() { manager.adoptPopup(engine.popup("parent", "child")); close(childDone) }()
	for deadline := time.Now().Add(hostedTestDeadline); ; {
		manager.mu.Lock()
		_, adopting := manager.adopting["child"]
		manager.mu.Unlock()
		if adopting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child's adoption never started")
		}
		time.Sleep(time.Millisecond)
	}
	manager.adoptPopup(engine.popup(opener, "parent"))
	<-childDone
	if n := len(manager.ownedPages(access.tb)); n != 3 {
		t.Fatalf("the thread has %d tabs, want 3", n)
	}
}

// The downloads of a popup whose fate never comes go to its opener once
// the park times out, with the newest progress reported meanwhile.
func TestParkedDownloadsGoToTheOpenerAfterTheTimeout(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	manager.parkTimeout = 10 * time.Millisecond
	popupDownload(manager, engine, "popup", "dl-1", opener)
	for i := range 50 {
		manager.downloadProgress(progressOf("dl-1", downloadInProgress, float64(i)))
	}
	// The row is listed, then takes its newest progress.
	want := []string{"dl-1 in_progress 49"}
	for deadline := time.Now().Add(hostedTestDeadline); !slices.Equal(downloadsOf(manager, opener), want); {
		if time.Now().After(deadline) {
			t.Fatalf("tab %s lists %q, want %q", opener, downloadsOf(manager, opener), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// A popup parks at most maxParkedDownloads downloads, and the Manager at
// most maxParkedPopups popups; past either a download is refused, so its
// engine cancels it.
func TestParkedDownloadsAreBounded(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	for i := range maxParkedDownloads {
		if !popupDownload(manager, engine, "popup", downloadID(fmt.Sprintf("dl-%d", i)), opener) {
			t.Fatalf("download %d was refused", i)
		}
	}
	if popupDownload(manager, engine, "popup", "dl-over", opener) {
		t.Fatal("a download past the popup's bound was taken")
	}
	for i := 1; i < maxParkedPopups; i++ {
		if !popupDownload(manager, engine, ids.PageHandle(fmt.Sprintf("popup-%d", i)), downloadID(fmt.Sprintf("p-%d", i)), opener) {
			t.Fatalf("popup %d's download was refused", i)
		}
	}
	if popupDownload(manager, engine, "popup-over", "p-over", opener) {
		t.Fatal("a download of a popup past the bound was taken")
	}
	manager.parkMu.Lock()
	popups, ids := len(manager.parked), len(manager.parkedIDs)
	manager.parkMu.Unlock()
	if popups != maxParkedPopups || ids != maxParkedDownloads+maxParkedPopups-1 {
		t.Fatalf("parked %d popups, %d downloads", popups, ids)
	}
}

// A popup that goes with no page that opened it still listed refuses its
// downloads: the engine cancels a running one, and a finished one stays
// where it was saved.
func TestParkedDownloadsWithNoListedOpenerAreCanceled(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	// Another thread's page keeps the profile, which cancels.
	if _, err := manager.Open(t.Context(), manager.Bind(Caller{ThreadID: "other", Workspace: access.Workspace()}), "https://example.test/", OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	popupDownload(manager, engine, "popup", "dl-running", opener)
	popupDownload(manager, engine, "popup", "dl-done", opener)
	manager.downloadProgress(progressOf("dl-done", downloadCompleted, 100))
	manager.removeClosedPage(opener)
	manager.removeClosedPage("popup")
	for deadline := time.Now().Add(hostedTestDeadline); len(engine.cancels()) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the running download was never canceled")
		}
		time.Sleep(time.Millisecond)
	}
	if got := engine.cancels(); !slices.Equal(got, []downloadID{"dl-running"}) {
		t.Fatalf("canceled %v", got)
	}
	if popupDownload(manager, engine, "popup", "dl-late", opener) {
		t.Fatal("a download of a popup with no listed opener was taken")
	}
}

// The browser's close drops every parked download and every fate.
func TestTheBrowsersCloseDropsParkedDownloads(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	popupDownload(manager, engine, "popup", "dl-1", opener)
	manager.resolveParked("gone", nil, opener)
	if err := manager.closeBrowser(t.Context()); err != nil {
		t.Fatal(err)
	}
	manager.parkMu.Lock()
	defer manager.parkMu.Unlock()
	if len(manager.parked) != 0 || len(manager.parkedIDs) != 0 || manager.popupFates != nil {
		t.Fatalf("parked %d, ids %d, fates %v", len(manager.parked), len(manager.parkedIDs), manager.popupFates)
	}
}

// A headless popup's download the browser's close drops takes its file in
// the jar's download directory with it, finished or not, and its
// reservation: no row lists it, and nothing else would remove it.
func TestTheBrowsersCloseRemovesParkedArtifacts(t *testing.T) {
	for _, state := range []downloadState{downloadCompleted, downloadInProgress} {
		t.Run(state.String(), func(t *testing.T) {
			manager, engine, _, opener := newAdoptionManagerSaving(t, downloadsAsArtifacts)
			before := manager.artifactBytes.Used()
			file := engineArtifact(t, manager, "dl-1")
			popupDownload(manager, engine, "popup", "dl-1", opener)
			manager.downloadProgress(progressOf("dl-1", state, 100))
			if err := manager.closeBrowser(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the parked download's file survives the browser's close: %v", err)
			}
			if got := manager.artifactBytes.Used(); got != before {
				t.Fatalf("after the close %d artifact bytes are reserved, want %d", got, before)
			}
		})
	}
}

// The fates the Manager remembers are bounded.
func TestPopupFatesAreBounded(t *testing.T) {
	manager, _, _, opener := newAdoptionManager(t)
	for i := range 4 * orderSlack {
		manager.resolveParked(ids.PageHandle(fmt.Sprintf("popup-%d", i)), nil, opener)
	}
	manager.parkMu.Lock()
	defer manager.parkMu.Unlock()
	if n := manager.popupFates.len(); n > orderSlack {
		t.Fatalf("%d fates kept", n)
	}
}

// A refused popup lets go of the address and title held for it, and one
// reported after its refusal is not held: however many popups a thread
// has refused, another popup's held report survives to its listing.
func TestARefusedPopupHoldsNoReport(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	manager.updatePageInfo("keep", "https://example.test/kept", "Kept", 1)
	for i := range 2 * orderSlack {
		handle := ids.PageHandle(fmt.Sprintf("refused-%d", i))
		engine.failing[handle] = true
		manager.updatePageInfo(handle, "https://example.test/refused", "", uint64(2+i))
		manager.adoptPopup(engine.popup(opener, handle))
		manager.updatePageInfo(handle, "https://example.test/late", "", uint64(2+i))
	}
	manager.mu.Lock()
	held := manager.heldInfo.len()
	manager.mu.Unlock()
	if held != 1 {
		t.Fatalf("%d reports held, want the pending popup's alone", held)
	}
	manager.adoptPopup(engine.popup(opener, "keep"))
	if info := pageInfoOf(manager, access, "keep"); info.URL != "https://example.test/kept" || info.Title != "Kept" {
		t.Fatalf("the popup lists %+v, want its held report", info)
	}
}

// A popup listed with a report held for it is pushed once, with that
// report.
func TestAPopupListedWithAHeldReportIsPushedOnce(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	var mu sync.Mutex
	var pushed []CompanionEvent
	manager.SetEventSink(func(event CompanionEvent) {
		if ThreadID(event.ThreadID) == access.ThreadID() && event.Kind == "state" {
			mu.Lock()
			pushed = append(pushed, event)
			mu.Unlock()
		}
	})
	manager.updatePageInfo("popup", "https://example.test/held", "Held", 1)
	manager.adoptPopup(engine.popup(opener, "popup"))
	mu.Lock()
	defer mu.Unlock()
	if len(pushed) != 1 {
		t.Fatalf("listing the popup pushed %d states, want 1", len(pushed))
	}
	for _, page := range pushed[0].Pages {
		if page.URL == "https://example.test/held" && page.Title == "Held" {
			return
		}
	}
	t.Fatalf("the push lists %+v, without the popup's held report", pushed[0].Pages)
}

// A popup that navigated before its listing is listed, in its one push,
// with Back as its history has it.
func TestAPopupIsListedWithItsHistory(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	engine.histories["popup"] = []string{"https://example.test/first", "https://example.test/second"}
	var mu sync.Mutex
	var pushed []CompanionEvent
	manager.SetEventSink(func(event CompanionEvent) {
		if ThreadID(event.ThreadID) == access.ThreadID() && event.Kind == "state" {
			mu.Lock()
			pushed = append(pushed, event)
			mu.Unlock()
		}
	})
	manager.updatePageInfo("popup", "https://example.test/second", "Second", 1)
	manager.adoptPopup(engine.popup(opener, "popup"))
	mu.Lock()
	defer mu.Unlock()
	if len(pushed) != 1 {
		t.Fatalf("listing the popup pushed %d states, want 1", len(pushed))
	}
	for _, page := range pushed[0].Pages {
		if page.URL == "https://example.test/second" {
			if !page.CanGoBack || page.CanGoForward {
				t.Fatalf("the popup is listed with back %v, forward %v", page.CanGoBack, page.CanGoForward)
			}
			return
		}
	}
	t.Fatalf("the push lists %+v, without the popup", pushed[0].Pages)
}

// pageInfoOf answers the info of the tab whose driver has handle.
func pageInfoOf(m *Manager, access Access, handle ids.PageHandle) PageInfo {
	m.mu.Lock()
	p := m.listedPageLocked(handle)
	m.mu.Unlock()
	if p == nil {
		return PageInfo{}
	}
	for _, page := range m.CompanionState(access.Caller()).Pages {
		if PageID(page.ID) == p.id {
			return page
		}
	}
	return PageInfo{}
}

// A popup being adopted holds its jar's profile: its opener's close, the
// jar's last listed page, leaves the profile running until the adoption
// ends, which then disposes it.
func TestAPopupBeingAdoptedHoldsItsProfile(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	manager.mu.Lock()
	var profile *adoptionProfile
	for _, scope := range manager.scopes {
		profile = scope.profile.(*adoptionProfile)
	}
	manager.mu.Unlock()
	disposed := func() bool {
		profile.mu.Lock()
		defer profile.mu.Unlock()
		return profile.disposed
	}
	release := engine.gate("popup")
	defer release()
	done := make(chan struct{})
	go func() { manager.adoptPopup(engine.popup(opener, "popup")); close(done) }()
	engine.awaitAttach(t, "popup")
	tabs := manager.CompanionState(access.Caller()).Pages
	if len(tabs) != 1 {
		t.Fatalf("the thread has %d tabs, want the opener", len(tabs))
	}
	if err := manager.ClosePage(t.Context(), access, PageID(tabs[0].ID)); err != nil {
		t.Fatal(err)
	}
	if disposed() {
		t.Fatal("the opener's close disposed the profile the popup is being attached in")
	}
	release()
	<-done
	if listed(manager, "popup") {
		t.Fatal("a popup whose opener closed was listed")
	}
	deadline := time.Now().Add(hostedTestDeadline)
	for !disposed() {
		if time.Now().After(deadline) {
			t.Fatal("the profile was never disposed once the adoption ended")
		}
		time.Sleep(time.Millisecond)
	}
	manager.mu.Lock()
	scopes := len(manager.scopes)
	manager.mu.Unlock()
	if scopes != 0 {
		t.Fatalf("%d jars are still registered", scopes)
	}
}

// A popup being adopted when its opener, the thread's last private tab,
// closes can no longer be listed, so it keeps the closed tab's private jar
// from no new tab: the thread's next private tab opens a new jar, and the
// old profile stays until the adoption ends.
func TestAPopupBeingAdoptedKeepsNoPrivateJarForANewTab(t *testing.T) {
	manager, engine, access, _ := newAdoptionManager(t)
	setJar(t, manager, access, JarPrivate)
	first := openPage(t, manager, access, "https://example.test/private")
	old := pageJar(t, manager, first.ID)
	manager.mu.Lock()
	oldScope := manager.scopes[jarKey(old)]
	manager.mu.Unlock()
	release := engine.gate("popup")
	defer release()
	done := make(chan struct{})
	go func() {
		manager.adoptPopup(engine.popup(driverHandle(manager, access, first.ID), "popup"))
		close(done)
	}()
	engine.awaitAttach(t, "popup")
	if err := manager.ClosePage(t.Context(), access, PageID(first.ID)); err != nil {
		t.Fatal(err)
	}
	next := pageJar(t, manager, openPage(t, manager, access, "https://example.test/next").ID)
	if next == old {
		t.Fatalf("a private tab opened while a popup of the closed one was adopted reuses its jar %q", old)
	}
	if oldScope.profile.(*adoptionProfile).isDisposed() {
		t.Fatal("the old private jar was disposed while a popup was being attached in it")
	}
	release()
	<-done
	if listed(manager, "popup") {
		t.Fatal("a popup whose opener closed was listed")
	}
	eventually(t, "the old private jar is disposed", oldScope.profile.(*adoptionProfile).isDisposed)
}

func (p *adoptionProfile) isDisposed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.disposed
}

// Popups being adopted count against the caps from their admission: a
// burst of them past the thread's cap is refused at once rather than
// attached and dropped at its listing.
func TestPopupsBeingAdoptedCountAgainstTheCaps(t *testing.T) {
	manager, engine, access, opener := newAdoptionManager(t)
	var releases []func()
	var dones []chan struct{}
	for i := range maxPagesPerThread {
		handle := ids.PageHandle(fmt.Sprintf("popup-%d", i))
		releases = append(releases, engine.gate(handle))
		done := make(chan struct{})
		dones = append(dones, done)
		go func() { manager.adoptPopup(engine.popup(opener, handle)); close(done) }()
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	// The opener is one page, so all but one popup is admitted.
	for range maxPagesPerThread - 1 {
		select {
		case <-engine.reached:
		case <-time.After(hostedTestDeadline):
			t.Fatal("an admitted popup was never attached")
		}
	}
	deadline := time.Now().Add(hostedTestDeadline)
	for len(engine.discards()) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("discarded %v while the others are attached, want the one past the cap", engine.discards())
		}
		time.Sleep(time.Millisecond)
	}
	for _, release := range releases {
		release()
	}
	for _, done := range dones {
		<-done
	}
	if got := len(manager.CompanionState(access.Caller()).Pages); got != maxPagesPerThread {
		t.Fatalf("the thread has %d tabs, want its cap of %d", got, maxPagesPerThread)
	}
}

// A popup whose discard the engine never answers ends its adoption at the
// discard's deadline: its reservation goes, so its profile and the caps
// are free again, and its downloads go to its opener.
func TestAPopupWhoseDiscardNeverAnswersEndsItsReservation(t *testing.T) {
	manager, engine, _, opener := newAdoptionManager(t)
	engine.failing["popup"] = true
	engine.stallDiscard = true
	popupDownload(manager, engine, "popup", "dl-1")
	done := make(chan struct{})
	go func() {
		manager.adoptPopup(engine.popup(opener, "popup"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(popupDiscardTimeout + hostedTestDeadline):
		t.Fatal("the adoption never ended")
	}
	manager.mu.Lock()
	reserved := 0
	for _, scope := range manager.scopes {
		reserved += len(scope.creating)
	}
	adopting := len(manager.adopting)
	manager.mu.Unlock()
	if reserved != 0 || adopting != 0 {
		t.Fatalf("%d reservations and %d adoptions remain, want none", reserved, adopting)
	}
	wantDownloads(t, manager, opener, "dl-1 in_progress 0")
}
