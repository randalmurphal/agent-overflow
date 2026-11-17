//go:build !windows

package browser

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
)

// TestHeadlessChromiumRealPopupDownloadsAndNesting drives a real Chromium's
// popups through the Manager, against a loopback server. A popup whose
// first navigation is a download, held from the Manager for a second,
// downloads and closes before its tab is listed: the download is listed,
// finished, in its opener's tab. A popup a popup opens becomes a tab of the same thread.
// Gated with TestHeadlessChromiumReal.
func TestHeadlessChromiumRealPopupDownloadsAndNesting(t *testing.T) {
	if os.Getenv("AO_HEADLESS_CHROMIUM_SMOKE") != "1" {
		t.Skip("set AO_HEADLESS_CHROMIUM_SMOKE=1 to launch this machine's real Chromium")
	}
	t.Setenv("HOME", t.TempDir())
	payload := bytes.Repeat([]byte("p"), 50_000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/popup.bin":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="popup.bin"`)
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			_, _ = w.Write(payload)
		default:
			name := r.URL.Path[1:]
			_, _ = fmt.Fprintf(w, `<!doctype html><title>%s</title><button id="download" onclick='window.open("/popup.bin")'>d</button><button id="parent" onclick='window.open("/parent")'>p</button><button id="child" onclick='window.open("/child")'>c</button>`, name)
		}
	}))
	t.Cleanup(server.Close)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{HeadlessChromium: &HeadlessChromiumOptions{Reaper: procutiltest.Reaper()}})
	engine, ok := manager.engine.(*headlessEngine)
	if !ok {
		t.Fatalf("no headless engine: %T", manager.engine)
	}
	adopt := engine.events.PopupOpened
	engine.events.PopupOpened = func(popup enginePopup) {
		time.Sleep(time.Second)
		adopt(popup)
	}
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
		for !done() {
			select {
			case <-ctx.Done():
				t.Fatalf("%s never happened: tabs %+v, downloads %+v", what, manager.CompanionState(access).Pages, manager.CompanionState(access).Downloads)
			case <-time.After(50 * time.Millisecond):
			}
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
}
