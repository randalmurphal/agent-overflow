package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
)

// A finished download is in the thread's state with its source page, size
// and file name, its file is in the user's Downloads folder or, on an
// engine whose downloads are AO's, the workspace's download directory, and
// it is announced exactly once, after the state that lists it.
func TestAFinishedDownloadIsListedAndAnnouncedOnce(t *testing.T) {
	for _, mode := range downloadModes {
		t.Run(mode.name, func(t *testing.T) {
			testAFinishedDownloadIsListedAndAnnouncedOnce(t, mode.saving)
		})
	}
}

func testAFinishedDownloadIsListedAndAnnouncedOnce(t *testing.T, saving downloadSaving) {
	manager, log, access, page := newDownloadManagerFor(t, saving)
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
	item, err := manager.threadDownload(access, DownloadKey(got.ID))
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(os.Getenv("HOME"), "Downloads")
	if saving == downloadsAsArtifacts {
		wantDir = authorityOf(t, manager, access).downloadDir
	}
	if filepath.Dir(item.path) != wantDir {
		t.Fatalf("the finished file is %s, want it in %s", item.path, wantDir)
	}
	events := log.take()
	if n := countKind(events, "download"); n != 1 {
		t.Fatalf("the download was announced %d times, want once", n)
	}
	at := slices.IndexFunc(events, func(event CompanionEvent) bool { return event.Kind == "download" })
	announced := events[at]
	if announced.Download == nil || *announced.Download != want || ThreadID(announced.ThreadID) != access.ThreadID() {
		t.Fatalf("the announcement is %+v, want %+v in thread %q", announced, want, access.ThreadID())
	}
	if at == 0 || events[at-1].Kind != "state" || len(events[at-1].Downloads) != 1 || events[at-1].Downloads[0] != want {
		t.Fatalf("the announcement is not preceded by the state that lists it: %+v", events[:at])
	}

	// browser_downloads still lists the page's download, now with its size.
	tool, err := manager.Downloads(t.Context(), access, DownloadOptions{PageID: PageID(page.ID)})
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
	if manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "G", SuggestedName: "big.bin"}) != downloadAdmitted {
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
	manager.downloadProgress(downloadProgress{ID: "G", Received: 300_000, Total: 400_000, State: downloadFailed, Error: "net::ERR_FAILED"})
	events := log.take()
	if countKind(events, "state") != 1 || countKind(events, "download") != 1 {
		t.Fatalf("a failure emitted %+v, want one state and one announcement", events)
	}
	manager.downloadsMu.Lock()
	pending := access.tb.downloads.push
	manager.downloadsMu.Unlock()
	if pending != nil {
		t.Fatal("a progress push is still pending after the failure pushed the state")
	}
	if got := threadDownloads(manager, access)[0]; got.State != downloadStateFailed || got.Error != "net::ERR_FAILED" {
		t.Fatalf("an engine failure lists as %+v, want failed with the engine's reason", got)
	}
}

