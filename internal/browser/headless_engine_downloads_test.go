//go:build !windows

package browser

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
)

// pinnedDownloadDir is the directory the launch told Chromium to write its
// downloads into.
func pinnedDownloadDir(t *testing.T, browser *fakeChromium) string {
	t.Helper()
	type behavior struct {
		DownloadPath string `json:"downloadPath"`
	}
	calls := browserCallParams[behavior](t, browser.calls(), "Browser.setDownloadBehavior")
	if len(calls) != 1 {
		t.Fatalf("the launch pinned downloads %d times, want once", len(calls))
	}
	return calls[0].DownloadPath
}

// Downloads land ONLY in a directory of the browser process's own inside
// the AO artifact directory, which goes with the process, and allowAndName
// is what the Manager's own bookkeeping reads back. Events are what make
// the download reports arrive at all.
func TestHeadlessProfilePinsDownloadsToTheArtifactDirectory(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", true)
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}

	var params struct {
		Behavior      string `json:"behavior"`
		DownloadPath  string `json:"downloadPath"`
		EventsEnabled bool   `json:"eventsEnabled"`
	}
	var found bool
	for _, call := range browser.calls() {
		if call.Method != "Browser.setDownloadBehavior" {
			continue
		}
		if err := json.Unmarshal(call.Params, &params); err != nil {
			t.Fatalf("decode setDownloadBehavior params: %v", err)
		}
		found = true
	}
	if !found {
		t.Fatalf("the launch never pinned downloads: %v", browser.methods())
	}
	if filepath.Dir(params.DownloadPath) != profile.downloadDir {
		t.Fatalf("downloads land in %q, want a directory in the artifact directory %q", params.DownloadPath, profile.downloadDir)
	}
	if info, err := os.Stat(params.DownloadPath); err != nil || !info.IsDir() {
		t.Fatalf("the browser's download directory is not there: %v", err)
	}
	if params.Behavior != "allowAndName" {
		t.Fatalf("download behavior is %q; the Manager renames from the GUID-named file allowAndName writes", params.Behavior)
	}
	if !params.EventsEnabled {
		t.Fatal("download events are off, so no download would ever be reported or capped")
	}
	if err := profile.Dispose(context.Background()); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	if _, err := os.Stat(params.DownloadPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the browser's download directory outlived its browser: %v", err)
	}
}

// A download the Manager refuses outright, over quota, is cancelled by the
// engine at once, with no wait for a popup to own it, and the file Chromium
// finished for it before the cancel goes at its end; one it keeps is left
// running.
func TestHeadlessProfileCancelsADownloadTheManagerRefuses(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	var mu sync.Mutex
	var started []downloadID
	engine.events.DownloadStarted = func(start downloadStart) downloadAdmission {
		mu.Lock()
		defer mu.Unlock()
		started = append(started, start.ID)
		if start.ID == "G-kept" {
			return downloadAdmitted
		}
		return downloadRefused
	}
	// An unowned start would wait this long for a popup to own it.
	engine.ownerWait = time.Hour
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}
	// What Chromium wrote for a download that finished before its cancel.
	finished := filepath.Join(pinnedDownloadDir(t, browser), "G-refused")
	if err := os.WriteFile(finished, []byte("whole"), 0o600); err != nil {
		t.Fatal(err)
	}

	browser.send(downloadWillBegin("G-refused", "F-1"))
	browser.send(downloadWillBegin("G-kept", "F-1"))
	type cancel struct {
		GUID string `json:"guid"`
	}
	eventually(t, "the refused download to be cancelled", func() bool {
		return len(browserCallParams[cancel](t, browser.calls(), "Browser.cancelDownload")) > 0
	})
	eventually(t, "both downloads to be reported", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(started) == 2
	})
	if got := browserCallParams[cancel](t, browser.calls(), "Browser.cancelDownload"); len(got) != 1 || got[0].GUID != "G-refused" {
		t.Fatalf("cancelled %+v, want only G-refused", got)
	}
	browser.send(downloadProgressEvent("G-refused", "completed"))
	eventually(t, "the refused download's file to go", func() bool {
		_, err := os.Stat(finished)
		return errors.Is(err, fs.ErrNotExist)
	})
}

