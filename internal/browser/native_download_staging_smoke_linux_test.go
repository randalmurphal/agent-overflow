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

	"agent-overflow/internal/browser/uicall"
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
	access := manager.Bind(Caller{ThreadID: "native-smoke-download-staging", Workspace: resolvedTempDir(t)})
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID()); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	page, err := manager.Open(ctx, access, server.URL+"/", OpenOptions{})
	if err != nil {
		t.Fatalf("open the page: %v", err)
	}
	p, _, err := manager.lookupOwnedPage(access, PageID(page.ID))
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
	profile.stage = func(id downloadID) (string, error) {
		enter.Do(func() { close(entered) })
		<-unblock
		return stage(id)
	}

	if _, _, err := manager.Evaluate(ctx, access, PageID(page.ID), fmt.Sprintf(`location.href = %q; true`, server.URL+"/file.bin"), nil); err != nil {
		t.Fatalf("start the download: %v", err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("the download was never staged")
	}
	answered := make(chan bool, 1)
	go func() { answered <- uiThread.Run(func() {}) == uicall.Ran }()
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
		listed := manager.CompanionState(access.Caller()).Downloads
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

// TestNativeEngineRealProfileCloseEndsADownloadBeingStaged closes a real
// WebKitGTK profile while a download's staging is blocked: the close
// cancels the open destination decision and releases the download on the
// GTK thread at once, and the staging that ends afterwards gives up the
// file it named, exactly once, with no row listed.
func TestNativeEngineRealProfileCloseEndsADownloadBeingStaged(t *testing.T) {
	window := smokeWindow(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/file.bin" {
			_, _ = fmt.Fprint(w, "<!doctype html><title>files</title><p>files</p>")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="file.bin"`)
		_, _ = w.Write(bytes.Repeat([]byte("a"), 1000))
	}))
	t.Cleanup(server.Close)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	access := manager.Bind(Caller{ThreadID: "native-smoke-download-staging-close", Workspace: resolvedTempDir(t)})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	page, err := manager.Open(ctx, access, server.URL+"/", OpenOptions{})
	if err != nil {
		t.Fatalf("open the page: %v", err)
	}
	p, _, err := manager.lookupOwnedPage(access, PageID(page.ID))
	if err != nil {
		t.Fatalf("look the page up: %v", err)
	}
	profile := p.driver.(*webkitPage).profile
	entered, unblock := make(chan downloadID, 1), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(unblock) }) })
	stage, releaseStaged := profile.stage, profile.releaseStaged
	profile.stage = func(id downloadID) (string, error) {
		entered <- id
		<-unblock
		return stage(id)
	}
	var mu sync.Mutex
	var released []downloadID
	profile.releaseStaged = func(id downloadID) {
		mu.Lock()
		released = append(released, id)
		mu.Unlock()
		releaseStaged(id)
	}
	before := webkitDownloadsAlive()
	if _, _, err := manager.Evaluate(ctx, access, PageID(page.ID), fmt.Sprintf(`location.href = %q; true`, server.URL+"/file.bin"), nil); err != nil {
		t.Fatalf("start the download: %v", err)
	}
	var id downloadID
	select {
	case id = <-entered:
	case <-ctx.Done():
		t.Fatal("the download was never staged")
	}
	if err := manager.CloseThread(ctx, access.ThreadID()); err != nil {
		t.Fatalf("close the thread's pages: %v", err)
	}
	for deadline := time.Now().Add(30 * time.Second); webkitDownloadsAlive() != before; {
		if time.Now().After(deadline) {
			t.Fatalf("downloads alive: %d before, %d with its profile closed and its staging blocked", before, webkitDownloadsAlive())
		}
		time.Sleep(20 * time.Millisecond)
	}
	release.Do(func() { close(unblock) })
	for deadline := time.Now().Add(30 * time.Second); ; {
		mu.Lock()
		got := append([]downloadID(nil), released...)
		mu.Unlock()
		if len(got) > 0 {
			if len(got) != 1 || got[0] != id {
				t.Fatalf("released the staged files of %v, want %s once", got, id)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the staged file the late staging named was never released")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if path := manager.stagedPath(id); path != "" {
		t.Fatalf("the download still holds its staged file %s", path)
	}
	if listed := manager.CompanionState(access.Caller()).Downloads; len(listed) != 0 {
		t.Fatalf("a download canceled before its destination is listed: %+v", listed)
	}
}
