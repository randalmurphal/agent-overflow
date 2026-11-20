package browser

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
)

// announcedDownloads answers the downloads announced since the log's last
// take, by name.
func announcedDownloads(log *downloadEventLog) map[string]ThreadDownload {
	out := map[string]ThreadDownload{}
	for _, event := range log.take() {
		if event.Kind == "download" && event.Download != nil {
			out[event.Download.Name] = *event.Download
		}
	}
	return out
}

// Many simultaneous transfers: the first to complete while all the others
// run is listed and announced, and its notification's Show in folder
// finds it by the announced id at once and after every other settles,
// though the shown list has dropped it by then. Clearing the pane's
// history leaves the page's browser_downloads log as it was, and closing
// the tab leaves the thread's results.
func TestANotificationResolvesItsDownloadAmongManyTransfers(t *testing.T) {
	for _, total := range []int{maxThreadDownloads + 1, 110} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			manager, log, access, page := newDownloadManager(t)
			var revealed []string
			manager.revealFileInFileManager = func(_ context.Context, path string) error {
				revealed = append(revealed, path)
				return nil
			}
			for i := range total {
				startDownload(t, manager, access, page, fmt.Sprintf("f%d.txt", i), 10, "hold")
			}
			profile := fakeProfileOf(t, manager)
			ids := func() []downloadID {
				profile.mu.Lock()
				defer profile.mu.Unlock()
				var out []downloadID
				for id := range profile.held {
					out = append(out, id)
				}
				slices.Sort(out)
				return out
			}
			held := ids()
			if len(held) != total {
				t.Fatalf("%d transfers run, want %d", len(held), total)
			}
			log.take()
			if err := profile.finishHeld(held[0]); err != nil {
				t.Fatal(err)
			}
			announced := announcedDownloads(log)
			if len(announced) != 1 {
				t.Fatalf("announced %v, want the first completion", announced)
			}
			var first ThreadDownload
			for _, download := range announced {
				first = download
			}
			if first.State != downloadStateCompleted {
				t.Fatalf("the first completion announced %+v", first)
			}
			if err := manager.RevealThreadDownload(t.Context(), access, DownloadKey(first.ID)); err != nil {
				t.Fatalf("the first completion's reveal while the others run: %v", err)
			}
			for _, id := range held[1:] {
				if err := profile.finishHeld(id); err != nil {
					t.Fatal(err)
				}
			}
			if got := len(announcedDownloads(log)); got != total-1 {
				t.Fatalf("announced %d more completions, want %d", got, total-1)
			}
			shown := threadDownloads(manager, access)
			if len(shown) != maxThreadDownloads || slices.ContainsFunc(shown, func(d ThreadDownload) bool { return d.ID == first.ID }) {
				t.Fatalf("the pane shows %d downloads, want the newest %d settled without the first", len(shown), maxThreadDownloads)
			}
			if err := manager.RevealThreadDownload(t.Context(), access, DownloadKey(first.ID)); err != nil {
				t.Fatalf("the first completion's reveal once every other settled: %v", err)
			}
			if len(revealed) != 2 || revealed[0] != revealed[1] {
				t.Fatalf("revealed %q, want the first completion's file twice", revealed)
			}

			if err := manager.ClearThreadDownloads(access); err != nil {
				t.Fatal(err)
			}
			if got := threadDownloads(manager, access); len(got) != 0 {
				t.Fatalf("the pane lists %d downloads after its history was cleared", len(got))
			}
			if err := manager.RevealThreadDownload(t.Context(), access, DownloadKey(first.ID)); err == nil {
				t.Fatal("a cleared download still resolves")
			}
			tool, err := manager.Downloads(t.Context(), access, DownloadOptions{PageID: PageID(page.ID)})
			if err != nil {
				t.Fatal(err)
			}
			if logged := tool.([]DownloadInfo); len(logged) != min(total, maxPageDownloads) {
				t.Fatalf("browser_downloads lists %d after the pane's clear, want %d", len(logged), min(total, maxPageDownloads))
			}

			startDownload(t, manager, access, page, "last.txt", 10)
			if err := manager.closeLivePage(t.Context(), access.tb, PageID(page.ID)); err != nil {
				t.Fatal(err)
			}
			kept := threadDownloads(manager, access)
			if len(kept) != 1 || kept[0].Name != "last.txt" || kept[0].State != downloadStateCompleted {
				t.Fatalf("after its tab closed the thread lists %+v, want the finished last.txt", kept)
			}
			if err := manager.RevealThreadDownload(t.Context(), access, DownloadKey(kept[0].ID)); err != nil {
				t.Fatalf("a download of a closed tab does not reveal: %v", err)
			}
		})
	}
}

