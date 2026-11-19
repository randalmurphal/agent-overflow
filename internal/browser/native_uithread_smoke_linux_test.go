//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestNativeEngineRealWithdrawnOperationNeverRuns holds the real GTK thread
// past uiThread's timeout while a page is told to navigate: the navigation
// answers that the thread was unavailable, and once the thread is free it
// never runs, so the failure its caller reported stays true.
func TestNativeEngineRealWithdrawnOperationNeverRuns(t *testing.T) {
	window := smokeWindow(t)
	var late atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/late" {
			late.Add(1)
		}
		_, _ = fmt.Fprint(w, "<!doctype html><title>page</title><p>page</p>")
	}))
	t.Cleanup(server.Close)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	access := Access{ThreadID: "native-smoke-withdrawn", Workspace: resolvedTempDir(t)}
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

	entered, gate := make(chan struct{}), make(chan struct{})
	go uiThread.Run(func() {
		close(entered)
		<-gate
	})
	<-entered
	err = webkitLoadURI(view.liveView(), server.URL+"/late")
	close(gate)
	if !errors.Is(err, errGTKUnavailable) {
		t.Fatalf("a navigation the GTK thread did not take in time answered %v", err)
	}
	// One turn of the thread after the hold: what was queued before it ran.
	uiThread.Run(func() {})
	time.Sleep(time.Second)
	if n := late.Load(); n != 0 {
		t.Fatalf("the navigation answered as failed ran afterwards: /late was requested %d times", n)
	}
}
