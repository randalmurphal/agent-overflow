package keybindings

import "testing"

func TestAcceleratorsResolveModPerPlatformAndSkipTyping(t *testing.T) {
	bindings := []Keybinding{
		{Key: "mod+w", Command: "pane.close"},
		{Key: "alt+shift+r", Command: "terminal.refresh"},
		{Key: "cmd+shift+p", Command: "palette"},
		{Key: "shift++", Command: "plus"},
		{Key: "escape", Command: "typing.bare"},
		{Key: "shift+enter", Command: "typing.shift"},
		{Key: "", Command: "unbound"},
		{Key: "ctrl + + w", Command: "garbage"},
	}
	mac := Accelerators(bindings, true)
	if _, ok := mac[Accelerator{Key: "w", Meta: true}]; !ok {
		t.Fatalf("mac: mod+w should resolve to cmd+w: %v", mac)
	}
	linux := Accelerators(bindings, false)
	if _, ok := linux[Accelerator{Key: "w", Ctrl: true}]; !ok {
		t.Fatalf("linux: mod+w should resolve to ctrl+w: %v", linux)
	}
	for _, want := range []Accelerator{
		{Key: "r", Alt: true, Shift: true},
		{Key: "p", Meta: true, Shift: true},
	} {
		if _, ok := mac[want]; !ok {
			t.Fatalf("missing %+v in %v", want, mac)
		}
	}
	for _, never := range []Accelerator{
		{Key: "+", Shift: true},
		{Key: "escape"},
		{Key: "enter", Shift: true},
	} {
		if _, ok := mac[never]; ok {
			t.Fatalf("a chord without ctrl/meta/alt was claimed: %+v", never)
		}
	}
	if len(mac) != 3 {
		t.Fatalf("set = %v, want exactly the three modifier chords", mac)
	}
}

func TestAcceleratorMatchTriesTheOtherShiftGlyph(t *testing.T) {
	set := Accelerators([]Keybinding{{Key: "cmd+shift+1", Command: "a"}, {Key: "ctrl+shift+~", Command: "b"}}, true)
	got, ok := set.Match(Accelerator{Key: "!", Meta: true, Shift: true})
	if !ok || got.Key != "1" {
		t.Fatalf("shifted glyph should match the unshifted binding and return it: %+v %v", got, ok)
	}
	got, ok = set.Match(Accelerator{Key: "`", Ctrl: true, Shift: true})
	if !ok || got.Key != "~" {
		t.Fatalf("unshifted key should match the shifted binding: %+v %v", got, ok)
	}
	if _, ok := set.Match(Accelerator{Key: "1", Meta: true}); ok {
		t.Fatal("the glyph fallback must need Shift")
	}
	if _, ok := AcceleratorSet(nil).Match(Accelerator{Key: "w", Meta: true}); ok {
		t.Fatal("a nil set matches nothing")
	}
}