// A download refused for quota, and one stopped by its page closing, are
// listed with the reason and announced.
func TestDownloadsAOStoppedAreListedWithTheirReason(t *testing.T) {
	manager, log, access, page := newDownloadManagerFor(t, downloadsAsArtifacts)
	frame := paneHandle(manager, access, page.ID)
	authorityOf(t, manager, access).pool.Pool().SetUsed(maxWorkspaceDownloadBytes)
	if manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "over", SuggestedName: "over.bin"}) == downloadAdmitted {
		t.Fatal("a download over quota was kept")
	}
	authorityOf(t, manager, access).pool.Pool().SetUsed(0)
	if manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "kept", SuggestedName: "kept.bin"}) != downloadAdmitted {
		t.Fatal("a download within quota was refused")
	}
	log.take()
	if err := manager.ClosePage(t.Context(), access, PageID(page.ID)); err != nil {
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

// A finished download is announced under the generation of the list that
// resolves it, numbered after the state listing it. A clear moves the list
// to another generation, which the next state carries, and an action on
// the cleared download is answered ErrDownloadUnavailable; an
// announcement made after the clear names no generation, so it offers no
// action, and neither does one of a download the list no longer has.
func TestADownloadsAnnouncementResolvesUntilItsListChanges(t *testing.T) {
	manager, log, access, page := newDownloadManager(t)
	var revealed []string
	manager.revealFileInFileManager = func(_ context.Context, path string) error {
		revealed = append(revealed, path)
		return nil
	}
	startDownload(t, manager, access, page, "done.txt", 10)
	var listed, announced CompanionEvent
	for _, event := range log.take() {
		switch event.Kind {
		case "state":
			listed = event
		case "download":
			if announced.Kind != "" {
				t.Fatal("the download was announced twice")
			}
			announced = event
		}
	}
	if announced.Kind == "" || announced.Download == nil || announced.Download.State != downloadStateCompleted {
		t.Fatalf("the finish announced %+v, want the completed download", announced)
	}
	if announced.DownloadsGen == 0 || announced.DownloadsGen != listed.DownloadsGen || announced.Seq <= listed.Seq {
		t.Fatalf("announced under generation %d at %d after a state of generation %d at %d, want the state's generation, numbered after it",
			announced.DownloadsGen, announced.Seq, listed.DownloadsGen, listed.Seq)
	}
	id := DownloadKey(announced.Download.ID)
	if err := manager.RevealThreadDownload(t.Context(), access, id); err != nil {
		t.Fatalf("the announced download does not resolve before a clear: %v", err)
	}
	if len(revealed) != 1 || filepath.Base(revealed[0]) != "done.txt" {
		t.Fatalf("the reveal before the clear showed %q, want done.txt", revealed)
	}

	if err := manager.ClearThreadDownloads(access); err != nil {
		t.Fatal(err)
	}
	cleared := manager.CompanionState(access.Caller())
	if cleared.DownloadsGen == announced.DownloadsGen || cleared.Seq <= announced.Seq {
		t.Fatalf("after the clear the state has generation %d at %d, want another than %d, numbered after %d",
			cleared.DownloadsGen, cleared.Seq, announced.DownloadsGen, announced.Seq)
	}
	for _, act := range []struct {
		name string
		do   func() error
	}{
		{"reveal", func() error { return manager.RevealThreadDownload(t.Context(), access, id) }},
		{"open", func() error { _, err := manager.OpenThreadDownload(t.Context(), access, id); return err }},
		{"cancel", func() error { return manager.CancelThreadDownload(access, id) }},
	} {
		if err := act.do(); !errors.Is(err, ErrDownloadUnavailable) {
			t.Errorf("%s of the cleared download answered %v, want ErrDownloadUnavailable", act.name, err)
		}
	}
	if len(revealed) != 1 {
		t.Fatalf("the file manager was asked to show %q, want only the reveal before the clear", revealed)
	}
	late := manager.downloadAnnouncement(access.tb, downloadNote{tb: access.tb, announce: announced.Download, gen: announced.DownloadsGen})
	if late.DownloadsGen != 0 {
		t.Fatalf("an announcement made after the clear names generation %d, want none", late.DownloadsGen)
	}
	dropped := manager.downloadAnnouncement(access.tb, downloadNote{tb: access.tb, announce: &ThreadDownload{ID: "dropped"}, gen: cleared.DownloadsGen})
	if dropped.DownloadsGen != 0 {
		t.Fatalf("an announcement of a download its list no longer has names generation %d, want none", dropped.DownloadsGen)
	}
}

// A download whose publish finishes after the thread's close detached its
// lifetime, while that lifetime's pages still close, is announced with no
// generation: the thread can no longer resolve it, so the announcement
// offers no action.
func TestADownloadPublishedAfterTheThreadClosedOffersNoAction(t *testing.T) {
	manager, log, access, page := newDownloadManager(t)
	linking, linked := make(chan struct{}), make(chan struct{})
	linkDownload = func(staged, candidate string) error {
		if !placing(candidate) {
			close(linking)
			<-linked
		}
		return linkFile(staged, candidate)
	}
	t.Cleanup(func() { linkDownload = linkFile })
	startDownload(t, manager, access, page, "late.txt", 10, "hold")
	item, _ := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
	// The page's engine close waits, so the detached lifetime's teardown
	// has not finished when the publish settles.
	closing, closed := make(chan struct{}), make(chan struct{})
	manager.engine.(*fakeEngine).beforePageClose = func() {
		close(closing)
		<-closed
	}
	profile := fakeProfileOf(t, manager)
	finished := make(chan error, 1)
	go func() { finished <- profile.finishHeld(item.engineID) }()
	<-linking
	log.take()
	threadClosed := make(chan error, 1)
	go func() { threadClosed <- manager.CloseThread(context.Background(), access.ThreadID()) }()
	<-closing
	close(linked)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	var announced []CompanionEvent
	for _, event := range log.take() {
		if event.Kind == "download" {
			announced = append(announced, event)
		}
	}
	close(closed)
	if err := <-threadClosed; err != nil {
		t.Fatal(err)
	}
	if len(announced) != 1 || announced[0].Download == nil || announced[0].Download.State != downloadStateCompleted {
		t.Fatalf("the publish announced %+v, want the completed download once", announced)
	}
	if announced[0].DownloadsGen != 0 {
		t.Fatalf("a download published after its thread closed is announced under generation %d, want none", announced[0].DownloadsGen)
	}
}

// A download's address is bounded as it starts, before its row or a parked
// popup download keeps it, and so is the source a blank page lists in its
// place.
func TestADownloadsAddressIsBoundedAsItStarts(t *testing.T) {
	manager, _, access, _ := newDownloadManager(t)
	blank, err := manager.NewCompanionPage(t.Context(), access, "")
	if err != nil {
		t.Fatal(err)
	}
	frame := paneHandle(manager, access, blank.ID)
	long := "data:text/plain," + strings.Repeat("a", 1<<20)
	if manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "listed", URL: long, SuggestedName: "a.txt"}) != downloadAdmitted {
		t.Fatal("the download was refused")
	}
	p, _, err := manager.lookupOwnedPage(access, PageID(blank.ID))
	if err != nil {
		t.Fatal(err)
	}
	logged := len(p.downloads.list()[0].URL)
	if logged > maxBrowserURLBytes {
		t.Fatalf("the page's log keeps a %d-byte address", logged)
	}
	if source := len(threadDownloads(manager, access)[0].Source); source > maxBrowserURLBytes {
		t.Fatalf("the thread's list keeps a %d-byte source", source)
	}
	if manager.downloadStarted(downloadStart{Frame: "popup", Popup: "popup", ID: "parked", URL: long, SuggestedName: "b.txt", Openers: []ids.PageHandle{ids.PageHandle(frame)}}) != downloadAdmitted {
		t.Fatal("the popup's download was refused")
	}
	manager.parkMu.Lock()
	parked := len(manager.parked["popup"].downloads[0].start.URL)
	manager.parkMu.Unlock()
	if parked > maxBrowserURLBytes {
		t.Fatalf("a parked download keeps a %d-byte address", parked)
	}
}
