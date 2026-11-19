package browser

import (
	"context"
	"fmt"
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
			ids := func() []string {
				profile.mu.Lock()
				defer profile.mu.Unlock()
				var out []string
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
			if err := manager.RevealThreadDownload(t.Context(), access, first.ID); err != nil {
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
			if err := manager.RevealThreadDownload(t.Context(), access, first.ID); err != nil {
				t.Fatalf("the first completion's reveal once every other settled: %v", err)
			}
			if len(revealed) != 2 || revealed[0] != revealed[1] {
				t.Fatalf("revealed %q, want the first completion's file twice", revealed)
			}

			manager.ClearThreadDownloads(access)
			if got := threadDownloads(manager, access); len(got) != 0 {
				t.Fatalf("the pane lists %d downloads after its history was cleared", len(got))
			}
			if err := manager.RevealThreadDownload(t.Context(), access, first.ID); err == nil {
				t.Fatal("a cleared download still resolves")
			}
			tool, err := manager.Downloads(t.Context(), access, DownloadOptions{PageID: page.ID})
			if err != nil {
				t.Fatal(err)
			}
			if logged := tool.([]DownloadInfo); len(logged) != min(total, maxPageDownloads) {
				t.Fatalf("browser_downloads lists %d after the pane's clear, want %d", len(logged), min(total, maxPageDownloads))
			}

			startDownload(t, manager, access, page, "last.txt", 10)
			if err := manager.closeLivePage(t.Context(), access, page.ID); err != nil {
				t.Fatal(err)
			}
			kept := threadDownloads(manager, access)
			if len(kept) != 1 || kept[0].Name != "last.txt" || kept[0].State != downloadStateCompleted {
				t.Fatalf("after its tab closed the thread lists %+v, want the finished last.txt", kept)
			}
			if err := manager.RevealThreadDownload(t.Context(), access, kept[0].ID); err != nil {
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
	if manager.downloadStarted(downloadStart{Frame: frame, ID: "no-address", SuggestedName: "data.bin"}) != downloadAdmitted {
		t.Fatal("the download was refused")
	}
	listed := threadDownloads(manager, access)
	if len(listed) != 1 || listed[0].Name != "data.bin" || listed[0].State != downloadStateRunning || listed[0].Source != "" {
		t.Fatalf("the thread lists %+v, want the running data.bin with no source", listed)
	}
	manager.downloadProgress(downloadProgress{ID: "no-address", State: downloadCanceled, Error: "stopped"})
	if listed := threadDownloads(manager, access); len(listed) != 1 || listed[0].State != downloadStateFailed {
		t.Fatalf("after its end the thread lists %+v, want it failed", listed)
	}
}
