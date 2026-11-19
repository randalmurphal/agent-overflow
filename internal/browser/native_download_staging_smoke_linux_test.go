//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestNativeEngineRealDownloadStagingLeavesTheGTKThreadFree holds a real
// WebKitGTK download's staging, the file system work that names its file:
// the GTK thread answers meanwhile, and the download is written to the
// staged file once staging ends.
func TestNativeEngineRealDownloadStagingLeavesTheGTKThreadFree(t *testing.T) {
	window := smokeWindow(t)
	payload := bytes.Repeat([]byte("a"), 100_000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/file.bin" {
			_, _ = fmt.Fprint(w, "<!doctype html><title>files</title><p>files</p>")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="file.bin"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	access := Access{ThreadID: "native-smoke-download-staging", Workspace: resolvedTempDir(t)}
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
	entered, unblock := make(chan struct{}), make(chan struct{})
	var enter, release sync.Once
	t.Cleanup(func() { release.Do(func() { close(unblock) }) })
	stage := profile.stage
	profile.stage = func(id string) (string, error) {
		enter.Do(func() { close(entered) })
		<-unblock
		return stage(id)
	}

	if _, _, err := manager.Evaluate(ctx, access, page.ID, fmt.Sprintf(`location.href = %q; true`, server.URL+"/file.bin"), nil); err != nil {
		t.Fatalf("start the download: %v", err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("the download was never staged")
	}
	answered := make(chan bool, 1)
	go func() { answered <- gtkDo(func() {}) }()
	select {
	case ran := <-answered:
		if !ran {
			t.Fatal("the GTK thread refused work while the download was staged")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the GTK thread waited on the download's staging")
	}
	release.Do(func() { close(unblock) })
	for {
		listed := manager.CompanionState(access).Downloads
		if len(listed) == 1 && listed[0].State != downloadStateRunning {
			if listed[0].State != downloadStateCompleted || listed[0].ReceivedBytes != int64(len(payload)) {
				t.Fatalf("the download settled as %+v", listed[0])
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the download never finished: %+v", listed)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