// Chromium names a cancel and a failure alike ("canceled"): the profile's
// own cancel of a download makes its end the user's cancel, and the same
// end of a download nobody canceled is a failure.
func TestHeadlessProfileTellsItsCancelFromAFailure(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	ends := make(chan downloadProgress, 4)
	// Sent while the tracker holds the start, which it admits
	// before it lets a cancel in.
	started := make(chan downloadID, 4)
	engine.events.DownloadStarted = func(start downloadStart) downloadAdmission {
		started <- start.ID
		return downloadAdmitted
	}
	engine.events.DownloadProgress = func(progress downloadProgress) { ends <- progress }
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}
	stopped := func(guid string) map[string]any {
		return map[string]any{"method": "Browser.downloadProgress", "params": map[string]any{
			"guid": guid, "totalBytes": 10, "receivedBytes": 4, "state": "canceled",
		}}
	}
	for _, tc := range []struct {
		guid   string
		cancel bool
		want   downloadState
	}{{"G-canceled", true, downloadCanceled}, {"G-failed", false, downloadFailed}} {
		browser.send(downloadWillBegin(tc.guid, "F-1"))
		select {
		case <-started:
		case <-time.After(headlessTestDeadline):
			t.Fatalf("%s never started", tc.guid)
		}
		if tc.cancel {
			if err := profile.CancelDownload(downloadID(tc.guid)); err != nil {
				t.Fatal(err)
			}
		}
		browser.send(stopped(tc.guid))
		select {
		case got := <-ends:
			if got.ID != downloadID(tc.guid) || got.State != tc.want {
				t.Fatalf("Chromium's cancel of %s reached the Manager as %+v, want %v", tc.guid, got, tc.want)
			}
		case <-time.After(headlessTestDeadline):
			t.Fatalf("%s's end never reached the Manager", tc.guid)
		}
	}
}

// The file Chromium writes for a download no page took outlives neither the
// download nor its browser: a refused download whose cancel Chromium never
// answers, and whose file appears only as it completes, loses the file at
// its completion; one still waiting for its frame's owner when the profile
// is disposed loses it once the browser has stopped.
func TestHeadlessProfileRemovesTheFilesOfDownloadsNoPageTook(t *testing.T) {
	t.Run("a refused download whose cancel is unanswered", func(t *testing.T) {
		browser := writeFakeChromium(t, "chromium")
		engine := newTestHeadlessEngine(t, browser.path)
		engine.closeTimeout = 50 * time.Millisecond
		engine.events.DownloadStarted = func(downloadStart) downloadAdmission { return downloadRefused }
		engine.events.DownloadProgress = func(downloadProgress) {}
		profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
		if _, err := profile.ensureBrowser(); err != nil {
			t.Fatalf("launch: %v", err)
		}
		room := pinnedDownloadDir(t, browser)
		browser.stallBrowserCalls.Store(true)
		browser.send(downloadWillBegin("G-late", "F-1"))
		eventually(t, "the refused download's cancel to be sent", func() bool {
			return slices.Contains(browser.methods(), "Browser.cancelDownload")
		})
		// The cancel goes unanswered; Chromium finishes the file meanwhile
		// and reports the completion.
		finished := filepath.Join(room, "G-late")
		if err := os.WriteFile(finished, []byte("whole"), 0o600); err != nil {
			t.Fatal(err)
		}
		browser.send(downloadProgressEvent("G-late", "completed"))
		eventually(t, "the refused download's file to go at its completion", func() bool {
			_, err := os.Stat(finished)
			return errors.Is(err, fs.ErrNotExist)
		})
	})
	t.Run("an unowned download when the profile is disposed", func(t *testing.T) {
		browser := writeFakeChromium(t, "chromium")
		engine := newTestHeadlessEngine(t, browser.path)
		offered := make(chan struct{}, 1)
		engine.events.DownloadStarted = func(downloadStart) downloadAdmission {
			select {
			case offered <- struct{}{}:
			default:
			}
			return downloadUnowned
		}
		engine.events.DownloadProgress = func(downloadProgress) {}
		engine.ownerWait = time.Hour
		profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
		if _, err := profile.ensureBrowser(); err != nil {
			t.Fatalf("launch: %v", err)
		}
		finished := filepath.Join(pinnedDownloadDir(t, browser), "G-unowned")
		if err := os.WriteFile(finished, []byte("whole"), 0o600); err != nil {
			t.Fatal(err)
		}
		browser.send(downloadWillBegin("G-unowned", "F-elsewhere"))
		select {
		case <-offered:
		case <-time.After(headlessTestDeadline):
			t.Fatal("the download was never offered")
		}
		browser.send(downloadProgressEvent("G-unowned", "completed"))
		if err := profile.Dispose(context.Background()); err != nil {
			t.Fatalf("dispose: %v", err)
		}
		if _, err := os.Stat(finished); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the unowned download's file is still there after the profile was disposed: %v", err)
		}
	})
}

