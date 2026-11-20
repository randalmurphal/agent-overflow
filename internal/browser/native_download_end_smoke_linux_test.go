//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// receivedEnd is a real WebKitGTK download whose finish the engine received
// on the GTK thread and has not reported: its reports to the Manager wait
// at a gate (release) while the test acts in that window.
type receivedEnd struct {
	ctx     context.Context
	manager *Manager
	access  Access
	page    PageInfo
	profile *webkitProfile
	id      downloadID
	size    int64
	release func()
}

func newReceivedEnd(t *testing.T, thread ThreadID) *receivedEnd {
	t.Helper()
	window := smokeWindow(t)
	const size = 200000
	rest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/late.bin" {
			_, _ = fmt.Fprint(w, "<!doctype html><title>files</title><p>files</p>")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="late.bin"`)
		w.Header().Set("Content-Length", fmt.Sprint(size))
		_, _ = w.Write([]byte(strings.Repeat("a", size/2)))
		w.(http.Flusher).Flush()
		select {
		case <-rest:
			_, _ = w.Write([]byte(strings.Repeat("a", size-size/2)))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	access := manager.Bind(Caller{ThreadID: thread, Workspace: resolvedTempDir(t)})
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID()); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	page, err := manager.Open(ctx, access, server.URL+"/", OpenOptions{})
	if err != nil {
		t.Fatalf("open the page: %v", err)
	}
	// The sibling keeps the jar's profile open past the tab's close.
	if _, err := manager.Open(ctx, access, server.URL+"/", OpenOptions{}); err != nil {
		t.Fatalf("open the sibling: %v", err)
	}
	p, _, err := manager.lookupOwnedPage(access, PageID(page.ID))
	if err != nil {
		t.Fatalf("look the page up: %v", err)
	}
	view, ok := p.driver.(*webkitPage)
	if !ok {
		t.Fatalf("the page's driver is %T, want the WebKitGTK page", p.driver)
	}
	r := &receivedEnd{ctx: ctx, manager: manager, access: access, page: page, profile: view.profile, size: size}
	if _, _, err := manager.Evaluate(ctx, access, PageID(page.ID), fmt.Sprintf(`location.href = %q; true`, server.URL+"/late.bin"), nil); err != nil {
		t.Fatalf("start the download: %v", err)
	}
	r.waitFor(t, "the download listed", func() bool {
		listed := manager.CompanionState(access.Caller()).Downloads
		return len(listed) == 1 && listed[0].State == downloadStateRunning
	})
	r.profile.mu.Lock()
	for handle := range r.profile.downloads {
		r.id = handle
	}
	r.profile.mu.Unlock()
	held, gate := make(chan struct{}), make(chan struct{})
	r.profile.engine.downloadEvents.Go(func() {
		close(held)
		<-gate
	})
	<-held
	released := false
	r.release = func() {
		if !released {
			released = true
			close(gate)
			r.profile.engine.downloadEvents.Wait()
		}
	}
	t.Cleanup(r.release)
	close(rest)
	r.waitFor(t, "the finish received", r.profile.ends.endReported(r.id))
	return r
}

func (r *receivedEnd) waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	for !done() {
		select {
		case <-r.ctx.Done():
			t.Fatalf("never saw %s", what)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// wantPublished checks, once the held reports ran, that the download lists
// completed with its file in the Downloads folder and its mark cleared.
func (r *receivedEnd) wantPublished(t *testing.T) {
	t.Helper()
	r.release()
	listed := r.manager.CompanionState(r.access.Caller()).Downloads
	if len(listed) != 1 || listed[0].State != downloadStateCompleted || listed[0].ReceivedBytes != r.size {
		t.Fatalf("the download lists as %+v, want completed", listed)
	}
	finished, err := r.manager.finishedDownloadPath(r.access, DownloadKey(listed[0].ID))
	if err != nil {
		t.Fatalf("the finished file: %v", err)
	}
	if stat, err := os.Stat(finished.path); err != nil || stat.Size() != r.size {
		t.Fatalf("the finished file at %s: %v", finished.path, err)
	}
	if r.profile.ends.pending(r.id) {
		t.Fatal("the end stays marked after its report")
	}
}

// TestNativeEngineRealFinishedDownloadOutlivesItsTab closes the tab of a
// real WebKitGTK download whose finish the engine received and has not
// reported: the row is left to that report, which publishes the staged
// file into the Downloads folder after the tab is gone.
func TestNativeEngineRealFinishedDownloadOutlivesItsTab(t *testing.T) {
	r := newReceivedEnd(t, "native-smoke-download-end-close")
	if err := r.manager.ClosePage(r.ctx, r.access, PageID(r.page.ID)); err != nil {
		t.Fatalf("close the tab: %v", err)
	}
	r.wantPublished(t)
}

// TestNativeEngineRealCancelAfterTheFinishIsReceived cancels a real
// WebKitGTK download whose finish the engine received and has not
// reported: WebKit is not asked to cancel what it finished, and the finish
// is published.
func TestNativeEngineRealCancelAfterTheFinishIsReceived(t *testing.T) {
	r := newReceivedEnd(t, "native-smoke-download-end-cancel")
	listed := r.manager.CompanionState(r.access.Caller()).Downloads
	if err := r.manager.CancelThreadDownload(r.access, DownloadKey(listed[0].ID)); err != nil {
		t.Fatalf("cancel the download: %v", err)
	}
	r.wantPublished(t)
}
