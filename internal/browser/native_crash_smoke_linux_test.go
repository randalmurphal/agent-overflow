//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestNativeEngineRealCrashKeepsTheTab ends a real WebKitGTK page's web
// process: the view's web-process-terminated reaches the Manager, the tab
// stays, failed with the reason, at its address, and its Reload brings the
// page back there in a new view.
func TestNativeEngineRealCrashKeepsTheTab(t *testing.T) {
	window := smokeWindow(t)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	workspace := resolvedTempDir(t)
	access := manager.Bind(Caller{ThreadID: "native-smoke-crash", Workspace: workspace})
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID()); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	page, err := manager.OpenFile(ctx, access, writeSmokeFile(t, workspace, "crash.html",
		`<!doctype html><title>crash</title><p>before</p>`), OpenOptions{})
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
	if err := view.terminateWebProcess(); err != nil {
		t.Fatalf("end the web process: %v", err)
	}
	var tab PageInfo
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		tab, _ = findTab(manager, access, page.ID)
		if tab.Error != "" || time.Now().After(deadline) {
			break
		}
	}
	if !tab.Suspended || !strings.Contains(tab.Error, "terminated") || tab.URL != page.URL {
		t.Fatalf("tab after the web process ended = %+v, want it failed at %s", tab, page.URL)
	}
	if err := manager.ReloadCompanionPage(ctx, access, PageID(page.ID)); err != nil {
		t.Fatalf("reload the crashed tab: %v", err)
	}
	tab, _ = findTab(manager, access, page.ID)
	if tab.Suspended || tab.Error != "" || tab.URL != page.URL {
		t.Fatalf("tab after its reload = %+v, want it live at %s", tab, page.URL)
	}
	value, _, err := manager.Evaluate(ctx, access, PageID(page.ID), "document.title", nil)
	if err != nil || value != "crash" {
		t.Fatalf("the reloaded page answers %v, %v; want its document", value, err)
	}
}