// A download with no address of its own, started on a page with none
// either, still gets its row in the thread's list.
func TestADownloadWithNoAddressOnABlankPageIsListed(t *testing.T) {
	manager, _, access, _ := newDownloadManager(t)
	blank, err := manager.NewCompanionPage(t.Context(), access, "")
	if err != nil {
		t.Fatal(err)
	}
	frame := paneHandle(manager, access, blank.ID)
	if manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "no-address", SuggestedName: "data.bin"}) != downloadAdmitted {
		t.Fatal("the download was refused")
	}
	listed := threadDownloads(manager, access)
	if len(listed) != 1 || listed[0].Name != "data.bin" || listed[0].State != downloadStateRunning || listed[0].Source != "" {
		t.Fatalf("the thread lists %+v, want the running data.bin with no source", listed)
	}
	manager.downloadProgress(downloadProgress{ID: "no-address", State: downloadFailed, Error: "stopped"})
	if listed := threadDownloads(manager, access); len(listed) != 1 || listed[0].State != downloadStateFailed {
		t.Fatalf("after its end the thread lists %+v, want it failed", listed)
	}
}

// Clearing the list drops settled downloads, keeps a running one, and
// deletes no file.
func TestClearingDownloadsKeepsRunningOnesAndFiles(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "done.txt", 10)
	done := threadDownloads(manager, access)[0]
	item, err := manager.threadDownload(access, DownloadKey(done.ID))
	if err != nil {
		t.Fatal(err)
	}
	startDownload(t, manager, access, page, "running.bin", 10, "hold")
	if err := manager.ClearThreadDownloads(access); err != nil {
		t.Fatal(err)
	}
	listed := threadDownloads(manager, access)
	if len(listed) != 1 || listed[0].Name != "running.bin" {
		t.Fatalf("after clearing the thread lists %+v, want only the running download", listed)
	}
	if _, err := os.Stat(item.path); err != nil {
		t.Fatalf("clearing the list deleted the file: %v", err)
	}
	// The cleared download takes no later change back into the list.
	if err := manager.ClearThreadDownloads(access); err != nil {
		t.Fatal(err)
	}
	if len(threadDownloads(manager, access)) != 1 {
		t.Fatal("a second clear dropped the running download")
	}
}

// The thread's list keeps maxThreadDownloads settled downloads, dropping the
// one that settled first, and every running one.
func TestTheThreadListIsBoundedAndKeepsRunningDownloads(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	frame := paneHandle(manager, access, page.ID)
	manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "first", SuggestedName: "first.bin"})
	for i := 0; i < maxThreadDownloads+5; i++ {
		startDownload(t, manager, access, page, fmt.Sprintf("f%d.txt", i), 1)
	}
	listed := threadDownloads(manager, access)
	if len(listed) != maxThreadDownloads+1 {
		t.Fatalf("the thread lists %d downloads, want %d settled and the running one", len(listed), maxThreadDownloads)
	}
	if listed[0].Name != fmt.Sprintf("f%d.txt", maxThreadDownloads+4) || listed[1].Name != fmt.Sprintf("f%d.txt", maxThreadDownloads+3) {
		t.Fatalf("the newest downloads are %q, %q, want them first", listed[0].Name, listed[1].Name)
	}
	if last := listed[len(listed)-1]; last.Name != "first.bin" || last.State != downloadStateRunning {
		t.Fatalf("the oldest listed is %+v, want the running first download kept", last)
	}
}

