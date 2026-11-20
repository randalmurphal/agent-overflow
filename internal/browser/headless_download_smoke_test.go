//go:build !windows

package browser

import (
	"bytes"
	"fmt"
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

// TestHeadlessChromiumRealDownloadFilesGoWithTheirBrowser closes a real
// Chromium's profile while a download it is writing waits for its frame's
// owner, with 1 MiB of it written to Chromium's partial file: once Dispose
// returns, the browser process has exited and neither the partial file
// nor the final one is left in the download directory.
// Gated with TestHeadlessChromiumReal.
func TestHeadlessChromiumRealDownloadFilesGoWithTheirBrowser(t *testing.T) {
	if os.Getenv("AO_HEADLESS_CHROMIUM_SMOKE") != "1" {
		t.Skip("set AO_HEADLESS_CHROMIUM_SMOKE=1 to launch this machine's real Chromium")
	}
	t.Setenv("HOME", t.TempDir())
	const written = 1 << 20
	release := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="big.bin"`)
		w.Header().Set("Content-Length", fmt.Sprint(2*written))
		_, _ = w.Write(bytes.Repeat([]byte("b"), written))
		w.(http.Flusher).Flush()
		// The rest never comes while the test runs.
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(stop)
	engine, err := newHeadlessChromiumEngine(t.TempDir(), HeadlessChromiumOptions{Reaper: procutiltest.Reaper()}, engineEvents{
		PopupOpened:     func(enginePopup) {},
		PageClosed:      func(ids.PageHandle) {},
		PageCrashed:     func(ids.PageHandle, string) {},
		PageInfoChanged: func(ids.PageHandle, string, string, uint64) {},
		// No page owns the frame yet: the download waits for its owner.
		DownloadStarted:  func(downloadStart) downloadAdmission { return downloadUnowned },
		DownloadProgress: func(downloadProgress) {},
	})
	if err != nil {
		t.Fatalf("find a Chromium: %v", err)
	}
	engine.ownerWait = time.Hour
	engine.logf = func(format string, args ...any) { t.Logf(format, args...) }
	if err := engine.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer engine.Stop()
	profile := testHeadlessProfile(t, engine, t.TempDir(), false)
	page, err := profile.NewPage(t.Context(), testPageHooks())
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	opCtx, cancel := operationContext(t.Context(), page.Lifetime(), operationTimeout)
	defer cancel()
	// A navigation to an attachment commits no document; its answer is
	// the download's, whatever it says.
	_ = page.Navigate(opCtx, server.URL+"/big.bin")
	partial := ""
	for deadline := time.Now().Add(30 * time.Second); partial == ""; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("Chromium never wrote %d bytes of the download into %s", written, profile.downloadDir)
		}
		entries, _ := os.ReadDir(profile.downloadDir)
		for _, entry := range entries {
			info, err := entry.Info()
			if err == nil && strings.HasSuffix(entry.Name(), chromiumPartialSuffix) && info.Size() >= written {
				partial = filepath.Join(profile.downloadDir, entry.Name())
			}
		}
	}
	t.Logf("Chromium wrote %s", partial)
	if err := profile.Dispose(t.Context()); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	entries, err := os.ReadDir(profile.downloadDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("%s is left in the download directory after its browser exited", entry.Name())
	}
}
