//go:build !windows

package browser

import (
	"bytes"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/procutil/procutiltest"
)

// stalledDownloadServer serves attachments that write written bytes of
// twice that and then stall until the test ends.
func stalledDownloadServer(t *testing.T, written int) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="big.bin"`)
		w.Header().Set("Content-Length", fmt.Sprint(2*written))
		_, _ = w.Write(bytes.Repeat([]byte("b"), written))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	return server
}

// realDownloadEngine is a started headless engine over this machine's
// Chromium, with downloads answered by started and reported to progress.
func realDownloadEngine(t *testing.T, started func(downloadStart) downloadAdmission, progress func(downloadProgress)) *headlessEngine {
	t.Helper()
	if os.Getenv("AO_HEADLESS_CHROMIUM_SMOKE") != "1" {
		t.Skip("set AO_HEADLESS_CHROMIUM_SMOKE=1 to launch this machine's real Chromium")
	}
	t.Setenv("HOME", t.TempDir())
	engine, err := newHeadlessChromiumEngine(t.TempDir(), HeadlessChromiumOptions{Reaper: procutiltest.Reaper()}, engineEvents{
		PopupOpened:      func(enginePopup) {},
		PageClosed:       func(ids.PageHandle) {},
		PageCrashed:      func(ids.PageHandle, string) {},
		PageInfoChanged:  func(ids.PageHandle, string, string, uint64) {},
		DownloadStarted:  started,
		DownloadProgress: progress,
	})
	if err != nil {
		t.Fatalf("find a Chromium: %v", err)
	}
	engine.ownerWait = time.Hour
	engine.logf = func(format string, args ...any) { t.Logf(format, args...) }
	if err := engine.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(engine.Stop)
	return engine
}

// navigateToDownload opens a page of profile at url, an attachment.
func navigateToDownload(t *testing.T, profile *headlessProfile, url string) {
	t.Helper()
	page, err := profile.NewPage(t.Context(), testPageHooks())
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	opCtx, cancel := operationContext(t.Context(), page.Lifetime(), operationTimeout)
	defer cancel()
	// A navigation to an attachment commits no document; its answer is
	// the download's, whatever it says.
	_ = page.Navigate(opCtx, url)
}

// filesUnder answers every file under dir, its subdirectories'
// included, by path and size.
func filesUnder(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	files := map[string]int64{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if info, err := entry.Info(); err == nil {
			files[path] = info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// awaitPartials waits until Chromium has written at least written bytes of
// n partial files under dir.
func awaitPartials(t *testing.T, dir string, n, written int) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		count := 0
		for path, size := range filesUnder(t, dir) {
			if strings.HasSuffix(path, chromiumPartialSuffix) && size >= int64(written) {
				count++
			}
		}
		if count >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Chromium wrote %d bytes of %d downloads, want %d, into %s", written, count, n, dir)
		}
	}
}

// wantEmptyDownloadDir fails for anything left under dir.
func wantEmptyDownloadDir(t *testing.T, dir string) {
	t.Helper()
	for path := range filesUnder(t, dir) {
		t.Errorf("%s is left in the download directory after its browser exited", path)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("%s is left in the download directory after its browser exited", entry.Name())
	}
}

// TestHeadlessChromiumRealDownloadFilesGoWithTheirBrowser closes a real
// Chromium's profile while a download it is writing waits for its frame's
// owner, with 1 MiB of it written to Chromium's partial file: once Dispose
// returns, the browser process has exited and nothing it wrote is left in
// the download directory. Gated with TestHeadlessChromiumReal.
func TestHeadlessChromiumRealDownloadFilesGoWithTheirBrowser(t *testing.T) {
	const written = 1 << 20
	engine := realDownloadEngine(t, func(downloadStart) downloadAdmission { return downloadUnowned }, func(downloadProgress) {})
	server := stalledDownloadServer(t, written)
	profile := testHeadlessProfile(t, engine, t.TempDir(), false)
	navigateToDownload(t, profile, server.URL+"/big.bin")
	awaitPartials(t, profile.downloadDir, 1, written)
	if err := profile.Dispose(t.Context()); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	wantEmptyDownloadDir(t, profile.downloadDir)
}

// TestHeadlessChromiumRealQueuedDownloadStartGoesWithItsBrowser holds the
// Manager's answer to one download's start, so a second tab's download
// start waits behind it on the connection's ordered input, unprocessed,
// while Chromium writes 1 MiB of each: once Dispose returns, neither file
// is left. Gated with TestHeadlessChromiumReal.
func TestHeadlessChromiumRealQueuedDownloadStartGoesWithItsBrowser(t *testing.T) {
	const written = 1 << 20
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	engine := realDownloadEngine(t, func(downloadStart) downloadAdmission {
		once.Do(func() { close(held) })
		<-release
		return downloadUnowned
	}, func(downloadProgress) {})
	defer close(release)
	server := stalledDownloadServer(t, written)
	profile := testHeadlessProfile(t, engine, t.TempDir(), false)
	navigateToDownload(t, profile, server.URL+"/first.bin")
	select {
	case <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("the first download's start never reached the Manager")
	}
	navigateToDownload(t, profile, server.URL+"/second.bin")
	awaitPartials(t, profile.downloadDir, 2, written)
	if err := profile.Dispose(t.Context()); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	wantEmptyDownloadDir(t, profile.downloadDir)
}

// TestHeadlessChromiumRealCancelDuringAdmissionIsListedCanceled cancels a
// real Chromium's download while the Manager is still answering its start,
// as a user's cancel of the row the Manager has just published can: the
// cancel succeeds and the download's end reaches the Manager as canceled,
// not failed. Gated with TestHeadlessChromiumReal.
func TestHeadlessChromiumRealCancelDuringAdmissionIsListedCanceled(t *testing.T) {
	const written = 1 << 20
	var profile *headlessProfile
	ready := make(chan struct{})
	canceled := make(chan error, 1)
	ended := make(chan downloadProgress, 1)
	engine := realDownloadEngine(t, func(start downloadStart) downloadAdmission {
		<-ready
		// From a goroutine of its own, as the user's cancel comes.
		done := make(chan error, 1)
		go func() { done <- profile.CancelDownload(start.ID) }()
		canceled <- <-done
		return downloadAdmitted
	}, func(progress downloadProgress) {
		if !progress.State.running() {
			ended <- progress
		}
	})
	server := stalledDownloadServer(t, written)
	profile = testHeadlessProfile(t, engine, t.TempDir(), false)
	close(ready)
	navigateToDownload(t, profile, server.URL+"/big.bin")
	select {
	case err := <-canceled:
		if err != nil {
			t.Fatalf("cancel during admission: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the download's start never reached the Manager")
	}
	select {
	case end := <-ended:
		if end.State != downloadCanceled {
			t.Fatalf("the canceled download ended %s (%s), want canceled", end.State, end.Error)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the canceled download never ended")
	}
	if err := profile.Dispose(t.Context()); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	wantEmptyDownloadDir(t, profile.downloadDir)
}
