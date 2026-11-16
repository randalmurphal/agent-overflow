package uikeys

import (
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// scriptWindow records ExecJS beside fakeWindow's reload calls.
type scriptWindow struct {
	fakeWindow
	scripts []string
}

func (w *scriptWindow) ExecJS(js string) { w.scripts = append(w.scripts, js) }

func TestPageChordsRoutesReloadToTheFocusedBrowserPane(t *testing.T) {
	c := NewPageChords()
	bindings := c.Bindings(Browser())
	reload := bindings["CmdOrCtrl+r"]
	w := &scriptWindow{}

	reload(w)
	if w.reloadCount != 1 || len(w.scripts) != 0 {
		t.Fatalf("unfocused: reloads=%d scripts=%d, want the app reload", w.reloadCount, len(w.scripts))
	}

	if !c.HandleMessage(w, FocusMessage(true)) {
		t.Fatal("focus message not taken")
	}
	reload(w)
	if w.reloadCount != 1 || len(w.scripts) != 1 || w.scripts[0] != DispatchScript {
		t.Fatalf("focused: reloads=%d scripts=%v, want the chord handed to the SPA", w.reloadCount, w.scripts)
	}

	// Shift+R stays the app reload, focused or not.
	bindings["CmdOrCtrl+Shift+r"](w)
	if w.forceReloadCount != 1 {
		t.Fatalf("force reloads = %d, want 1", w.forceReloadCount)
	}

	c.HandleMessage(w, FocusMessage(false))
	reload(w)
	if w.reloadCount != 2 || len(w.scripts) != 1 {
		t.Fatalf("unfocused again: reloads=%d scripts=%d", w.reloadCount, len(w.scripts))
	}
}

func TestPageChordsReloadAppMessageReloadsAndClearsFocus(t *testing.T) {
	c := NewPageChords()
	bindings := c.Bindings(Browser())
	w := &scriptWindow{}
	c.HandleMessage(w, FocusMessage(true))

	c.HandleMessage(w, ReloadAppMessage)
	if w.reloadCount != 1 || c.BrowserFocused() {
		t.Fatalf("reloads=%d focused=%v, want the app reload and focus cleared", w.reloadCount, c.BrowserFocused())
	}
	bindings["CmdOrCtrl+r"](w)
	if w.reloadCount != 2 || len(w.scripts) != 0 {
		t.Fatalf("after reload-app: reloads=%d scripts=%d", w.reloadCount, len(w.scripts))
	}
	if c.HandleMessage(w, "ao:pane-geometry:{}") {
		t.Fatal("took a message without the prefix")
	}
}

func TestPageChordsDocumentLoadedForgetsFocusAndAdvertises(t *testing.T) {
	c := NewPageChords()
	c.Bindings(Browser())
	w := &scriptWindow{}
	c.HandleMessage(w, FocusMessage(true))

	c.DocumentLoaded(w)
	if c.BrowserFocused() {
		t.Fatal("a new document inherited the previous one's focus")
	}
	if len(w.scripts) != 1 || w.scripts[0] != CapabilityScript {
		t.Fatalf("scripts = %v, want the capability", w.scripts)
	}
}

func TestTrustedPageAcceptsOnlyThePageOrigin(t *testing.T) {
	const page = "http://127.0.0.1:4100/?t=x"
	cases := []struct {
		name   string
		origin *application.OriginInfo
		want   bool
	}{
		{"nil", nil, false},
		{"same origin", &application.OriginInfo{Origin: "http://127.0.0.1:4100/app"}, true},
		{"same origin and top", &application.OriginInfo{Origin: "http://127.0.0.1:4100", TopOrigin: "http://127.0.0.1:4100"}, true},
		{"other port", &application.OriginInfo{Origin: "http://127.0.0.1:4101"}, false},
		{"framed by another top", &application.OriginInfo{Origin: "http://127.0.0.1:4100", TopOrigin: "https://evil.example"}, false},
		{"file", &application.OriginInfo{Origin: "file:///C:/x.html"}, false},
		{"empty", &application.OriginInfo{}, false},
	}
	for _, tc := range cases {
		if got := TrustedPage(tc.origin, page); got != tc.want {
			t.Errorf("%s: TrustedPage = %v, want %v", tc.name, got, tc.want)
		}
	}
}