// A discarded popup's close and a download's cancel end when a connected
// Chromium stops answering them: the discard within its caller's deadline,
// the cancel within the close timeout. Meanwhile another page opens, the
// target the discard could not close stays bound to its profile, and a
// later discard, answered, closes it.
func TestHeadlessProfileBoundsItsBrowserCalls(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	engine.closeTimeout = 50 * time.Millisecond
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}
	engine.bindPage("P-1", profile)
	browser.stallBrowserCalls.Store(true)
	discarded := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		discarded <- engine.DiscardPage(ctx, "P-1")
	}()
	opened, err := profile.NewPage(t.Context(), testPageHooks())
	if err != nil {
		t.Fatalf("another page while the discard is unanswered: %v", err)
	}
	opened.Close()
	select {
	case err := <-discarded:
		if err == nil {
			t.Fatal("a discard Chromium never answered succeeded")
		}
	case <-time.After(headlessTestDeadline):
		t.Fatal("a discard Chromium never answers did not end")
	}
	if owner, bound := engine.profileForPage("P-1"); !bound || owner != profile {
		t.Fatal("the target the discard could not close has no owner")
	}
	canceled := make(chan error, 1)
	go func() { canceled <- profile.CancelDownload("G-1") }()
	select {
	case err := <-canceled:
		if err == nil {
			t.Fatal("a cancel Chromium never answered succeeded")
		}
	case <-time.After(headlessTestDeadline):
		t.Fatal("a cancel Chromium never answers did not end")
	}
	browser.stallBrowserCalls.Store(false)
	if err := engine.DiscardPage(t.Context(), "P-1"); err != nil {
		t.Fatalf("a discard again, answered: %v", err)
	}
	if _, bound := engine.profileForPage("P-1"); bound {
		t.Fatal("the discarded page is still bound")
	}
}

// A download in a popup's main frame names the popup, its openers as the
// browser reported them, and its profile, so the Manager lists it whatever
// becomes of the popup; one in any other frame names no popup.
func TestHeadlessProfileNamesThePopupADownloadBeganIn(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	starts := make(chan downloadStart, 4)
	engine.events.DownloadStarted = func(start downloadStart) downloadAdmission {
		starts <- start
		return downloadAdmitted
	}
	engine.events.PopupOpened = func(enginePopup) {}
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}
	browser.send(popupCreated("P-1", "T-1"))
	browser.send(popupCreated("P-2", "P-1"))
	eventually(t, "the popups to be recorded", func() bool {
		_, first := engine.profileForPage("P-1")
		_, second := engine.profileForPage("P-2")
		return first && second
	})
	browser.send(downloadWillBegin("G-popup", "P-2"))
	browser.send(downloadWillBegin("G-page", "F-1"))
	for range 2 {
		var start downloadStart
		select {
		case start = <-starts:
		case <-time.After(headlessTestDeadline):
			t.Fatal("a download was never reported")
		}
		switch start.ID {
		case "G-popup":
			if start.Frame != "P-2" || start.Popup != "P-2" || !slices.Equal(start.Openers, []ids.PageHandle{"P-1", "T-1"}) || start.Profile != profile.Handle() {
				t.Errorf("the popup's download started as %+v", start)
			}
		case "G-page":
			if start.Popup != "" || start.Openers != nil || start.Profile != "" {
				t.Errorf("the page's download started as %+v", start)
			}
		}
	}
}

// A download whose start Chromium sends after its popup's destroy was
// handled still names the popup, its openers and its profile: the event
// subscriptions run in no fixed order, and the Manager lists it by the
// popup's fate.
func TestHeadlessProfileNamesAGonePopupADownloadBeganIn(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	starts := make(chan downloadStart, 1)
	engine.events.DownloadStarted = func(start downloadStart) downloadAdmission {
		starts <- start
		return downloadAdmitted
	}
	engine.events.PopupOpened = func(enginePopup) {}
	engine.events.PageClosed = func(ids.PageHandle) {}
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}
	browser.send(popupCreated("P-1", "T-1"))
	browser.send(popupCreated("P-2", "P-1"))
	eventually(t, "the popups to be recorded", func() bool {
		_, first := engine.profileForPage("P-1")
		_, second := engine.profileForPage("P-2")
		return first && second
	})
	browser.send(map[string]any{"method": "Target.targetDestroyed", "params": map[string]any{"targetId": "P-1"}})
	browser.send(map[string]any{"method": "Target.targetDestroyed", "params": map[string]any{"targetId": "P-2"}})
	eventually(t, "the popups' destroys to be handled", func() bool {
		_, first := engine.profileForPage("P-1")
		_, second := engine.profileForPage("P-2")
		return !first && !second
	})
	browser.send(downloadWillBegin("G-gone", "P-2"))
	select {
	case start := <-starts:
		if start.Popup != "P-2" || !slices.Equal(start.Openers, []ids.PageHandle{"P-1", "T-1"}) || start.Profile != profile.Handle() {
			t.Fatalf("the gone popup's download started as %+v", start)
		}
	case <-time.After(headlessTestDeadline):
		t.Fatal("the download was never reported")
	}
}
