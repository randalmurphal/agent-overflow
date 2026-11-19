//go:build !windows

package browser

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
)

// heldAdoption is an engine's PopupOpened that admits each popup at once,
// as an engine reports a popup before anything else of it (engineEvents),
// and adopts it only after hold, so the popup decides its first
// navigations with no page: the window a profile's policy covers. It
// returns at once, as the Manager's does. The real-engine smokes use it.
func heldAdoption(manager *Manager, hold time.Duration) func(enginePopup) {
	return func(popup enginePopup) {
		if !manager.announcePopup(popup) {
			return
		}
		go func() {
			time.Sleep(hold)
			manager.runAdoption(popup)
		}()
	}
}

// TestHeadlessChromiumRealPopupDownloadsAndNesting drives a real Chromium's
// popups through the Manager, against a loopback server. A popup whose
// first navigation is a download, its adoption held for a second,
// downloads and closes before its tab is listed: the download is listed,
// finished, in its opener's tab. A popup a popup opens becomes a tab of the
// same thread, one the popup opens with no gesture while it is still held
// from its listing included. A frame a popup makes while held is the
// popup's once it is listed.
// Gated with TestHeadlessChromiumReal.
func TestHeadlessChromiumRealPopupDownloadsAndNesting(t *testing.T) {
	if os.Getenv("AO_HEADLESS_CHROMIUM_SMOKE") != "1" {
		t.Skip("set AO_HEADLESS_CHROMIUM_SMOKE=1 to launch this machine's real Chromium")
	}
	t.Setenv("HOME", t.TempDir())
	payload := bytes.Repeat([]byte("p"), 50_000)
	var seenMu sync.Mutex
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMu.Lock()
		seen[r.URL.Path] = true
		seenMu.Unlock()
		switch r.URL.Path {
		case "/popup.bin", "/framed.bin":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, r.URL.Path[1:]))
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			_, _ = w.Write(payload)
		case "/autoparent":
			// Opens its own popup as it loads, with no gesture, while the
			// Manager still holds it from its listing.
			_, _ = fmt.Fprint(w, `<!doctype html><title>autoparent</title><script>window.open("/autochild")</script>`)
		case "/framer":
			// Makes a same-origin frame as it loads, while the Manager
			// still holds it from its listing.
			_, _ = fmt.Fprint(w, `<!doctype html><title>framer</title><iframe id="f" src="/inner"></iframe>`)
		default:
			name := r.URL.Path[1:]
			_, _ = fmt.Fprintf(w, `<!doctype html><title>%s</title><button id="download" onclick='window.open("/popup.bin")'>d</button><button id="parent" onclick='window.open("/parent")'>p</button><button id="child" onclick='window.open("/child")'>c</button><button id="autoparent" onclick='window.open("/autoparent")'>a</button><button id="framer" onclick='window.open("/framer")'>f</button>`, name)
		}
	}))
	served := func(path string) bool {
		seenMu.Lock()
		defer seenMu.Unlock()
		return seen[path]
	}
	t.Cleanup(server.Close)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{HeadlessChromium: &HeadlessChromiumOptions{Reaper: procutiltest.Reaper()}})
	engine, ok := manager.engine.(*headlessEngine)
	if !ok {
		t.Fatalf("no headless engine: %T", manager.engine)
	}
	engine.events.PopupOpened = heldAdoption(manager, time.Second)
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	access := Access{ThreadID: "headless-popups", Workspace: resolvedTempDir(t)}
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	opener, err := manager.Open(ctx, access, server.URL+"/opener", OpenOptions{})
	if err != nil {
		t.Fatalf("open the opener: %v", err)
	}
	await := func(t *testing.T, what string, done func() bool) {
		t.Helper()
		deadline := time.After(30 * time.Second)
		for !done() {
			select {
			case <-ctx.Done():
			case <-deadline:
			case <-time.After(50 * time.Millisecond):
				continue
			}
			var tabs []string
			for _, tab := range manager.CompanionState(access).Pages {
				tabs = append(tabs, tab.Title+" "+tab.URL)
			}
			t.Fatalf("%s never happened: tabs %q, downloads %+v", what, tabs, manager.CompanionState(access).Downloads)
		}
	}
	tabNamed := func(title string) string {
		for _, tab := range manager.CompanionState(access).Pages {
			if tab.Title == title {
				return tab.ID
			}
		}
		return ""
	}
	click := func(t *testing.T, pageID, button string) {
		t.Helper()
		if _, err := manager.Click(ctx, access, pageID, "#"+button); err != nil {
			t.Fatalf("click %s: %v", button, err)
		}
	}

	t.Run("a download a popup starts before its listing", func(t *testing.T) {
		click(t, opener.ID, "download")
		await(t, "the popup's download to finish", func() bool {
			listed := manager.CompanionState(access).Downloads
			return len(listed) == 1 && listed[0].State == downloadStateCompleted && listed[0].ReceivedBytes == int64(len(payload))
		})
		// Chromium closes a popup whose first navigation is a download, so
		// the popup never becomes a tab and its opener's lists the download.
		var where []string
		for _, p := range manager.ownedPages(access.ThreadID) {
			p.downloadMu.Lock()
			if len(p.downloads) > 0 {
				where = append(where, p.id)
			}
			p.downloadMu.Unlock()
		}
		if len(where) != 1 || where[0] != opener.ID {
			t.Fatalf("the download is listed in tabs %v, want the opener's %s", where, opener.ID)
		}
	})

	t.Run("a popup's popup", func(t *testing.T) {
		click(t, opener.ID, "parent")
		await(t, "the parent popup's tab", func() bool { return tabNamed("parent") != "" })
		click(t, tabNamed("parent"), "child")
		await(t, "the child popup's tab", func() bool { return tabNamed("child") != "" })
	})

	t.Run("a popup's popup opened with no gesture before its opener's listing", func(t *testing.T) {
		click(t, opener.ID, "autoparent")
		await(t, "the parent's and the child's tabs", func() bool { return tabNamed("autoparent") != "" && tabNamed("autochild") != "" })
	})

	// The frame is the popup's from its listing on, though it reported
	// itself before: a download in it is listed in the popup's tab, and a
	// navigation of it reaches the server.
	t.Run("a frame a popup made before its listing", func(t *testing.T) {
		click(t, opener.ID, "framer")
		await(t, "the framer's tab", func() bool { return tabNamed("framer") != "" })
		framer := tabNamed("framer")
		await(t, "the frame's document", func() bool { return served("/inner") })
		navigateFrame := func(path string) {
			t.Helper()
			if _, _, err := manager.Evaluate(ctx, access, framer, fmt.Sprintf(`document.getElementById("f").contentWindow.location.href = %q; true`, path), nil); err != nil {
				t.Fatalf("navigate the frame to %s: %v", path, err)
			}
		}
		// A download commits no document, so the frame's attach is still
		// the one no event of the tab's reported.
		navigateFrame("/framed.bin")
		await(t, "the frame's download to finish in the framer's tab", func() bool {
			p, _, err := manager.lookupOwnedPage(access, framer)
			if err != nil {
				return false
			}
			p.downloadMu.Lock()
			defer p.downloadMu.Unlock()
			for _, d := range p.downloads {
				if d.SuggestedName == "framed.bin" {
					if d.State != downloadStateCompleted && d.State != downloadStateRunning {
						t.Fatalf("the frame's download ended %s", d.State)
					}
					return d.State == downloadStateCompleted
				}
			}
			return false
		})
		navigateFrame("/framed-later")
		await(t, "the frame's navigation to reach the server", func() bool { return served("/framed-later") })
	})
}
