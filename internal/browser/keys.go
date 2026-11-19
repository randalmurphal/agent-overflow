package browser

import (
	"log"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/keybindings"
)

// pageEscape is the key the page always keeps: a bare Escape, which AO only
// observes (keyChord).
var pageEscape = keybindings.Accelerator{Key: "escape"}

// pageEscapeAnnounced decides whether a key pressed in a page is announced
// as the page's Escape: a bare Escape, newly pressed (not repeating, not
// held), that no input method composition takes to cancel itself.
func pageEscapeAnnounced(pressed keybindings.Accelerator, press KeyPress) bool {
	return pressed == pageEscape && !press.Repeat && !press.Held && !press.Composing
}

// keyChord is engineEvents.KeyChord: it answers on the engine's UI thread, so
// it is one set lookup, and the routing that takes m.mu happens off it.
//
// A bare Escape is never AO's: the page has it exactly as if AO were not
// there. When pageEscapeAnnounced says so it is announced (emitEscape), so
// a find bar the pane shows over the page can close.
//
// A chord AO claims takes keyboard focus out of the page: the command it
// runs puts focus where it wants it (a find bar's input, the palette), and
// the keys typed next must reach that, not the page. So every claim hands
// focus to the app (engineAppFocus) before it returns; a chord AO does not
// claim leaves focus in the page.
func (m *Manager) keyChord(handle ids.PageHandle, pressed keybindings.Accelerator, press KeyPress) bool {
	if pressed == pageEscape {
		if pageEscapeAnnounced(pressed, press) {
			go m.emitEscape(handle)
		}
		return false
	}
	if m.accelerators == nil {
		return false
	}
	bound, ok := m.accelerators().Match(pressed)
	if !ok {
		return false
	}
	if engine, ok := m.engine.(engineAppFocus); ok {
		if err := engine.FocusApp(); err != nil {
			// The chord still runs; its surface opens with focus left in
			// the page, as before the handoff existed.
			log.Printf("browser: hand keyboard focus to the app for %s: %v", handle, err)
		}
	}
	go m.emitAccelerator(handle, bound)
	return true
}

// emitEscape announces a bare Escape pressed in a page a pane shows, as a
// Kind "escape" event naming the thread, the page and the presenting mount,
// whose client's find bar it closes.
func (m *Manager) emitEscape(handle ids.PageHandle) {
	m.mu.Lock()
	p := m.pageByHandleLocked(handle)
	mount := ""
	if p != nil {
		mount, _, _ = m.presenterLocked(p.owner)
	}
	m.mu.Unlock()
	if p == nil || mount == "" || !m.pageShown(p.owner.id, p.id) {
		return
	}
	m.emit(CompanionEvent{Kind: "escape", ThreadID: p.owner.id, PageID: string(p.id), Mount: mount})
}

// emitAccelerator routes a claimed chord to the pane mount showing the page,
// so that only the client holding the mount replays it. A page no pane shows
// has nobody to act for, and its chord is dropped.
func (m *Manager) emitAccelerator(handle ids.PageHandle, bound keybindings.Accelerator) {
	m.mu.Lock()
	p := m.pageByHandleLocked(handle)
	mount := ""
	if p != nil && m.presentedPageLocked(p.owner) == p.id {
		mount, _, _ = m.presenterLocked(p.owner)
	}
	m.mu.Unlock()
	if p == nil {
		return
	}
	if mount == "" {
		log.Printf("browser: drop the chord %+v pressed in page %s: no pane shows the page", bound, p.id)
		return
	}
	m.emit(CompanionEvent{Kind: "accelerator", ThreadID: p.owner.id, Mount: mount, Accelerator: &bound})
}

// AcceleratorsChanged tells an engine that matches chords out of process
// (engineAccelerators) that the bound set it holds is stale.
func (m *Manager) AcceleratorsChanged() {
	if engine, ok := m.engine.(engineAccelerators); ok {
		engine.SyncAccelerators()
	}
}
