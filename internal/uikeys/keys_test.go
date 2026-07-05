package uikeys

import (
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// TestBrowserBindings pins the keybinding shape so an accidental
// deletion or rename in keys.go reaches CI rather than a user finding
// out Ctrl+R stopped working. The map is the single source for three
// WebviewWindow surfaces (desktop, --connect client, WSL launcher);
// drift here re-introduces the bug the package was extracted to fix.
func TestBrowserBindings(t *testing.T) {
	want := []string{
		"CmdOrCtrl+plus",
		"CmdOrCtrl+=",
		"CmdOrCtrl+-",
		"CmdOrCtrl+r",
		"CmdOrCtrl+Shift+r",
		"Alt+Left",
		"Alt+Right",
		"F11",
		"Ctrl+Command+F",
	}

	got := Browser()
	if len(got) != len(want) {
		t.Fatalf("Browser() returned %d bindings, want %d (%v)", len(got), len(want), keys(got))
	}
	for _, k := range want {
		fn, ok := got[k]
		if !ok {
			t.Errorf("Browser() missing binding %q", k)
			continue
		}
		if fn == nil {
			t.Errorf("Browser()[%q] is nil", k)
		}
	}
}

// TestBrowserReturnsFreshMap confirms the package-doc claim that each
// call returns an independent map. Wails copies KeyBindings into the
// window at construction so aliasing isn't observable in production,
// but a future caller may mutate the returned map (e.g. to layer
// extra shortcuts) and shouldn't disturb other call sites.
func TestBrowserReturnsFreshMap(t *testing.T) {
	a := Browser()
	b := Browser()
	delete(a, "F11")
	if _, ok := b["F11"]; !ok {
		t.Fatal("mutating one Browser() result affected another; map is shared")
	}
}

// TestBrowserWithReloadOverridesReload pins the load-bearing reason
// BrowserWithReload exists: when reloadURL returns a non-empty URL,
// reload paths must call SetURL with that URL instead of letting Wails
// reload the (token-scrubbed) current URL. A regression here brings
// back the "Ctrl+R hits empty-token bootstrap → 404 → error page" UX
// the override was added to fix.
func TestBrowserWithReloadOverridesReload(t *testing.T) {
	want := "http://127.0.0.1:9999/?t=abc123"
	keys := BrowserWithReload(func() string { return want })
	for _, key := range []string{"CmdOrCtrl+r", "CmdOrCtrl+Shift+r"} {
		fw := &fakeWindow{}
		keys[key](fw)
		if len(fw.setURLCalls) != 1 || fw.setURLCalls[0] != want {
			t.Errorf("%s: SetURL calls = %v, want [%q]", key, fw.setURLCalls, want)
		}
		if fw.reloadCount != 0 || fw.forceReloadCount != 0 {
			t.Errorf("%s: fell through to native reload (reload=%d force=%d)", key, fw.reloadCount, fw.forceReloadCount)
		}
	}
}

// TestBrowserWithReloadFallsThroughOnEmpty confirms the reverse: when
// reloadURL is nil or returns "", reload paths use Wails' native
// Reload/ForceReload. The launcher relies on this — before the WSL
// backend boots, currentBackendURL() returns "" and reload should hit
// the static picker / loading pages via window.Reload(), not navigate
// to "".
func TestBrowserWithReloadFallsThroughOnEmpty(t *testing.T) {
	cases := map[string]func() string{
		"nil":   nil,
		"empty": func() string { return "" },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			keys := BrowserWithReload(fn)

			fw := &fakeWindow{}
			keys["CmdOrCtrl+r"](fw)
			if fw.reloadCount != 1 || len(fw.setURLCalls) != 0 {
				t.Errorf("Reload: SetURL=%v reload=%d, want SetURL=[] reload=1", fw.setURLCalls, fw.reloadCount)
			}

			fw = &fakeWindow{}
			keys["CmdOrCtrl+Shift+r"](fw)
			if fw.forceReloadCount != 1 || len(fw.setURLCalls) != 0 {
				t.Errorf("ForceReload: SetURL=%v force=%d, want SetURL=[] force=1", fw.setURLCalls, fw.forceReloadCount)
			}
		})
	}
}

func keys(m map[string]func(application.Window)) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// fakeWindow records SetURL/Reload/ForceReload calls without standing
// up a real Wails window. Embeds application.Window so the type
// satisfies the interface without re-implementing the other ~80
// methods; any method we don't override is a nil call and would panic
// — fine because only the three methods exercised by reload bindings
// are reached.
type fakeWindow struct {
	application.Window
	setURLCalls      []string
	reloadCount      int
	forceReloadCount int
}

func (f *fakeWindow) SetURL(s string) application.Window {
	f.setURLCalls = append(f.setURLCalls, s)
	return f
}

func (f *fakeWindow) Reload()      { f.reloadCount++ }
func (f *fakeWindow) ForceReload() { f.forceReloadCount++ }
