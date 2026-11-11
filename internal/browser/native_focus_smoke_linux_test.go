//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/keybindings"
)

// TestNativeEngineRealClaimedChordFocusesTheApp drives chords through the
// real WebKitGTK view's key controller while the page view holds keyboard
// focus: a chord AO claims moves the window's focus widget to the SPA's
// webview before the chord is routed, every time, shown or hidden; a chord
// AO does not claim leaves it in the page. GTK 4 cannot make a real input
// event, so what this proves is GTK's focus state, the widget GTK delivers
// the next key to; the page document's focus is checked as well when the
// window is active, which a headless compositor may not make it.
func TestNativeEngineRealClaimedChordFocusesTheApp(t *testing.T) {
	window := nativeSmokeWindow.Load()
	if window == nil {
		t.Skipf("set %s=1 to drive this platform's browser engine in a real window", nativeBrowserSmokeEnv)
	}
	bound := keybindings.Accelerator{Key: "f", Ctrl: true}
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{
		NativeWindow: window.NativeWindow,
		Accelerators: func() keybindings.AcceleratorSet { return keybindings.AcceleratorSet{bound: {}} },
	})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	chords := make(chan keybindings.Accelerator, 16)
	manager.SetEventSink(func(event CompanionEvent) {
		if event.Kind == "accelerator" && event.Accelerator != nil {
			chords <- *event.Accelerator
		}
	})
	workspace := resolvedTempDir(t)
	access := Access{ThreadID: "native-smoke-focus", Workspace: workspace}
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	page, err := manager.OpenFile(ctx, access, writeSmokeFile(t, workspace, "focus.html",
		`<!doctype html><title>focus</title><input id="field" autofocus>`), OpenOptions{})
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
	holder := func(state string) (webkitFocusHolder, bool) {
		t.Helper()
		who, active, err := view.focusHolder()
		if err != nil {
			t.Fatalf("%s: read the focus widget: %v", state, err)
		}
		return who, active
	}
	pageHasFocus := func(state string) bool {
		t.Helper()
		value, _, err := manager.Evaluate(ctx, access, page.ID, `document.hasFocus()`, nil)
		if err != nil {
			t.Fatalf("%s: read the page's focus: %v", state, err)
		}
		focused, _ := value.(bool)
		return focused
	}
	// focusPage puts keyboard focus in the page, as a click into it does.
	focusPage := func(state string) {
		t.Helper()
		if err := view.grabFocus(); err != nil {
			t.Fatalf("%s: focus the page: %v", state, err)
		}
		if who, _ := holder(state); who != focusHeldByPage {
			t.Fatalf("%s: the window's focus widget is %d after focusing the page, want the page", state, who)
		}
	}
	press := func(state string, key rune, claimed bool) {
		t.Helper()
		stopped, err := view.offerCtrlKey(key)
		if err != nil {
			t.Fatalf("%s: offer Ctrl+%c: %v", state, key, err)
		}
		if stopped != claimed {
			t.Fatalf("%s: Ctrl+%c stopped %v, want %v", state, key, stopped, claimed)
		}
	}

	focusPage("an unbound chord")
	_, active := holder("an unbound chord")
	t.Logf("the smoke window is active: %v", active)
	if active && !pageHasFocus("an unbound chord") {
		t.Fatal("an active window's focused page does not have document focus")
	}
	press("an unbound chord", 'g', false)
	if who, _ := holder("an unbound chord"); who != focusHeldByPage {
		t.Fatalf("an unbound chord moved the focus widget to %d", who)
	}

	for _, state := range []string{"a claimed chord", "a claimed chord again"} {
		focusPage(state)
		press(state, 'f', true)
		// The handoff is inside the press: GTK's focus is the app's before
		// the chord reaches the frontend.
		if who, _ := holder(state); who != focusHeldByApp {
			t.Fatalf("%s: the window's focus widget is %d, want the app's webview", state, who)
		}
		if chord := <-chords; chord != bound {
			t.Fatalf("%s: routed %+v, want Ctrl+F", state, chord)
		}
		if active && pageHasFocus(state) {
			t.Fatalf("%s: the page still has document focus", state)
		}
	}

	// A page that keeps keyboard focus while hidden hands it over too.
	focusPage("the page hidden")
	hide := false
	if _, err := manager.Visibility(ctx, access, &hide, ""); err != nil {
		t.Fatalf("hide the page: %v", err)
	}
	hidden, _ := holder("the page hidden")
	t.Logf("the window's focus widget after the hide: %d", hidden)
	press("the page hidden", 'f', true)
	if who, _ := holder("the page hidden"); who != focusHeldByApp {
		t.Fatalf("the page hidden: the window's focus widget is %d, want the app's webview", who)
	}
	<-chords
}
