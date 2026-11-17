package browser

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

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

func newDownloadManager(t *testing.T) (*Manager, *downloadEventLog, Access, PageInfo) {
	t.Helper()
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	log := &downloadEventLog{}
	manager.SetEventSink(log.sink)
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page, err := manager.Open(t.Context(), access, "https://example.test/files", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	log.take()
	return manager, log, access, page
}

// fakeDownload navigates the page to the fake engine's download address.
func fakeDownloadURL(name string, bytes int, flags ...string) string {
	query := url.Values{"bytes": {fmt.Sprint(bytes)}}
	for _, flag := range flags {
		query.Set(flag, "")
	}
	return "https://" + fakeDownloadHost + "/" + name + "?" + query.Encode()
}

func startDownload(t *testing.T, manager *Manager, access Access, page PageInfo, name string, bytes int, flags ...string) {
	t.Helper()
	if _, err := manager.Open(t.Context(), access, fakeDownloadURL(name, bytes, flags...), OpenOptions{PageID: page.ID}); err != nil {
		t.Fatal(err)
	}
}

func threadDownloads(manager *Manager, access Access) []ThreadDownload {
	return manager.CompanionState(access).Downloads
}

func authorityOf(t *testing.T, manager *Manager, access Access) *workspaceAuthority {
	t.Helper()
	workspace, err := canonicalRoot(access.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.authorities[workspace]
}

// A finished download is in the thread's state with its source page, size
// and file name, its file is in the workspace's download directory, and it
// is announced exactly once, after the state that lists it.
func TestAFinishedDownloadIsListedAndAnnouncedOnce(t *testing.T) {
	manager, log, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "report.pdf", 2048)

	listed := threadDownloads(manager, access)
	if len(listed) != 1 {
		t.Fatalf("the thread lists %d downloads, want 1", len(listed))
	}
	got := listed[0]
	want := ThreadDownload{ID: got.ID, Name: "report.pdf", Source: "https://example.test/files", State: downloadStateCompleted, ReceivedBytes: 2048, TotalBytes: 2048, Openable: true}
	if got != want {
		t.Fatalf("the thread lists %+v, want %+v", got, want)
	}
	item, err := manager.threadDownload(access.ThreadID, got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !pathInside(authorityOf(t, manager, access).downloadDir, item.path) {
		t.Fatalf("the finished file %s is outside the workspace's download directory", item.path)
	}
	events := log.take()
	if n := countKind(events, "download"); n != 1 {
		t.Fatalf("the download was announced %d times, want once", n)
	}
	at := slices.IndexFunc(events, func(event CompanionEvent) bool { return event.Kind == "download" })
	announced := events[at]
	if announced.Download == nil || *announced.Download != want || announced.ThreadID != access.ThreadID {
		t.Fatalf("the announcement is %+v, want %+v in thread %q", announced, want, access.ThreadID)
	}
	if at == 0 || events[at-1].Kind != "state" || len(events[at-1].Downloads) != 1 || events[at-1].Downloads[0] != want {
		t.Fatalf("the announcement is not preceded by the state that lists it: %+v", events[:at])
	}

	// browser_downloads still lists the page's download, now with its size.
	tool, err := manager.Downloads(t.Context(), access, DownloadOptions{PageID: page.ID})
	if err != nil {
		t.Fatal(err)
	}
	entries := tool.([]DownloadInfo)
	if len(entries) != 1 || entries[0].State != downloadStateCompleted || entries[0].TotalBytes != 2048 || entries[0].Path != item.path {
		t.Fatalf("browser_downloads lists %+v", entries)
	}
}

// Byte progress is coalesced: a start pushes the thread's state at once,
// and any number of progress reports within the interval make one push.
func TestDownloadProgressCoalescesIntoOneStatePush(t *testing.T) {
	manager, log, access, page := newDownloadManager(t)
	manager.downloadPushInterval = 50 * time.Millisecond
	frame := paneHandle(manager, access, page.ID)
	if !manager.downloadStarted(downloadStart{Frame: frame, ID: "G", SuggestedName: "big.bin"}) {
		t.Fatal("the download was refused")
	}
	if n := countKind(log.take(), "state"); n != 1 {
		t.Fatalf("a start pushed %d states, want 1", n)
	}
	for i := 1; i <= 200; i++ {
		manager.downloadProgress(downloadProgress{ID: "G", Received: float64(i * 1000), Total: 400_000, State: downloadInProgress})
	}
	if n := countKind(log.take(), "state"); n != 0 {
		t.Fatalf("progress pushed %d states before the interval, want 0", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	var pushed []CompanionEvent
	for time.Now().Before(deadline) && countKind(pushed, "state") == 0 {
		time.Sleep(10 * time.Millisecond)
		pushed = append(pushed, log.take()...)
	}
	time.Sleep(150 * time.Millisecond)
	pushed = append(pushed, log.take()...)
	if n := countKind(pushed, "state"); n != 1 {
		t.Fatalf("200 progress reports pushed %d states, want 1", n)
	}
	downloads := pushed[0].Downloads
	if len(downloads) != 1 || downloads[0].ReceivedBytes != 200_000 || downloads[0].TotalBytes != 400_000 {
		t.Fatalf("the coalesced push carries %+v, want the newest progress", downloads)
	}

	// A finish pushes at once and withdraws a pending progress push.
	manager.downloadPushInterval = time.Hour
	manager.downloadProgress(downloadProgress{ID: "G", Received: 300_000, Total: 400_000, State: downloadInProgress})
	manager.downloadProgress(downloadProgress{ID: "G", Received: 300_000, Total: 400_000, State: downloadCanceled, Error: "net::ERR_FAILED"})
	events := log.take()
	if countKind(events, "state") != 1 || countKind(events, "download") != 1 {
		t.Fatalf("a failure emitted %+v, want one state and one announcement", events)
	}
	manager.downloadsMu.Lock()
	pending := manager.threadDownloads[access.ThreadID].push
	manager.downloadsMu.Unlock()
	if pending != nil {
		t.Fatal("a progress push is still pending after the failure pushed the state")
	}
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateFailed || got.Error != "net::ERR_FAILED" {
		t.Fatalf("an engine failure lists as %+v, want failed with the engine's reason", got)
	}
}

// The user's cancel stops a running download, keeps no partial file, gives
// the reservation back, and is not announced.
func TestCancelingADownloadSettlesItAsCanceled(t *testing.T) {
	manager, log, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "big.zip", 4096, "hold")
	listed := threadDownloads(manager, access)
	if len(listed) != 1 || listed[0].State != downloadStateRunning || listed[0].ReceivedBytes != 2048 || listed[0].TotalBytes != 4096 {
		t.Fatalf("a held download lists as %+v", listed)
	}
	item, err := manager.threadDownload(access.ThreadID, listed[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	p, scope := manager.pageForDownload(item.engineID)
	partial := filepath.Join(scope.downloadDir, item.engineID)
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("the held download has no partial file: %v", err)
	}
	log.take()

	if err := manager.CancelThreadDownload(access, listed[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateCanceled || got.Error != "" {
		t.Fatalf("a canceled download lists as %+v", got)
	}
	if n := countKind(log.take(), "download"); n != 0 {
		t.Fatalf("the user's cancel was announced %d times", n)
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the partial file is still there: %v", err)
	}
	if reserved := p.authority.downloadBytes.Load(); reserved != 0 {
		t.Fatalf("the workspace still reserves %d bytes", reserved)
	}
	// A second cancel of a settled download is no error and changes nothing.
	if err := manager.CancelThreadDownload(access, listed[0].ID); err != nil {
		t.Fatalf("a repeated cancel: %v", err)
	}
	if err := manager.CancelThreadDownload(access, "d-nonexistent"); !errors.Is(err, errDownloadNotFound) {
		t.Fatalf("a cancel of an unknown download answers %v", err)
	}
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

// A cancel that reaches the engine after it let the finished download go,
// and before it reported the finish, loses: the finish settles the download
// and its file is kept. The engine's errDownloadEnded is that answer.
func TestACancelRacingTheFinishKeepsTheFinishedFile(t *testing.T) {
	manager, log, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "late.bin", 4096, "hold")
	listed := threadDownloads(manager, access)
	item, err := manager.threadDownload(access.ThreadID, listed[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	log.take()
	engine := manager.engine.(*fakeEngine)
	engine.downloadReleased = func(string) {
		if err := manager.CancelThreadDownload(access, listed[0].ID); err != nil {
			t.Errorf("a cancel that lost to the finish answers %v", err)
		}
		if got := threadDownloads(manager, access)[0]; got.State != downloadStateRunning {
			t.Errorf("a cancel that lost settled the download as %+v before the engine's report", got)
		}
	}
	if err := fakeProfileOf(t, manager).finishHeld(item.engineID); err != nil {
		t.Fatal(err)
	}
	got := threadDownloads(manager, access)[0]
	if got.State != downloadStateCompleted || got.ReceivedBytes != 4096 {
		t.Fatalf("the download lists as %+v, want completed", got)
	}
	finished, err := manager.finishedDownloadPath(access, got.ID)
	if err != nil {
		t.Fatalf("the finished file: %v", err)
	}
	if stat, err := os.Stat(finished.path); err != nil || stat.Size() != 4096 {
		t.Fatalf("the finished file at %s: %v", finished.path, err)
	}
	if n := countKind(log.take(), "download"); n != 1 {
		t.Fatalf("the finish that beat the cancel was announced %d times, want once", n)
	}
}

type cancelRefusingEngine struct {
	*fakeEngine
	refuse error
}

func (e *cancelRefusingEngine) NewProfile(ctx context.Context, opts profileOptions) (engineProfile, error) {
	profile, err := e.fakeEngine.NewProfile(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &cancelRefusingProfile{fakeProfile: profile.(*fakeProfile), refuse: e.refuse}, nil
}

type cancelRefusingProfile struct {
	*fakeProfile
	refuse error
}

func (p *cancelRefusingProfile) CancelDownload(string) error { return p.refuse }

// An engine that refuses the cancel answers the user with its refusal, and
// the download stays running: the engine's later report of its end is a
// failure, not the user's cancel.
func TestARefusedCancelReachesTheUserAndTheDownloadRuns(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	engine := &cancelRefusingEngine{fakeEngine: manager.engine.(*fakeEngine), refuse: errors.New("Browser.cancelDownload wasn't found")}
	manager.engine = engine
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page, err := manager.Open(t.Context(), access, "https://example.test/", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	frame := paneHandle(manager, access, page.ID)
	manager.downloadStarted(downloadStart{Frame: frame, ID: "G", SuggestedName: "a.bin"})
	id := threadDownloads(manager, access)[0].ID

	err = manager.CancelThreadDownload(access, id)
	if err == nil || !strings.Contains(err.Error(), "cancelDownload wasn't found") {
		t.Fatalf("a refused cancel answers %v, want the engine's refusal", err)
	}
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateRunning {
		t.Fatalf("after a refused cancel the download lists as %+v, want running", got)
	}
	manager.downloadProgress(downloadProgress{ID: "G", State: downloadCanceled})
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateFailed {
		t.Fatalf("an end after a refused cancel lists as %+v, want failed", got)
	}
}

// A download refused for quota, and one stopped by its page closing, are
// listed with the reason and announced.
func TestDownloadsAOStoppedAreListedWithTheirReason(t *testing.T) {
	manager, log, access, page := newDownloadManager(t)
	frame := paneHandle(manager, access, page.ID)
	authorityOf(t, manager, access).downloadBytes.Store(maxWorkspaceDownloadBytes)
	if manager.downloadStarted(downloadStart{Frame: frame, ID: "over", SuggestedName: "over.bin"}) {
		t.Fatal("a download over quota was kept")
	}
	authorityOf(t, manager, access).downloadBytes.Store(0)
	if !manager.downloadStarted(downloadStart{Frame: frame, ID: "kept", SuggestedName: "kept.bin"}) {
		t.Fatal("a download within quota was refused")
	}
	log.take()
	if err := manager.ClosePage(t.Context(), access, page.ID); err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, download := range threadDownloads(manager, access) {
		reasons = append(reasons, download.Name+": "+download.State+": "+download.Error)
	}
	want := []string{
		"kept.bin: canceled: page closed",
		fmt.Sprintf("over.bin: canceled: workspace downloads exceed %d bytes", maxWorkspaceDownloadBytes),
	}
	if !slices.Equal(reasons, want) {
		t.Fatalf("the thread lists %q, want %q (newest first, after its page closed)", reasons, want)
	}
	if n := countKind(log.take(), "download"); n != 1 {
		t.Fatalf("the page close announced %d downloads, want 1", n)
	}
}

// Clearing the list drops settled downloads, keeps a running one, and
// deletes no file.
func TestClearingDownloadsKeepsRunningOnesAndFiles(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "done.txt", 10)
	done := threadDownloads(manager, access)[0]
	item, err := manager.threadDownload(access.ThreadID, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "running.bin", 10, "hold")
	manager.ClearThreadDownloads(access)
	listed := threadDownloads(manager, access)
	if len(listed) != 1 || listed[0].Name != "running.bin" {
		t.Fatalf("after clearing the thread lists %+v, want only the running download", listed)
	}
	if _, err := os.Stat(item.path); err != nil {
		t.Fatalf("clearing the list deleted the file: %v", err)
	}
	// The cleared download takes no later change back into the list.
	manager.ClearThreadDownloads(access)
	if len(threadDownloads(manager, access)) != 1 {
		t.Fatal("a second clear dropped the running download")
	}
}

// The thread's list keeps maxThreadDownloads, dropping the oldest settled
// download first and never a running one.
func TestTheThreadListIsBoundedAndKeepsRunningDownloads(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	frame := paneHandle(manager, access, page.ID)
	manager.downloadStarted(downloadStart{Frame: frame, ID: "first", SuggestedName: "first.bin"})
	for i := 0; i < maxThreadDownloads+5; i++ {
		startDownload(t, manager, access, page, fmt.Sprintf("f%d.txt", i), 1)
	}
	listed := threadDownloads(manager, access)
	if len(listed) != maxThreadDownloads {
		t.Fatalf("the thread lists %d downloads, want %d", len(listed), maxThreadDownloads)
	}
	if listed[0].Name != fmt.Sprintf("f%d.txt", maxThreadDownloads+4) {
		t.Fatalf("the newest download is %q, want it first", listed[0].Name)
	}
	if last := listed[len(listed)-1]; last.Name != "first.bin" || last.State != downloadStateRunning {
		t.Fatalf("the oldest listed is %+v, want the running first download kept", last)
	}
}

// A page's own log drops its oldest settled entries past maxPageDownloads,
// never a running one, whose progress must still find it.
func TestThePageLogKeepsARunningDownloadPastItsBound(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	frame := paneHandle(manager, access, page.ID)
	manager.downloadStarted(downloadStart{Frame: frame, ID: "running", SuggestedName: "r.bin"})
	for i := 0; i < maxPageDownloads; i++ {
		id := fmt.Sprintf("done-%d", i)
		manager.downloadStarted(downloadStart{Frame: frame, ID: id, SuggestedName: id})
		manager.downloadProgress(downloadProgress{ID: id, State: downloadCanceled})
	}
	manager.downloadProgress(downloadProgress{ID: "running", Received: 5, State: downloadInProgress})
	tool, err := manager.Downloads(t.Context(), access, DownloadOptions{PageID: page.ID})
	if err != nil {
		t.Fatal(err)
	}
	entries := tool.([]DownloadInfo)
	if len(entries) != maxPageDownloads || entries[0].ID != "running" || entries[0].Bytes != 5 {
		t.Fatalf("the page log holds %d entries starting %+v, want %d with the running one first and current", len(entries), entries[0], maxPageDownloads)
	}
}

// Show in folder hands the finished file to the file manager; a file pruned
// since is an error naming it.
func TestRevealingADownloadShowsItsFile(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	var revealed []string
	manager.revealFileInFileManager = func(_ context.Context, path string) error {
		revealed = append(revealed, path)
		return nil
	}
	startDownload(t, manager, access, page, "notes.txt", 10)
	download := threadDownloads(manager, access)[0]
	item, _ := manager.threadDownload(access.ThreadID, download.ID)
	if err := manager.RevealThreadDownload(t.Context(), access, download.ID); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(revealed, []string{item.path}) {
		t.Fatalf("revealed %q, want %q", revealed, item.path)
	}
	if err := os.Remove(item.path); err != nil {
		t.Fatal(err)
	}
	if err := manager.RevealThreadDownload(t.Context(), access, download.ID); err == nil || !strings.Contains(err.Error(), "notes.txt is gone") {
		t.Fatalf("revealing a pruned download answers %v", err)
	}
	// Another thread cannot reach this thread's download.
	other := Access{ThreadID: "other", Workspace: access.Workspace}
	if err := manager.RevealThreadDownload(t.Context(), other, download.ID); !errors.Is(err, errDownloadNotFound) {
		t.Fatalf("another thread's reveal answers %v", err)
	}
}

// A row acts only on the file its download wrote. Moving a download out
// frees its name for the next one, so the two rows share a path: the older
// row refuses the newer file, and once the older file is back in its place
// the newer row refuses it.
func TestADownloadRowActsOnlyOnItsOwnFile(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	var revealed []string
	manager.revealFileInFileManager = func(_ context.Context, path string) error {
		revealed = append(revealed, path)
		return nil
	}
	act := func(id string) error {
		revealErr := manager.RevealThreadDownload(t.Context(), access, id)
		_, openErr := manager.OpenThreadDownload(t.Context(), access, id)
		if (revealErr == nil) != (openErr == nil) {
			t.Fatalf("Show in folder answers %v and Open %v", revealErr, openErr)
		}
		return revealErr
	}
	replaced := func(err error) bool { return err != nil && strings.Contains(err.Error(), "moved or replaced") }

	startDownload(t, manager, access, page, "report.pdf", 10)
	older := threadDownloads(manager, access)[0]
	olderItem, _ := manager.threadDownload(access.ThreadID, older.ID)
	moved := olderItem.path + ".moved"
	if err := os.Rename(olderItem.path, moved); err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "report.pdf", 20)
	newer := threadDownloads(manager, access)[0]
	newerItem, _ := manager.threadDownload(access.ThreadID, newer.ID)
	if newerItem.path != olderItem.path {
		t.Fatalf("the second download is at %s, not the freed name %s", newerItem.path, olderItem.path)
	}

	if err := act(older.ID); !replaced(err) {
		t.Fatalf("the older row acting on the newer file answers %v", err)
	}
	if err := act(newer.ID); err != nil {
		t.Fatalf("the newer row: %v", err)
	}

	if err := os.Rename(newerItem.path, newerItem.path+".newer"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, olderItem.path); err != nil {
		t.Fatal(err)
	}
	if err := act(newer.ID); !replaced(err) {
		t.Fatalf("the newer row acting on the older file answers %v", err)
	}
	if err := act(older.ID); err != nil {
		t.Fatalf("the older row with its own file back: %v", err)
	}
	if !slices.Equal(revealed, []string{newerItem.path, olderItem.path}) {
		t.Fatalf("revealed %q", revealed)
	}
}

// Open shows a finished download of a type tabs show in a new tab; a page
// may open its own workspace's downloads and no other workspace's.
func TestOpeningADownloadShowsItInANewTab(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "chart.png", 10)
	startDownload(t, manager, access, page, "page.html", 10)
	listed := threadDownloads(manager, access)
	html, png := listed[0], listed[1]
	if html.Openable || !png.Openable {
		t.Fatalf("openable: html=%v png=%v, want only the image", html.Openable, png.Openable)
	}
	opened, err := manager.OpenThreadDownload(t.Context(), access, png.ID)
	if err != nil {
		t.Fatal(err)
	}
	item, _ := manager.threadDownload(access.ThreadID, png.ID)
	if opened.ID == page.ID || !strings.HasSuffix(opened.URL, filepath.ToSlash(item.path)) {
		t.Fatalf("the download opened as %+v, want a new tab at %s", opened, item.path)
	}
	if _, err := manager.OpenThreadDownload(t.Context(), access, html.ID); err == nil {
		t.Fatal("an HTML download opened in a tab")
	}

	otherWorkspace := Access{ThreadID: "other", Workspace: t.TempDir()}
	if _, err := manager.authorizeFile(otherWorkspace, item.path); err == nil {
		t.Fatal("another workspace may open this workspace's download")
	}
	if _, err := manager.authorizeFile(access, item.path); err != nil {
		t.Fatalf("the workspace may not open its own download: %v", err)
	}
}

// Deleting the thread forgets its downloads.
func TestClosingTheThreadForgetsItsDownloads(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "a.txt", 1)
	if err := manager.CloseThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	manager.downloadsMu.Lock()
	_, kept := manager.threadDownloads[access.ThreadID]
	manager.downloadsMu.Unlock()
	if kept {
		t.Fatal("the closed thread's downloads are still kept")
	}
}

// However a page stops being the Manager's, a download it left running
// settles, its reservations are released, the engine is asked to stop it,
// and its row can be cleared.
func TestEveryPageTeardownSettlesItsRunningDownloads(t *testing.T) {
	for _, tc := range []struct {
		name     string
		teardown func(t *testing.T, manager *Manager, access Access, page PageInfo)
		listGone bool
	}{
		{"clear site data", func(t *testing.T, manager *Manager, _ Access, _ PageInfo) {
			if err := manager.ClearSiteData(t.Context()); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"disable", func(t *testing.T, manager *Manager, _ Access, _ PageInfo) {
			if err := manager.Reconfigure(Config{Enabled: false}); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"persistence change", func(t *testing.T, manager *Manager, _ Access, _ PageInfo) {
			if err := manager.Reconfigure(Config{Enabled: true, PersistSiteData: true}); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"engine page closed", func(t *testing.T, manager *Manager, _ Access, page PageInfo) {
			manager.mu.Lock()
			p, _ := manager.livePageLocked(page.ID)
			manager.mu.Unlock()
			manager.engine.(*fakeEngine).events.PageClosed(p.driver.Handle())
		}, false},
		{"page crashed", func(t *testing.T, manager *Manager, _ Access, page PageInfo) {
			manager.mu.Lock()
			p, _ := manager.livePageLocked(page.ID)
			manager.mu.Unlock()
			manager.engine.(*fakeEngine).events.PageCrashed(p.driver.Handle(), "killed")
		}, false},
		{"close the page", func(t *testing.T, manager *Manager, access Access, page PageInfo) {
			if err := manager.closeLivePage(t.Context(), access, page.ID); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"thread close", func(t *testing.T, manager *Manager, access Access, _ PageInfo) {
			if err := manager.CloseThread(t.Context(), access.ThreadID); err != nil {
				t.Fatal(err)
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, _, access, page := newDownloadManager(t)
			baseline := manager.artifactBytes.Load()
			startDownload(t, manager, access, page, "held.bin", 4096, "hold")
			authority := authorityOf(t, manager, access)
			item, err := manager.threadDownload(access.ThreadID, threadDownloads(manager, access)[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			profile := fakeProfileOf(t, manager)

			tc.teardown(t, manager, access, page)

			deadline := time.Now().Add(3 * time.Second)
			for {
				listed := threadDownloads(manager, access)
				running := slices.ContainsFunc(listed, func(d ThreadDownload) bool { return d.State == downloadStateRunning })
				settled := (tc.listGone && len(listed) == 0) || (!tc.listGone && len(listed) == 1 && listed[0].State == downloadStateCanceled)
				profile.mu.Lock()
				_, held := profile.held[item.engineID]
				profile.mu.Unlock()
				if !running && settled && !held && authority.downloadBytes.Load() == 0 && manager.artifactBytes.Load() == baseline {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("after the teardown the thread lists %+v, the engine holds it: %v, the workspace reserves %d bytes and the artifacts %d over %d", listed, held, authority.downloadBytes.Load(), manager.artifactBytes.Load(), baseline)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !tc.listGone {
				manager.ClearThreadDownloads(access)
				if listed := threadDownloads(manager, access); len(listed) != 0 {
					t.Fatalf("Clear list keeps %+v", listed)
				}
			}
		})
	}
}
