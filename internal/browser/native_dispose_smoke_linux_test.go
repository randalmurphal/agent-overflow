//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"testing"
	"time"
)

// TestNativeEngineRealDisposeDuringPageCreation disposes a real WebKitGTK
// profile while one of its pages is being built: the profile lists a page
// only once its view is in place, so the disposal does not close a page
// with no view, and the creation, finding the profile disposed, closes its
// view and fails rather than answering a page of a disposed profile.
func TestNativeEngineRealDisposeDuringPageCreation(t *testing.T) {
	window := smokeWindow(t)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	workspace := resolvedTempDir(t)
	access := Access{ThreadID: "native-smoke-dispose", Workspace: workspace}
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	// The engine starts with the thread's first page.
	if _, err := manager.OpenFile(ctx, access, writeSmokeFile(t, workspace, "first.html", `<!doctype html><title>first</title>`), OpenOptions{}); err != nil {
		t.Fatalf("open a page: %v", err)
	}
	engine, ok := manager.engine.(*webkitEngine)
	if !ok {
		t.Fatalf("the engine is %T, want WebKitGTK", manager.engine)
	}
	created, err := engine.NewProfile(ctx, profileOptions{Jar: "native-smoke-dispose-jar", Ephemeral: true})
	if err != nil {
		t.Fatalf("new profile: %v", err)
	}
	profile := created.(*webkitProfile)

	// The GTK thread is held while the page is asked for and the disposal
	// begins, so the view's construction and the disposal wait there in
	// that order.
	entered, gate := make(chan struct{}), make(chan struct{})
	go gtkDo(func() {
		close(entered)
		<-gate
	})
	<-entered
	type answer struct {
		driver pageDriver
		err    error
	}
	built := make(chan answer, 1)
	go func() {
		driver, err := profile.NewPage(ctx, pageHooks{})
		built <- answer{driver, err}
	}()
	// Long enough for the creation to reach the GTK thread's queue.
	time.Sleep(200 * time.Millisecond)
	disposed := make(chan error, 1)
	go func() { disposed <- profile.Dispose(ctx) }()
	time.Sleep(200 * time.Millisecond)
	close(gate)
	got := <-built
	if err := <-disposed; err != nil {
		t.Fatalf("dispose: %v", err)
	}
	if got.err == nil {
		got.driver.Close()
		t.Fatal("a page whose profile was disposed while it was built was answered")
	}
	profile.mu.Lock()
	pages := len(profile.pages)
	profile.mu.Unlock()
	if pages != 0 {
		t.Fatalf("the disposed profile lists %d pages", pages)
	}
}
