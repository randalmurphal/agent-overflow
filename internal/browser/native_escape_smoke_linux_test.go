//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/keybindings"
)

// TestNativeEngineRealEscapeStaysThePages drives Escape through the real
// WebKitGTK view's key controller, as GTK delivers a key to it: a bare
// Escape is never stopped, with a search running or none, and is announced
// once per press while a pane shows the page and not for a key held down;
// a bound Ctrl+Escape goes through the accelerator gate. GTK 4 cannot make
// a real input event, so the press reaches AO's gate and not the page's DOM:
// that the gate lets it through is what this run shows.
func TestNativeEngineRealEscapeStaysThePages(t *testing.T) {
	window := nativeSmokeWindow.Load()
	if window == nil {
		t.Skipf("set %s=1 to drive this platform's browser engine in a real window", nativeBrowserSmokeEnv)
	}
	ctrl := keybindings.Accelerator{Key: "escape", Ctrl: true}
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{
		NativeWindow: window.NativeWindow,
		Accelerators: func() keybindings.AcceleratorSet { return keybindings.AcceleratorSet{ctrl: {}} },
	})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	escapes := make(chan CompanionEvent, 16)
	chords := make(chan keybindings.Accelerator, 16)
	manager.SetEventSink(func(event CompanionEvent) {
		switch {
		case event.Kind == "escape":
			escapes <- event
		case event.Kind == "accelerator" && event.Accelerator != nil:
			chords <- *event.Accelerator
		}
	})
	workspace := resolvedTempDir(t)
	access := Access{ThreadID: "native-smoke-escape", Workspace: workspace}
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	page, err := manager.OpenFile(ctx, access, writeSmokeFile(t, workspace, "escape.html",
		`<!doctype html><title>escape</title><p>alpha one</p><p>alpha two</p>`), OpenOptions{})
	if err != nil {
		t.Fatalf("open the page: %v", err)
	}
	show := true
	if _, err := manager.Visibility(ctx, access, &show, page.ID); err != nil {
		t.Fatalf("show the page: %v", err)
	}
	mount, err := manager.AttachPane(access)
	if err != nil {
		t.Fatalf("mount a pane: %v", err)
	}
	t.Cleanup(func() { manager.DetachPane(mount.ID) })
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatalf("look the page up: %v", err)
	}
	view, ok := p.driver.(*webkitPage)
	if !ok {
		t.Fatalf("the page's driver is %T, want the WebKitGTK page", p.driver)
	}
	offer := func(state string, ctrl, release bool) bool {
		t.Helper()
		handled, err := view.offerEscape(ctrl, release)
		if err != nil {
			t.Fatalf("%s: offer Escape: %v", state, err)
		}
		return handled
	}
	announced := func(state string, want bool) {
		t.Helper()
		select {
		case event := <-escapes:
			if !want {
				t.Fatalf("%s: an Escape was announced: %+v", state, event)
			}
			if event.ThreadID != access.ThreadID || event.PageID != page.ID {
				t.Fatalf("%s: escape event = %+v", state, event)
			}
		case <-time.After(time.Second):
			if want {
				t.Fatalf("%s: the Escape was never announced", state)
			}
		}
	}

	if offer("no search", false, true) {
		t.Fatal("no search: AO stopped the page's Escape")
	}
	announced("no search", true)

	started, err := manager.Find(ctx, access, page.ID, FindStart, "alpha", mount.ID, "smoke-find")
	if err != nil || !started.Active {
		t.Fatalf("start a search = %+v, %v", started, err)
	}
	if offer("a search running", false, true) {
		t.Fatal("a search running: AO stopped the page's Escape")
	}
	announced("a search running", true)
	// The Escape changed nothing of the search: the pane decides.
	if state, err := manager.Find(ctx, access, page.ID, FindNext, "", mount.ID, "smoke-find"); err != nil || !state.Active || state.Session != started.Session {
		t.Fatalf("step after the Escape = %+v, %v", state, err)
	}

	// Held down: the first press is announced, the repeats are not, and the
	// release makes the next press new.
	for i := range 3 {
		if offer("held", false, false) {
			t.Fatalf("held press %d: AO stopped the page's Escape", i)
		}
		announced("held", i == 0)
	}
	if offer("released", false, true) {
		t.Fatal("released: AO stopped the page's Escape")
	}
	announced("the release of a held Escape", false)
	if offer("pressed again", false, true) {
		t.Fatal("pressed again: AO stopped the page's Escape")
	}
	announced("pressed again", true)

	// A bound Ctrl+Escape is a chord: stopped and routed, never announced as
	// the page's Escape.
	if !offer("Ctrl+Escape", true, true) {
		t.Fatal("a bound Ctrl+Escape was not stopped")
	}
	select {
	case chord := <-chords:
		if chord != ctrl {
			t.Fatalf("routed chord %+v, want Ctrl+Escape", chord)
		}
	case <-time.After(time.Second):
		t.Fatal("a bound Ctrl+Escape was never routed")
	}
	announced("Ctrl+Escape", false)

	// The page no longer shown: the Escape is still the page's and nothing
	// is announced.
	hide := false
	if _, err := manager.Visibility(ctx, access, &hide, ""); err != nil {
		t.Fatalf("hide the companion: %v", err)
	}
	if offer("not shown", false, true) {
		t.Fatal("not shown: AO stopped the page's Escape")
	}
	announced("not shown", false)
}
