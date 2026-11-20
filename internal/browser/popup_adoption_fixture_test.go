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

func (p *adoptionProfile) isDisposed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.disposed
}

// handledPage is a fake page under the handle its popup was reported with.
type handledPage struct {
	pageDriver
	handle ids.PageHandle
}

func (p *handledPage) Handle() ids.PageHandle { return p.handle }

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
	p := m.pageByHandleLocked(handle)
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
	return m.pageByHandleLocked(handle) != nil
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

// pageInfoOf answers the info of the tab whose driver has handle.
func pageInfoOf(m *Manager, access Access, handle ids.PageHandle) PageInfo {
	m.mu.Lock()
	p := m.pageByHandleLocked(handle)
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
