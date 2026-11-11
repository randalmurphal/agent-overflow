//go:build linux && cgo && !gtk3 && !android && !server && !nogui

package browser

import (
	"context"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/keybindings"
)

// TestNativeEngineRealEscapeStaysThePages drives Escape through the real
// WebKitGTK view's key controller, as GTK delivers a key to it: a bare
// Escape is never stopped, with a search running or none; the gate gathers
// whether the press is held and whether the view's input method is
// composing, and a fresh press while a pane shows the page is announced; a
// bound Ctrl+Escape goes through the accelerator gate. GTK 4 cannot make a
// real input event or composition, so the press reaches AO's gate and not
// the page's DOM, and the composing input is only ever read as false here.
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
	// What the gate gathered for each press, in order; it reports
	// synchronously, inside the press.
	var presses []KeyPress
	restore, err := view.onKeyPress(func(pressed keybindings.Accelerator, press KeyPress) {
		if pressed.Key == "escape" {
			presses = append(presses, press)
		}
	})
	if err != nil {
		t.Fatalf("watch the gate: %v", err)
	}
	t.Cleanup(func() {
		if err := restore(); err != nil {
			t.Errorf("restore the gate: %v", err)
		}
	})
	// offer hands the gate the presses inside one GTK callback, so no focus
	// change lands between them, and answers what the gate gathered.
	offer := func(state string, offers ...escapeOffer) []KeyPress {
		t.Helper()
		presses = nil
		stopped, err := view.offerEscapes(offers)
		if err != nil {
			t.Fatalf("%s: offer Escape: %v", state, err)
		}
		for i, offer := range offers {
			if stopped[i] != offer.Ctrl {
				t.Fatalf("%s: press %d (%+v) stopped %v", state, i, offer, stopped[i])
			}
		}
		if len(presses) != len(offers) {
			t.Fatalf("%s: the gate saw %d presses of %d", state, len(presses), len(offers))
		}
		return presses
	}
	// announced waits for the Escape event a fresh press while shown makes;
	// the bound is reached only when none comes. Which presses are announced
	// is pageEscapeAnnounced's, covered by the unit tests.
	announced := func(state string) {
		t.Helper()
		select {
		case event := <-escapes:
			if event.ThreadID != access.ThreadID || event.PageID != page.ID {
				t.Fatalf("%s: escape event = %+v", state, event)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: the Escape was never announced", state)
		}
	}

	// The gate reads a composition from the view's input method context;
	// WebKitGTK gives every view one, and with nothing typed it is empty.
	hasIM, err := view.hasInputMethod()
	if err != nil || !hasIM {
		t.Fatalf("the view's input method context: %v, %v", hasIM, err)
	}
	fresh := KeyPress{}
	if got := offer("no search", escapeOffer{Release: true}); got[0] != fresh {
		t.Fatalf("no search: gathered %+v", got[0])
	}
	announced("no search")

	started, err := manager.Find(ctx, access, page.ID, FindStart, "alpha", mount.ID, 1)
	if err != nil || !started.Active {
		t.Fatalf("start a search = %+v, %v", started, err)
	}
	if got := offer("a search running", escapeOffer{Release: true}); got[0] != fresh {
		t.Fatalf("a search running: gathered %+v", got[0])
	}
	announced("a search running")
	// The Escape changed nothing of the search: the pane decides.
	if state, err := manager.Find(ctx, access, page.ID, FindNext, "", mount.ID, 1); err != nil || !state.Active || state.Session != started.Session {
		t.Fatalf("step after the Escape = %+v, %v", state, err)
	}

	// Held down: the presses after the first are held until a release, and
	// the press after the release is fresh again.
	got := offer("held", escapeOffer{}, escapeOffer{}, escapeOffer{Release: true}, escapeOffer{Release: true})
	want := []KeyPress{fresh, {Held: true}, {Held: true}, fresh}
	if !slices.Equal(got, want) {
		t.Fatalf("held: gathered %+v, want %+v", got, want)
	}

	// A bound Ctrl+Escape is a chord: stopped and routed.
	offer("Ctrl+Escape", escapeOffer{Ctrl: true, Release: true})
	select {
	case chord := <-chords:
		if chord != ctrl {
			t.Fatalf("routed chord %+v, want Ctrl+Escape", chord)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a bound Ctrl+Escape was never routed")
	}
}
