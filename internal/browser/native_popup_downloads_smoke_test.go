//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestNativeEngineRealPopupDownloadsAndNesting drives this engine's popups
// against a loopback server. A popup whose first navigation is a download,
// held from the Manager for a second, downloads before its tab is listed:
// the download is listed in the popup's tab, finished. A popup's own popup
// becomes a tab of the same thread.
func TestNativeEngineRealPopupDownloadsAndNesting(t *testing.T) {
	window := smokeWindow(t)
	payload := bytes.Repeat([]byte("p"), 50_000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/popup.bin":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="popup.bin"`)
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			_, _ = w.Write(payload)
		default:
			_, _ = fmt.Fprintf(w, "<!doctype html><title>%s</title><p>%s</p>", r.URL.Path[1:], r.URL.Path[1:])
		}
	}))
	t.Cleanup(server.Close)
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	holdPopupAdoption(manager, time.Second)
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	access := Access{ThreadID: "native-smoke-popup-downloads", Workspace: resolvedTempDir(t)}
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	opener, err := manager.Open(ctx, access, server.URL+"/opener", OpenOptions{})
	if err != nil {
		t.Fatalf("open the opener: %v", err)
	}
	await := func(what string, done func() bool) {
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
	open := func(pageID, path string) {
		t.Helper()
		if _, _, err := manager.Evaluate(ctx, access, pageID, fmt.Sprintf(`window.open(%s); null`, jsonString(server.URL+path)), nil); err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
	}

	t.Run("a download a popup starts before its listing", func(t *testing.T) {
		open(opener.ID, "/popup.bin")
		await("the popup's download to finish", func() bool {
			listed := manager.CompanionState(access).Downloads
			return len(listed) == 1 && listed[0].State == downloadStateCompleted && listed[0].ReceivedBytes == int64(len(payload))
		})
		// In the popup's tab, not its opener's.
		var inOpener, inPopup int
		for _, p := range manager.ownedPages(access.ThreadID) {
			if p.id == opener.ID {
				inOpener += len(p.downloads.list())
			} else {
				inPopup += len(p.downloads.list())
			}
		}
		if inOpener != 0 || inPopup != 1 {
			t.Fatalf("the opener lists %d downloads and its popups %d, want the popup's one", inOpener, inPopup)
		}
	})

	t.Run("a popup's popup", func(t *testing.T) {
		open(opener.ID, "/parent")
		await("the parent popup's tab", func() bool { return tabNamed("parent") != "" })
		open(tabNamed("parent"), "/child")
		await("the child popup's tab", func() bool { return tabNamed("child") != "" })
	})
}
