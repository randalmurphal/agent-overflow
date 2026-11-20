package browser

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Fixtures the download tests share: a Manager on the fake engine with a
// Downloads folder of the test's own, the events it emits, and ways to
// start, find and inspect its downloads.

// downloadEventLog records what the Manager emits.
type downloadEventLog struct {
	mu     sync.Mutex
	events []CompanionEvent
}

func (l *downloadEventLog) sink(event CompanionEvent) {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}
func (l *downloadEventLog) take() []CompanionEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.events
	l.events = nil
	return out
}

func countKind(events []CompanionEvent, kind string) int {
	n := 0
	for _, event := range events {
		if event.Kind == kind {
			n++
		}
	}
	return n
}

// downloadModes are the three ways an engine's downloads are saved
// (downloadSaving): staged in AO's directory and published to the user's
// Downloads folder (WebKitGTK, WKWebView), saved there by the engine
// (WebView2), and kept as AO's artifacts (headless Chromium).
var downloadModes = []struct {
	name   string
	saving downloadSaving
}{{"staged", downloadsStaged}, {"saved by the engine", downloadsSavedByEngine}, {"artifacts", downloadsAsArtifacts}}

// newDownloadManager opens a page on a fake engine standing in for an
// engine in this process, whose downloads are published to a Downloads
// folder of the test's own.
func newDownloadManager(t *testing.T) (*Manager, *downloadEventLog, Access, PageInfo) {
	t.Helper()
	return newDownloadManagerFor(t, downloadsStaged)
}

// newDownloadManagerFor is newDownloadManager, standing in for an engine
// that saves as saving says.
func newDownloadManagerFor(t *testing.T, saving downloadSaving) (*Manager, *downloadEventLog, Access, PageInfo) {
	t.Helper()
	testDownloadsFolder(t)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	manager.engine.(*fakeEngine).saving = saving
	log := &downloadEventLog{}
	manager.SetEventSink(log.sink)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page, err := manager.Open(t.Context(), access, "https://example.test/files", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	log.take()
	return manager, log, access, page
}

// testDownloadsFolder gives the test a home of its own with an empty
// Downloads folder, and answers that folder, links resolved.
func testDownloadsFolder(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	downloads := filepath.Join(home, "Downloads")
	if err := os.Mkdir(downloads, 0o700); err != nil {
		t.Fatal(err)
	}
	return downloads
}

// heldFile is the file the fake engine is writing a held download into.
func heldFile(t *testing.T, manager *Manager, engineID downloadID) string {
	t.Helper()
	profile := fakeProfileOf(t, manager)
	profile.mu.Lock()
	defer profile.mu.Unlock()
	held, ok := profile.held[engineID]
	if !ok {
		t.Fatalf("the engine holds no download %s", engineID)
	}
	return held.file
}

// fakeDownloadURL is the fake engine's download address for a file of
// name and bytes, with flags ("hold", "fail") as query keys.
func fakeDownloadURL(name string, bytes int, flags ...string) string {
	query := url.Values{"bytes": {fmt.Sprint(bytes)}}
	for _, flag := range flags {
		query.Set(flag, "")
	}
	return "https://" + fakeDownloadHost + "/" + name + "?" + query.Encode()
}

func startDownload(t *testing.T, manager *Manager, access Access, page PageInfo, name string, bytes int, flags ...string) {
	t.Helper()
	if _, err := manager.Open(t.Context(), access, fakeDownloadURL(name, bytes, flags...), OpenOptions{PageID: PageID(page.ID)}); err != nil {
		t.Fatal(err)
	}
}

func threadDownloads(manager *Manager, access Access) []ThreadDownload {
	return manager.CompanionState(access.Caller()).Downloads
}

func authorityOf(t *testing.T, manager *Manager, access Access) *workspaceAuthority {
	t.Helper()
	workspace, err := canonicalRoot(access.Workspace())
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.authorities[workspace]
}

func fakeProfileOf(t *testing.T, manager *Manager) *fakeProfile {
	t.Helper()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, scope := range manager.scopes {
		if profile, ok := scope.profile.(*fakeProfile); ok {
			return profile
		}
	}
	t.Fatal("no fake profile")
	return nil
}
