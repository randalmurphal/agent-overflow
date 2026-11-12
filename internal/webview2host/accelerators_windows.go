//go:build windows

package webview2host

import (
	"encoding/json"
	"fmt"

	"agent-overflow/internal/keybindings"
)

// The chord gate: app chords pressed while a PAGE controller has keyboard
// focus. WebView2 raises AcceleratorKeyPressed for every key event with a
// modifier before the document sees it and wants Handled answered inside the
// callback, so the match runs here, in the launcher, against the set the
// backend ships in OpAccelerators. A match is swallowed and reported as
// ReportAccelerator; the backend routes it to the owning thread and the SPA
// dispatches it. An unmodified key, Escape included, is never taken
// (claimChord). A claimed chord hands keyboard focus to the launcher window,
// and so to the SPA, before the report, as the Manager does for the
// in-process engines (browser keyChord): this claim is answered here, so
// the handoff is too, inside the key event and before the next key.

func (h *Host) setAccelerators(list []keybindings.Accelerator) {
	set := make(keybindings.AcceleratorSet, len(list))
	for _, acc := range list {
		set[acc] = struct{}{}
	}
	h.mu.Lock()
	h.accelerators = set
	h.mu.Unlock()
}

func (h *Host) acceleratorPressed(page *hostPage, args *iAcceleratorKeyPressedEventArgs) {
	if kind := args.keyEventKind(); kind != keyEventKindKeyDown && kind != keyEventKindSystemKeyDown {
		return
	}
	pressed := keybindings.Accelerator{
		Ctrl:  keyDown(vkControl),
		Meta:  keyDown(vkLWin) || keyDown(vkRWin),
		Alt:   keyDown(vkMenu),
		Shift: keyDown(vkShift),
	}
	pressed.Key = domKeyForVirtualKey(args.virtualKey())
	if pressed.Key == "" {
		return
	}
	bound, ok := h.claimChord(pressed)
	if !ok {
		return
	}
	if err := args.putHandled(true); err != nil {
		h.config.Logf("browser host: page %s accelerator handled: %v", page.id, err)
	}
	if !focusWindow(h.config.HostWindow()) {
		h.config.Logf("browser host: page %s accelerator: keyboard focus stays in the page (the launcher window is not in the foreground)", page.id)
	}
	detail, err := json.Marshal(bound)
	if err != nil {
		return
	}
	h.config.Report(page.id, ReportAccelerator, string(detail))
}

// releaseFocus ends a page's hold on keyboard focus when it stops being
// visible: held says the page's window had the focus before its hide or
// close. Nothing in the hide says where Windows puts that focus, so it is
// given to the launcher window, and so to the SPA, by the same handoff as a
// claimed chord's. Runs on the UI thread.
func (h *Host) releaseFocus(page *hostPage, held bool) {
	if !held {
		return
	}
	if !focusWindow(h.config.HostWindow()) {
		h.config.Logf("browser host: page %s hidden: keyboard focus stays where it was (the launcher window is not in the foreground)", page.id)
	}
}

// claimChord answers the bound chord a key event is, if AO takes it. An
// unmodified key is never taken: Escape (always an accelerator: WebView2.idl,
// add_AcceleratorKeyPressed) belongs to the page and to the engine's own find
// bar, whose close the engine does not report. Runs on the UI thread.
func (h *Host) claimChord(pressed keybindings.Accelerator) (keybindings.Accelerator, bool) {
	if !pressed.Ctrl && !pressed.Meta && !pressed.Alt {
		return pressed, false
	}
	h.mu.Lock()
	set := h.accelerators
	h.mu.Unlock()
	return set.Match(pressed)
}

// domKeyForVirtualKey spells a Win32 virtual key the way KeyboardEvent.key
// does, unshifted; AcceleratorSet.Match tries the shifted glyph itself.
// Empty for keys no chord is bound to.
func domKeyForVirtualKey(vk uint32) string {
	switch {
	case vk >= 0x30 && vk <= 0x39: // 0-9
		return string(rune(vk))
	case vk >= 0x41 && vk <= 0x5a: // A-Z
		return string(rune(vk + 0x20))
	case vk >= 0x70 && vk <= 0x7b: // F1-F12
		return fmt.Sprintf("f%d", vk-0x70+1)
	}
	if key, ok := namedVirtualKeys[vk]; ok {
		return key
	}
	return ""
}

var namedVirtualKeys = map[uint32]string{
	0x08: "backspace", 0x09: "tab", 0x0d: "enter", 0x1b: "escape", 0x20: " ",
	0x21: "pageup", 0x22: "pagedown", 0x23: "end", 0x24: "home",
	0x25: "arrowleft", 0x26: "arrowup", 0x27: "arrowright", 0x28: "arrowdown",
	0x2d: "insert", 0x2e: "delete",
	// VK_OEM_*: the US-layout unshifted glyphs.
	0xba: ";", 0xbb: "=", 0xbc: ",", 0xbd: "-", 0xbe: ".", 0xbf: "/", 0xc0: "`",
	0xdb: "[", 0xdc: "\\", 0xdd: "]", 0xde: "'",
}