// A download that settles while maxThreadDownloads others run stays listed
// for its announcement, and once the others settle it is the one dropped,
// the first to settle, though it started last.
func TestADownloadThatSettlesAmongRunningOnesStaysListed(t *testing.T) {
	manager, log, access, page := newDownloadManager(t)
	frame := paneHandle(manager, access, page.ID)
	start := func(id downloadID) {
		if manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: id, SuggestedName: string(id) + ".bin"}) != downloadAdmitted {
			t.Fatalf("%s was refused", id)
		}
	}
	for i := 1; i <= maxThreadDownloads; i++ {
		start(downloadID(fmt.Sprintf("dl-%d", i)))
	}
	start("newest")
	log.take()
	manager.downloadProgress(downloadProgress{ID: "newest", State: downloadFailed, Error: "the server closed the connection"})
	if len(threadDownloads(manager, access)) != maxThreadDownloads+1 {
		t.Fatalf("the list holds %d downloads, want every one", len(threadDownloads(manager, access)))
	}
	var announced *ThreadDownload
	for _, event := range log.take() {
		if event.Kind == "download" {
			announced = event.Download
		}
	}
	if announced == nil {
		t.Fatal("the settled download was not announced")
	}
	if _, err := manager.threadDownload(access, DownloadKey(announced.ID)); err != nil {
		t.Fatalf("the announced download is not in the list: %v", err)
	}

	for i := 1; i <= maxThreadDownloads; i++ {
		manager.downloadProgress(downloadProgress{ID: downloadID(fmt.Sprintf("dl-%d", i)), State: downloadFailed, Error: "stopped"})
	}
	listed := threadDownloads(manager, access)
	if len(listed) != maxThreadDownloads {
		t.Fatalf("the list holds %d downloads, want %d", len(listed), maxThreadDownloads)
	}
	for _, item := range listed {
		if item.Name == "newest.bin" {
			t.Fatal("the download that settled first is still listed")
		}
	}
}

// A page's log keeps maxPageDownloads settled entries once a burst
// settles, the ones that settled last, whatever order they started in.
func TestAPageLogIsBoundedOnceItsDownloadsSettle(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	frame := paneHandle(manager, access, page.ID)
	total := maxPageDownloads + 10
	for i := range total {
		id := downloadID(fmt.Sprintf("dl-%d", i))
		if manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: id, SuggestedName: string(id) + ".bin"}) != downloadAdmitted {
			t.Fatalf("%s was refused", id)
		}
	}
	// The first to start settles last.
	for i := total - 1; i >= 0; i-- {
		manager.downloadProgress(downloadProgress{ID: downloadID(fmt.Sprintf("dl-%d", i)), State: downloadFailed, Error: "stopped"})
	}
	p, _, err := manager.lookupOwnedPage(access, PageID(page.ID))
	if err != nil {
		t.Fatal(err)
	}
	if logged := p.downloads.list(); len(logged) != maxPageDownloads {
		t.Fatalf("the page logs %d downloads, want %d", len(logged), maxPageDownloads)
	}
	if !p.downloads.has("dl-0") || p.downloads.has(downloadID(fmt.Sprintf("dl-%d", total-1))) {
		t.Fatal("the log dropped a download that settled last, or kept one that settled first")
	}
}

// A page's own log keeps maxPageDownloads settled entries and every running
// one, whose progress must still find it.
func TestThePageLogKeepsARunningDownloadPastItsBound(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	frame := paneHandle(manager, access, page.ID)
	manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: "running", SuggestedName: "r.bin"})
	for i := 0; i < maxPageDownloads; i++ {
		id := downloadID(fmt.Sprintf("done-%d", i))
		manager.downloadStarted(downloadStart{Frame: frameID(frame), ID: id, SuggestedName: string(id)})
		manager.downloadProgress(downloadProgress{ID: id, State: downloadCanceled})
	}
	manager.downloadProgress(downloadProgress{ID: "running", Received: 5, State: downloadInProgress})
	tool, err := manager.Downloads(t.Context(), access, DownloadOptions{PageID: PageID(page.ID)})
	if err != nil {
		t.Fatal(err)
	}
	entries := tool.([]DownloadInfo)
	if len(entries) != maxPageDownloads+1 || entries[0].ID != "running" || entries[0].Bytes != 5 {
		t.Fatalf("the page log holds %d entries starting %+v, want %d settled and the running one first and current", len(entries), entries[0], maxPageDownloads)
	}
}
