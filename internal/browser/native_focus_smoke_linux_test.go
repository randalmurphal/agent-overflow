//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"fmt"
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
	// A chord is routed to the pane showing its page.
	if err := manager.SetPaneRect(mount.ID, PaneRect{Width: 640, Height: 400, ViewportWidth: 1280, ViewportHeight: 800, Visible: true}); err != nil {
		t.Fatalf("place the pane: %v", err)
	}
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

	// The hide itself hands focus over (TestNativeEngineRealHiddenPageGivesUpFocus);
	// a parked page given focus anyway hands it over on a claim too. No pane
	// shows it, so the chord is not routed.
	hide := false
	if _, err := manager.Visibility(ctx, access, &hide, ""); err != nil {
		t.Fatalf("hide the page: %v", err)
	}
	focusPage("the page hidden")
	press("the page hidden", 'f', true)
	if who, _ := holder("the page hidden"); who != focusHeldByApp {
		t.Fatalf("the page hidden: the window's focus widget is %d, want the app's webview", who)
	}
}

// TestNativeEngineRealHiddenPageGivesUpFocus holds the real WebKitGTK views
// to the paneHost rule that a page that is not visible never holds keyboard
// focus: each way a presented page stops showing gives the window's focus
// widget to the SPA's webview, and showing a page again takes nothing. A
// click into the page is grabFocus, as in the chord smoke above.
func TestNativeEngineRealHiddenPageGivesUpFocus(t *testing.T) {
	window := nativeSmokeWindow.Load()
	if window == nil {
		t.Skipf("set %s=1 to drive this platform's browser engine in a real window", nativeBrowserSmokeEnv)
	}
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	left := PaneRect{X: 0, Y: 0, Width: 640, Height: 400, ViewportWidth: 1280, ViewportHeight: 800, Visible: true}
	right := left
	right.X = 640

	type shownPage struct {
		access Access
		id     string
		view   *webkitPage
		mount  string
	}
	view := func(t *testing.T, access Access, id string) *webkitPage {
		t.Helper()
		p, _, err := manager.lookupOwnedPage(access, id)
		if err != nil {
			t.Fatalf("look the page up: %v", err)
		}
		page, ok := p.driver.(*webkitPage)
		if !ok {
			t.Fatalf("the page's driver is %T, want the WebKitGTK page", p.driver)
		}
		return page
	}
	// open opens a page with a focused input in a new thread and presents it
	// in a pane at rect.
	open := func(t *testing.T, thread string, rect PaneRect) shownPage {
		t.Helper()
		workspace := resolvedTempDir(t)
		access := Access{ThreadID: "native-smoke-hidden-focus-" + thread, Workspace: workspace}
		t.Cleanup(func() {
			if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
				t.Errorf("close the thread's pages: %v", err)
			}
		})
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
		if err := manager.SetPaneRect(mount.ID, rect); err != nil {
			t.Fatalf("place the pane: %v", err)
		}
		return shownPage{access: access, id: page.ID, view: view(t, access, page.ID), mount: mount.ID}
	}
	// expect checks who holds the window's focus, read through any live view.
	expect := func(t *testing.T, through *webkitPage, want webkitFocusHolder, state string) {
		t.Helper()
		who, _, err := through.focusHolder()
		if err != nil {
			t.Fatalf("%s: read the focus widget: %v", state, err)
		}
		if who != want {
			t.Fatalf("%s: the window's focus widget is %d, want %d", state, who, want)
		}
	}
	expectPresented := func(t *testing.T, page *webkitPage, want bool, state string) {
		t.Helper()
		on, err := page.presented()
		if err != nil {
			t.Fatalf("%s: read the presentation: %v", state, err)
		}
		if on != want {
			t.Fatalf("%s: the page is presented %v, want %v", state, on, want)
		}
	}
	// click focuses a presented page, as a click into it does.
	click := func(t *testing.T, page *webkitPage) {
		t.Helper()
		expectPresented(t, page, true, "before the click")
		if err := page.grabFocus(); err != nil {
			t.Fatalf("focus the page: %v", err)
		}
		expect(t, page, focusHeldByPage, "after the click")
	}
	setVisible := func(t *testing.T, access Access, on bool, id string) {
		t.Helper()
		if _, err := manager.Visibility(ctx, access, &on, id); err != nil {
			t.Fatalf("set the companion visible %v: %v", on, err)
		}
	}
	// title is the page's title as the engine last reported it, read without
	// asking the page while it navigates.
	title := func(t *testing.T, access Access, id string) string {
		t.Helper()
		p, _, err := manager.lookupOwnedPage(access, id)
		if err != nil {
			t.Fatalf("look the page up: %v", err)
		}
		return p.cachedInfo().Title
	}

	t.Run("hide while an input is focused, then show", func(t *testing.T) {
		page := open(t, "parked", left)
		click(t, page.view)
		setVisible(t, page.access, false, "")
		expectPresented(t, page.view, false, "hidden")
		expect(t, page.view, focusHeldByApp, "hidden")
		setVisible(t, page.access, true, page.id)
		expectPresented(t, page.view, true, "shown again")
		expect(t, page.view, focusHeldByApp, "shown again")
	})

	t.Run("the pane closed", func(t *testing.T) {
		page := open(t, "detached", left)
		click(t, page.view)
		manager.DetachPane(page.mount)
		expect(t, page.view, focusHeldByApp, "the pane closed")
	})

	t.Run("a compact screen change", func(t *testing.T) {
		page := open(t, "compact", left)
		click(t, page.view)
		offscreen := left
		offscreen.Visible = false
		if err := manager.SetPaneRect(page.mount, offscreen); err != nil {
			t.Fatalf("report the pane off screen: %v", err)
		}
		expect(t, page.view, focusHeldByApp, "off screen")
		if err := manager.SetPaneRect(page.mount, left); err != nil {
			t.Fatalf("report the pane on screen: %v", err)
		}
		expectPresented(t, page.view, true, "on screen again")
		expect(t, page.view, focusHeldByApp, "on screen again")
	})

	t.Run("a tab switch", func(t *testing.T) {
		page := open(t, "tabs", left)
		other, err := manager.NewCompanionPage(ctx, page.access)
		if err != nil {
			t.Fatalf("open a second tab: %v", err)
		}
		otherView := view(t, page.access, other.ID)
		if err := manager.ActivateCompanionPage(ctx, page.access, page.id); err != nil {
			t.Fatalf("select the first tab: %v", err)
		}
		click(t, page.view)
		if err := manager.ActivateCompanionPage(ctx, page.access, other.ID); err != nil {
			t.Fatalf("switch tabs: %v", err)
		}
		expectPresented(t, otherView, true, "switched")
		expect(t, page.view, focusHeldByApp, "switched")
	})

	t.Run("the focused page closed", func(t *testing.T) {
		page := open(t, "closed", left)
		other, err := manager.NewCompanionPage(ctx, page.access)
		if err != nil {
			t.Fatalf("open a second tab: %v", err)
		}
		otherView := view(t, page.access, other.ID)
		if err := manager.ActivateCompanionPage(ctx, page.access, page.id); err != nil {
			t.Fatalf("select the first tab: %v", err)
		}
		click(t, page.view)
		if err := manager.ClosePage(ctx, page.access, page.id); err != nil {
			t.Fatalf("close the page: %v", err)
		}
		expect(t, otherView, focusHeldByApp, "closed")
	})

	t.Run("hide during a navigation", func(t *testing.T) {
		page := open(t, "navigating", left)
		next := writeSmokeFile(t, page.access.Workspace, "next.html",
			`<!doctype html><title>next</title><input autofocus>`)
		click(t, page.view)
		if _, _, err := manager.Evaluate(ctx, page.access, page.id,
			fmt.Sprintf(`location.href = %s; null`, jsonString(smokeFileURL(next))), nil); err != nil {
			t.Fatalf("start the navigation: %v", err)
		}
		setVisible(t, page.access, false, "")
		expect(t, page.view, focusHeldByApp, "hidden mid-navigation")
		for title(t, page.access, page.id) != "next" {
			select {
			case <-ctx.Done():
				t.Fatal("the navigation never finished")
			case <-time.After(50 * time.Millisecond):
			}
		}
		expect(t, page.view, focusHeldByApp, "the navigation finished hidden")
	})

	t.Run("hide a popup page", func(t *testing.T) {
		opener := open(t, "popup", left)
		popupFile := writeSmokeFile(t, opener.access.Workspace, "popup.html",
			`<!doctype html><title>popup</title><input autofocus>`)
		if _, _, err := manager.Evaluate(ctx, opener.access, opener.id,
			fmt.Sprintf(`window.open(%s); null`, jsonString(smokeFileURL(popupFile))), nil); err != nil {
			t.Fatalf("open the popup: %v", err)
		}
		var popupID string
		for popupID == "" {
			if ids := smokePopups(ctx, t, manager, opener.access, opener.id); len(ids) > 0 {
				popupID = ids[0]
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("the popup was never adopted")
			case <-time.After(50 * time.Millisecond):
			}
		}
		popup := view(t, opener.access, popupID)
		if err := manager.ActivateCompanionPage(ctx, opener.access, popupID); err != nil {
			t.Fatalf("show the popup: %v", err)
		}
		if who, _, err := popup.focusHolder(); err != nil || who == focusHeldByPage {
			t.Fatalf("the popup took the focus when it was shown (%d, %v)", who, err)
		}
		click(t, popup)
		setVisible(t, opener.access, false, "")
		expectPresented(t, popup, false, "the popup hidden")
		expect(t, popup, focusHeldByApp, "the popup hidden")
	})

	t.Run("two panes with two pages", func(t *testing.T) {
		first := open(t, "first", left)
		second := open(t, "second", right)
		click(t, first.view)
		manager.DetachPane(second.mount)
		expectPresented(t, second.view, false, "the other pane closed")
		expectPresented(t, first.view, true, "the other pane closed")
		expect(t, first.view, focusHeldByPage, "the other pane closed")
		manager.DetachPane(first.mount)
		expect(t, first.view, focusHeldByApp, "its own pane closed")
	})
}
