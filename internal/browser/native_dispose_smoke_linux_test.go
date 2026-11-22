//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"errors"
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
	access := manager.Bind(Caller{ThreadID: "native-smoke-dispose", Workspace: workspace})
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID()); err != nil {
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
	go uiThread.Run(func() {
		close(entered)
		<-gate
	})
	<-entered
	type answer struct {
		driver pageDriver
		err    error
	}
	built := make(chan answer, 1)
	reached := make(chan struct{})
	profile.beforeView = func() { close(reached) }
	go func() {
		driver, err := profile.NewPage(ctx, pageHooks{})
		built <- answer{driver, err}
	}()
	// The creation is at its view's construction, which it posts to the
	// held GTK thread next.
	<-reached
	disposed := make(chan error, 1)
	go func() { disposed <- profile.Dispose(ctx) }()
	// The disposal has begun once the profile reads disposed; its GTK work
	// follows.
	for {
		profile.mu.Lock()
		begun := profile.disposed
		profile.mu.Unlock()
		if begun {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the disposal never began")
		case <-time.After(time.Millisecond):
		}
	}
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

// TestNativeEngineRealDisposeBeforePageView disposes a real WebKitGTK
// profile, its session freed, after a page creation passed the profile's
// check and before its view is built: the build, checking the profile on
// the GTK thread, refuses rather than building a view in the freed session.
func TestNativeEngineRealDisposeBeforePageView(t *testing.T) {
	window := smokeWindow(t)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	workspace := resolvedTempDir(t)
	access := manager.Bind(Caller{ThreadID: "native-smoke-dispose-first", Workspace: workspace})
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID()); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if _, err := manager.OpenFile(ctx, access, writeSmokeFile(t, workspace, "first.html", `<!doctype html><title>first</title>`), OpenOptions{}); err != nil {
		t.Fatalf("open a page: %v", err)
	}
	engine, ok := manager.engine.(*webkitEngine)
	if !ok {
		t.Fatalf("the engine is %T, want WebKitGTK", manager.engine)
	}
	created, err := engine.NewProfile(ctx, profileOptions{Jar: "native-smoke-dispose-first-jar", Ephemeral: true})
	if err != nil {
		t.Fatalf("new profile: %v", err)
	}
	profile := created.(*webkitProfile)
	reached, proceed := make(chan struct{}), make(chan struct{})
	profile.beforeView = func() {
		close(reached)
		<-proceed
	}
	built := make(chan error, 1)
	go func() {
		driver, err := profile.NewPage(ctx, pageHooks{})
		if err == nil {
			driver.Close()
		}
		built <- err
	}()
	<-reached
	if err := profile.Dispose(ctx); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	close(proceed)
	if err := <-built; !errors.Is(err, errProfileClosedBeforeView) {
		t.Fatalf("the creation answered %v, want its view refused for the disposed profile", err)
	}
}
