//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNativeEngineRealDownloadCancelMeetsItsRelease cancels a real
// WebKitGTK download while its release waits on the GTK thread: the cancel
// resolves the download there, in order with the release, so it answers
// that the download ended instead of reaching a reference the release
// drops.
func TestNativeEngineRealDownloadCancelMeetsItsRelease(t *testing.T) {
	window := smokeWindow(t)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slow.bin" {
			_, _ = fmt.Fprint(w, "<!doctype html><title>files</title><p>files</p>")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="slow.bin"`)
		w.Header().Set("Content-Length", "10000000")
		_, _ = w.Write([]byte(strings.Repeat("a", 65536)))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	access := manager.Bind(Caller{ThreadID: "native-smoke-download-release", Workspace: resolvedTempDir(t)})
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	page, err := manager.Open(ctx, access, server.URL+"/", OpenOptions{})
	if err != nil {
		t.Fatalf("open the page: %v", err)
	}
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatalf("look the page up: %v", err)
	}
	view, ok := p.driver.(*webkitPage)
	if !ok {
		t.Fatalf("the page's driver is %T, want the WebKitGTK page", p.driver)
	}
	profile := view.profile
	if _, _, err := manager.Evaluate(ctx, access, page.ID, fmt.Sprintf(`location.href = %q; true`, server.URL+"/slow.bin"), nil); err != nil {
		t.Fatalf("start the download: %v", err)
	}
	var id string
	for id == "" {
		profile.mu.Lock()
		for handle := range profile.downloads {
			id = handle
		}
		profile.mu.Unlock()
		select {
		case <-ctx.Done():
			t.Fatal("the download never started")
		case <-time.After(20 * time.Millisecond):
		}
	}

	// The GTK thread is held while the cancel is asked and the release
	// begins, so both wait there, the cancel first.
	entered, gate := make(chan struct{}), make(chan struct{})
	go gtkDo(func() {
		close(entered)
		<-gate
	})
	<-entered
	canceled := make(chan error, 1)
	go func() { canceled <- profile.CancelDownload(id) }()
	// Long enough for the cancel to reach the GTK thread's queue.
	time.Sleep(200 * time.Millisecond)
	released := make(chan struct{})
	go func() {
		profile.releaseDownload(id)
		close(released)
	}()
	for profile.heldDownload(id) != nil {
		time.Sleep(time.Millisecond)
	}
	close(gate)
	if err := <-canceled; !errors.Is(err, errDownloadEnded) {
		t.Fatalf("a cancel that met the download's release answered %v, want that it ended", err)
	}
	<-released
}

// TestNativeEngineRealDownloadFailingBeforeItsFileIsFreed starts a real
// WebKitGTK download whose response never comes: it fails before WebKit
// asks for its destination, so the engine never owned it, and WebKit frees
// it.
func TestNativeEngineRealDownloadFailingBeforeItsFileIsFreed(t *testing.T) {
	window := smokeWindow(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/broken.bin" {
			_, _ = fmt.Fprint(w, `<!doctype html><title>files</title><a id="d" href="/broken.bin" download>file</a>`)
			return
		}
		// The connection ends before any response header.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(server.Close)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	access := manager.Bind(Caller{ThreadID: "native-smoke-download-early-failure", Workspace: resolvedTempDir(t)})
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	page, err := manager.Open(ctx, access, server.URL+"/", OpenOptions{})
	if err != nil {
		t.Fatalf("open the page: %v", err)
	}
	before := webkitDownloadsAlive()
	if _, _, err := manager.Evaluate(ctx, access, page.ID, `document.getElementById("d").click(); true`, nil); err != nil {
		t.Fatalf("start the download: %v", err)
	}
	started := false
	for deadline := time.Now().Add(30 * time.Second); ; {
		alive := webkitDownloadsAlive()
		started = started || alive > before
		if started && alive == before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("downloads alive: %d before, %d now, started %v; want one started and freed", before, alive, started)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if listed := manager.CompanionState(access.Caller()).Downloads; len(listed) != 0 {
		t.Fatalf("a download that failed before its destination is listed: %+v", listed)
	}
}
