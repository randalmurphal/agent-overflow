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
// dispatches it. Plain typing never enters the gate.

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
	if !pressed.Ctrl && !pressed.Meta && !pressed.Alt {
		return
	}
	pressed.Key = domKeyForVirtualKey(args.virtualKey())
	if pressed.Key == "" {
		return
	}
	h.mu.Lock()
	set := h.accelerators
	h.mu.Unlock()
	bound, ok := set.Match(pressed)
	if !ok {
		return
	}
	if err := args.putHandled(true); err != nil {
		h.config.Logf("browser host: page %s accelerator handled: %v", page.id, err)
	}
	detail, err := json.Marshal(bound)
	if err != nil {
		return
	}
	h.config.Report(page.id, ReportAccelerator, string(detail))
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
