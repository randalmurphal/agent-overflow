//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestNativeEngineRealAdoptedPopupShowsItsLoadedPage has a popup finish
// loading before the Manager adopts it, so its view's address and title
// change before AO observes them: the adopted tab still shows its title,
// from the reports alone, with no read of the page.
func TestNativeEngineRealAdoptedPopupShowsItsLoadedPage(t *testing.T) {
	window := smokeWindow(t)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	holdPopupAdoption(manager, time.Second)
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	workspace := resolvedTempDir(t)
	access := Access{ThreadID: "native-smoke-adopted-popup", Workspace: workspace}
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	opener, err := manager.OpenFile(ctx, access, writeSmokeFile(t, workspace, "opener.html",
		`<!doctype html><title>opener</title>`), OpenOptions{})
	if err != nil {
		t.Fatalf("open the opener: %v", err)
	}
	popupFile := writeSmokeFile(t, workspace, "popup.html", `<!doctype html><title>popup</title>`)
	if _, _, err := manager.Evaluate(ctx, access, opener.ID,
		fmt.Sprintf(`window.open(%s); null`, jsonString(smokeFileURL(popupFile))), nil); err != nil {
		t.Fatalf("open the popup: %v", err)
	}
	for {
		for _, tab := range manager.CompanionState(access).Pages {
			if tab.ID != opener.ID && tab.Title == "popup" && tab.URL == smokeFileURL(popupFile) {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the adopted popup's tab never showed its page: %+v", manager.CompanionState(access).Pages)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestNativeEngineRealPopupOpensAChildWhileHeld has a popup's parser script
// open a child at once, while the Manager still holds the popup from its
// adoption: the child becomes a tab of the same thread, after its opener.
func TestNativeEngineRealPopupOpensAChildWhileHeld(t *testing.T) {
	window := smokeWindow(t)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	holdPopupAdoption(manager, time.Second)
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	workspace := resolvedTempDir(t)
	access := Access{ThreadID: "native-smoke-popup-child", Workspace: workspace}
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	opener, err := manager.OpenFile(ctx, access, writeSmokeFile(t, workspace, "opener.html",
		`<!doctype html><title>opener</title>`), OpenOptions{})
	if err != nil {
		t.Fatalf("open the opener: %v", err)
	}
	child := writeSmokeFile(t, workspace, "child.html", `<!doctype html><title>child</title>`)
	parent := writeSmokeFile(t, workspace, "parent.html", fmt.Sprintf(
		`<!doctype html><title>parent</title><script>window.open(%s)</script>`, jsonString(smokeFileURL(child))))
	if _, _, err := manager.Evaluate(ctx, access, opener.ID,
		fmt.Sprintf(`window.open(%s); null`, jsonString(smokeFileURL(parent))), nil); err != nil {
		t.Fatalf("open the parent popup: %v", err)
	}
	for {
		titles := map[string]int{}
		for _, tab := range manager.CompanionState(access).Pages {
			titles[tab.Title]++
		}
		if titles["parent"] > 0 && titles["child"] > 0 {
			// One window, one tab.
			time.Sleep(500 * time.Millisecond)
			titles = map[string]int{}
			for _, tab := range manager.CompanionState(access).Pages {
				titles[tab.Title]++
			}
			if titles["parent"] != 1 || titles["child"] != 1 {
				t.Fatalf("tabs by title %v, want one parent and one child", titles)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the parent's and the child's tabs never both opened: %+v", manager.CompanionState(access).Pages)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
