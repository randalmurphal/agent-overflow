package browser

import (
	"errors"
	"os"
	"slices"
	"testing"
	"time"
)

// A download that begins while its page is leaving is refused when the page
// is gone by its admission: the engine is told no, nothing is listed and
// nothing is reserved. The gate runs the departure between the start's page
// lookup and its admission.
func TestADownloadStartRacingItsPageDepartureIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leave func(t *testing.T, manager *Manager, access Access, page PageInfo)
	}{
		{"clear site data", func(t *testing.T, manager *Manager, _ Access, _ PageInfo) {
			if err := manager.ClearSiteData(t.Context()); err != nil {
				t.Error(err)
			}
		}},
		{"close the page", func(t *testing.T, manager *Manager, access Access, page PageInfo) {
			if err := manager.closeLivePage(t.Context(), access.tb, PageID(page.ID)); err != nil {
				t.Error(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, log, access, page := newDownloadManager(t)
			baseline := manager.artifactBytes.Used()
			authority := authorityOf(t, manager, access)
			manager.mu.Lock()
			p, _ := manager.livePageLocked(PageID(page.ID))
			manager.mu.Unlock()
			manager.downloadAdmitting = func() { tc.leave(t, manager, access, page) }

			if manager.downloadStarted(downloadStart{Frame: frameID(p.driver.Handle()), ID: "late-1", URL: "https://example.test/late.bin", SuggestedName: "late.bin"}) == downloadAdmitted {
				t.Fatal("a download on a page that left was admitted")
			}
			if listed := threadDownloads(manager, access); len(listed) != 0 {
				t.Fatalf("the thread lists %+v", listed)
			}
			entries := len(p.downloads.list())
			if entries != 0 {
				t.Fatalf("the page that left holds %d download entries", entries)
			}
			if authority.downloadBytes.Used() != 0 || manager.artifactBytes.Used() != baseline {
				t.Fatalf("the workspace reserves %d bytes and the artifacts %d over %d", authority.downloadBytes.Used(), manager.artifactBytes.Used(), baseline)
			}
			if n := countKind(log.take(), "download"); n != 0 {
				t.Fatalf("the refused download was announced %d times", n)
			}
		})
	}
}

// Deleting the thread forgets its downloads.
func TestClosingTheThreadForgetsItsDownloads(t *testing.T) {
	manager, _, access, page := newDownloadManager(t)
	startDownload(t, manager, access, page, "a.txt", 1)
	if err := manager.CloseThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	manager.downloadsMu.Lock()
	kept := access.tb.downloads != nil
	manager.downloadsMu.Unlock()
	if kept {
		t.Fatal("the closed thread's downloads are still kept")
	}
}

// However a page stops being the Manager's, a download it left running
// settles, its reservations are released, the engine is asked to stop it,
// its partial file goes (the engine's in the Downloads folder, AO's in its
// own directory), and its row can be cleared.
func TestEveryPageTeardownSettlesItsRunningDownloads(t *testing.T) {
	for _, mode := range downloadModes {
		t.Run(mode.name, func(t *testing.T) {
			testEveryPageTeardownSettlesItsRunningDownloads(t, mode.saving)
		})
	}
}

func testEveryPageTeardownSettlesItsRunningDownloads(t *testing.T, saving downloadSaving) {
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
			p, _ := manager.livePageLocked(PageID(page.ID))
			manager.mu.Unlock()
			manager.engine.(*fakeEngine).events.PageClosed(p.driver.Handle())
		}, false},
		{"page crashed", func(t *testing.T, manager *Manager, _ Access, page PageInfo) {
			manager.mu.Lock()
			p, _ := manager.livePageLocked(PageID(page.ID))
			manager.mu.Unlock()
			manager.engine.(*fakeEngine).events.PageCrashed(p.driver.Handle(), "killed")
		}, false},
		{"close the page", func(t *testing.T, manager *Manager, access Access, page PageInfo) {
			if err := manager.closeLivePage(t.Context(), access.tb, PageID(page.ID)); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"thread close", func(t *testing.T, manager *Manager, access Access, _ PageInfo) {
			if err := manager.CloseThread(t.Context(), access.ThreadID()); err != nil {
				t.Fatal(err)
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, _, access, page := newDownloadManagerFor(t, saving)
			baseline := manager.artifactBytes.Used()
			startDownload(t, manager, access, page, "held.bin", 4096, "hold")
			authority := authorityOf(t, manager, access)
			item, err := manager.threadDownload(access, DownloadKey(threadDownloads(manager, access)[0].ID))
			if err != nil {
				t.Fatal(err)
			}
			profile := fakeProfileOf(t, manager)
			partial := heldFile(t, manager, item.engineID)

			tc.teardown(t, manager, access, page)

			deadline := time.Now().Add(3 * time.Second)
			for {
				listed := threadDownloads(manager, access)
				running := slices.ContainsFunc(listed, func(d ThreadDownload) bool { return d.State == downloadStateRunning })
				settled := (tc.listGone && len(listed) == 0) || (!tc.listGone && len(listed) == 1 && listed[0].State == downloadStateCanceled)
				profile.mu.Lock()
				_, held := profile.held[item.engineID]
				profile.mu.Unlock()
				_, statErr := os.Stat(partial)
				partialGone := errors.Is(statErr, os.ErrNotExist)
				if !running && settled && !held && partialGone && authority.downloadBytes.Used() == 0 && manager.artifactBytes.Used() == baseline {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("after the teardown the thread lists %+v, the engine holds it: %v, its partial file is gone: %v, the workspace reserves %d bytes and the artifacts %d over %d", listed, held, partialGone, authority.downloadBytes.Used(), manager.artifactBytes.Used(), baseline)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !tc.listGone {
				if err := manager.ClearThreadDownloads(access); err != nil {
					t.Fatal(err)
				}
				if listed := threadDownloads(manager, access); len(listed) != 0 {
					t.Fatalf("Clear list keeps %+v", listed)
				}
			}
		})
	}
}
