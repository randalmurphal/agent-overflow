package uikeys

import (
	"os/exec"
	"strings"
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
	c.DocumentLoaded(w)
	w.scripts = nil

	reload(w)
	if w.reloadCount != 1 || len(w.scripts) != 0 {
		t.Fatalf("unfocused: reloads=%d scripts=%d, want the app reload", w.reloadCount, len(w.scripts))
	}

	if !c.HandleMessage(w, RouteMessage(1, 1, true)) {
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

	c.HandleMessage(w, RouteMessage(1, 2, false))
	reload(w)
	if w.reloadCount != 2 || len(w.scripts) != 1 {
		t.Fatalf("unfocused again: reloads=%d scripts=%d", w.reloadCount, len(w.scripts))
	}
}

func TestPageChordsReloadAppMessageReloadsAndClearsFocus(t *testing.T) {
	c := NewPageChords()
	bindings := c.Bindings(Browser())
	w := &scriptWindow{}
	c.DocumentLoaded(w)
	w.scripts = nil
	c.HandleMessage(w, RouteMessage(1, 1, true))

	// A request from a document the window no longer shows reloads nothing.
	c.HandleMessage(w, ReloadAppMessage(0))
	if w.reloadCount != 0 || !c.Routing() {
		t.Fatalf("another document's reload request: reloads=%d focused=%v", w.reloadCount, c.Routing())
	}
	c.HandleMessage(w, ReloadAppMessage(1))
	if w.reloadCount != 1 || c.Routing() {
		t.Fatalf("reloads=%d focused=%v, want the app reload and focus cleared", w.reloadCount, c.Routing())
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
	c.DocumentLoaded(w)
	c.HandleMessage(w, RouteMessage(1, 1, true))

	c.DocumentLoaded(w)
	if c.Routing() {
		t.Fatal("a new document inherited the previous one's focus")
	}
	if len(w.scripts) != 2 || w.scripts[0] != CapabilityScript(1) || w.scripts[1] != CapabilityScript(2) {
		t.Fatalf("scripts = %v, want each document's capability", w.scripts)
	}
	// The previous document's report, arriving late, counts for nothing.
	c.HandleMessage(w, RouteMessage(1, 2, true))
	if c.Routing() {
		t.Fatal("the previous document's late report routed the chord")
	}
}

// Raw messages are handled on a goroutine each, so reports arrive in any
// order: the newest of the document wins, whichever lands last.
func TestPageChordsAppliesOnlyTheNewestReport(t *testing.T) {
	c := NewPageChords()
	c.Bindings(Browser())
	w := &scriptWindow{}
	c.DocumentLoaded(w)
	// Left and refocused the browser pane: false (2), then true (3), applied
	// in the opposite order.
	c.HandleMessage(w, RouteMessage(1, 1, true))
	c.HandleMessage(w, RouteMessage(1, 3, true))
	c.HandleMessage(w, RouteMessage(1, 2, false))
	if !c.Routing() {
		t.Fatal("an older report overrode the newest")
	}
	c.HandleMessage(w, RouteMessage(1, 3, false))
	if !c.Routing() {
		t.Fatal("a repeated number applied")
	}
	for _, garbled := range []string{
		MessagePrefix + "route-reload:1",
		MessagePrefix + "route-reload:1:9:2",
		MessagePrefix + "route-reload:x:9:0",
		MessagePrefix + "route-reload:1:9:0:0",
		MessagePrefix + "reload-app:x",
	} {
		if !c.HandleMessage(w, garbled) || !c.Routing() || w.reloadCount != 0 {
			t.Fatalf("%q changed the routing or reloaded", garbled)
		}
	}
}

// Two runtime-ready callbacks of one document number it 1 then 2, and their
// scripts may run 2 then 1: the document keeps 2, the number the window
// accepts reports for, and announces itself once.
func TestCapabilityScriptsInReverseOrderLeaveTheNewestNumber(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required: the capability script runs in a document")
	}
	c := NewPageChords()
	c.Bindings(Browser())
	w := &scriptWindow{}
	c.DocumentLoaded(w)
	c.DocumentLoaded(w)
	if len(w.scripts) != 2 {
		t.Fatalf("scripts = %v", w.scripts)
	}
	program := "const window = { ready: 0, dispatchEvent(e) { if (e.type === '" + ReadyEvent + "') this.ready++; } };\n" +
		w.scripts[1] + "\n" + w.scripts[0] + "\n" +
		"process.stdout.write(window." + CapabilityGlobal + " + ' ' + window.ready);"
	out, err := exec.Command(node, "-e", program).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "2 1" {
		t.Fatalf("after the scripts ran newest first the document holds number and announcements %q, want \"2 1\"", got)
	}
	c.HandleMessage(w, RouteMessage(2, 1, true))
	if !c.Routing() {
		t.Fatal("a report under the document's number did not route the chord")
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
