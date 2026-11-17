//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// smokePNG is a 1x1 PNG.
var smokePNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")

// smokePDF is a one-page PDF.
const smokePDF = "%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]>>endobj\ntrailer<</Root 1 0 R>>\n%%EOF\n"

// TestNativeEngineRealDownloads drives this engine's downloads through the
// pane's list: the size the response announced, a finished file saved in the
// Downloads folder of the run's temporary home under a name that overwrites
// nothing, the user's cancel of a running one and an engine failure, each
// leaving no partial file, a name taken before the engine writes, a missing
// Downloads folder, and a finished file opened in a tab and by
// browser_open_file.
func TestNativeEngineRealDownloads(t *testing.T) {
	window := nativeSmokeWindow.Load()
	if window == nil {
		t.Skipf("set %s=1 to drive this platform's browser engine in a real window", nativeBrowserSmokeEnv)
	}
	release := make(chan struct{})
	payload := bytes.Repeat([]byte("a"), 300_000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attach := func(name, kind string, size int) {
			w.Header().Set("Content-Type", kind)
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
			w.Header().Set("Content-Length", fmt.Sprint(size))
		}
		switch r.URL.Path {
		case "/file.bin":
			attach("file.bin", "application/octet-stream", len(payload))
			_, _ = w.Write(payload)
		case "/chart.png":
			attach("chart.png", "image/png", len(smokePNG))
			_, _ = w.Write(smokePNG)
		case "/doc.pdf":
			attach("doc.pdf", "application/pdf", len(smokePDF))
			_, _ = w.Write([]byte(smokePDF))
		case "/slow.bin", "/broken.bin":
			attach(strings.TrimPrefix(r.URL.Path, "/"), "application/octet-stream", 10_000_000)
			_, _ = w.Write(payload[:65536])
			w.(http.Flusher).Flush()
			if r.URL.Path == "/broken.bin" {
				// A connection dropped short of the announced length.
				panic(http.ErrAbortHandler)
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		default:
			_, _ = fmt.Fprint(w, "<!doctype html><title>files</title><p>files</p>")
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{NativeWindow: window.NativeWindow})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	access := Access{ThreadID: "native-smoke-downloads", Workspace: resolvedTempDir(t)}
	t.Cleanup(func() {
		if err := manager.CloseThread(context.Background(), access.ThreadID); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	page, err := manager.Open(ctx, access, server.URL+"/", OpenOptions{})
	if err != nil {
		t.Fatalf("open the page: %v", err)
	}
	download := func(path string) ThreadDownload {
		t.Helper()
		before := len(manager.CompanionState(access).Downloads)
		if _, _, err := manager.Evaluate(ctx, access, page.ID, fmt.Sprintf(`location.href = %q; true`, server.URL+path), nil); err != nil {
			t.Fatalf("start %s: %v", path, err)
		}
		for {
			if listed := manager.CompanionState(access).Downloads; len(listed) > before {
				return listed[0]
			}
			select {
			case <-ctx.Done():
				t.Fatalf("%s never started a download", path)
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	settled := func(id string, want func(ThreadDownload) bool) ThreadDownload {
		t.Helper()
		for {
			for _, item := range manager.CompanionState(access).Downloads {
				if item.ID == id && want(item) {
					return item
				}
			}
			select {
			case <-ctx.Done():
				t.Fatalf("download %s never reached the expected state: %+v", id, manager.CompanionState(access).Downloads)
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	notRunning := func(item ThreadDownload) bool { return item.State != downloadStateRunning }
	downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
	// folder lists the Downloads folder, which only this run writes into.
	folder := func() []string {
		t.Helper()
		entries, err := os.ReadDir(downloads)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names
	}
	// unchanged waits out a moment for the folder to be names again, as an
	// engine may remove a partial file just after its report.
	unchanged := func(want []string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			got := folder()
			if slices.Equal(got, want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the Downloads folder holds %q, want %q", got, want)
			}
		}
	}

	t.Run("a finished download is saved in Downloads and keeps its size", func(t *testing.T) {
		item := settled(download("/file.bin").ID, notRunning)
		if item.State != downloadStateCompleted || item.ReceivedBytes != int64(len(payload)) || item.TotalBytes != int64(len(payload)) || item.Name != "file.bin" {
			t.Fatalf("the download settled as %+v", item)
		}
		saved, err := os.ReadFile(filepath.Join(downloads, "file.bin"))
		if err != nil || !bytes.Equal(saved, payload) {
			t.Fatalf("the saved file: %d bytes, %v", len(saved), err)
		}
	})
	t.Run("a second download of a name takes the next free one", func(t *testing.T) {
		first, err := os.Stat(filepath.Join(downloads, "file.bin"))
		if err != nil {
			t.Fatal(err)
		}
		item := settled(download("/file.bin").ID, notRunning)
		if item.State != downloadStateCompleted || item.Name != "file (1).bin" {
			t.Fatalf("the second download settled as %+v, want file (1).bin", item)
		}
		again, err := os.Stat(filepath.Join(downloads, "file.bin"))
		if err != nil || !sameDownloadedFile(first, again) {
			t.Fatalf("the first file changed: %v", err)
		}
		// browser_open_file opens the download and not a file beside it.
		saved := filepath.Join(downloads, "file (1).bin")
		if _, err := manager.authorizeFile(access, saved); err != nil {
			t.Fatalf("the finished download is refused: %v", err)
		}
		neighbor := filepath.Join(downloads, "neighbor.txt")
		if err := os.WriteFile(neighbor, []byte("not a download"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.authorizeFile(access, neighbor); err == nil {
			t.Fatal("a file beside the download is allowed")
		}
		if err := os.Remove(neighbor); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the user's cancel stops a running download", func(t *testing.T) {
		before := folder()
		item := settled(download("/slow.bin").ID, func(item ThreadDownload) bool { return item.ReceivedBytes > 0 })
		if item.TotalBytes != 10_000_000 {
			t.Errorf("a running download reports a total of %d, want the announced 10000000", item.TotalBytes)
		}
		if err := manager.CancelThreadDownload(access, item.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if got := settled(item.ID, notRunning); got.State != downloadStateCanceled {
			t.Fatalf("a canceled download settled as %+v", got)
		}
		unchanged(before)
	})
	t.Run("a dropped connection is a failure", func(t *testing.T) {
		before := folder()
		item := settled(download("/broken.bin").ID, notRunning)
		// WebKitGTK follows "failed" with "finished"; a completion reported
		// for it would be a finish of a file the engine removed.
		if item.State != downloadStateFailed || item.Error == "" {
			t.Fatalf("a dropped download settled as %+v, want failed with the engine's reason", item)
		}
		t.Logf("the engine's reason: %s", item.Error)
		unchanged(before)
	})
	t.Run("a name taken as the download starts keeps that file and takes the next", func(t *testing.T) {
		theirs := takeCheckedDownloadPath(t)
		item := settled(download("/file.bin").ID, notRunning)
		if item.State != downloadStateCompleted || item.Name != "file (3).bin" {
			t.Fatalf("the download settled as %+v, want file (3).bin", item)
		}
		got, err := os.ReadFile(*theirs)
		if err != nil || string(got) != "theirs" || filepath.Base(*theirs) != "file (2).bin" {
			t.Fatalf("the other program's file %s reads %q, %v", *theirs, got, err)
		}
		if err := os.Remove(*theirs); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a cancel as soon as the download starts leaves no file", func(t *testing.T) {
		before := folder()
		item := download("/slow.bin")
		if err := manager.CancelThreadDownload(access, item.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if got := settled(item.ID, notRunning); got.State != downloadStateCanceled {
			t.Fatalf("the download settled as %+v", got)
		}
		unchanged(before)
	})
	t.Run("a missing Downloads folder fails the download", func(t *testing.T) {
		parked := downloads + ".parked"
		if err := os.Rename(downloads, parked); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.Rename(parked, downloads); err != nil {
				t.Fatal(err)
			}
		}()
		item := settled(download("/chart.png").ID, notRunning)
		if item.State != downloadStateFailed || !strings.Contains(item.Error, "does not exist") {
			t.Fatalf("the download settled as %+v, want failed for the missing folder", item)
		}
		if _, err := os.Stat(downloads); err == nil {
			t.Fatal("the download created the Downloads folder")
		}
	})
	for _, file := range []struct{ path, contentType string }{{"/chart.png", "image/png"}, {"/doc.pdf", "application/pdf"}} {
		t.Run("a finished "+file.contentType+" opens in a tab", func(t *testing.T) {
			item := settled(download(file.path).ID, notRunning)
			if item.State != downloadStateCompleted || !item.Openable {
				t.Fatalf("the download settled as %+v, want completed and openable", item)
			}
			before := len(manager.CompanionState(access).Downloads)
			opened, err := manager.OpenThreadDownload(ctx, access, item.ID)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if !strings.HasPrefix(opened.URL, "file://") || !strings.HasSuffix(opened.URL, item.Name) {
				t.Fatalf("the tab is at %s, want the downloaded file", opened.URL)
			}
			// A type the engine cannot show would download again instead.
			time.Sleep(time.Second)
			if after := len(manager.CompanionState(access).Downloads); after != before {
				t.Fatalf("opening %s started another download", item.Name)
			}
			value, _, err := manager.Evaluate(ctx, access, opened.ID, `document.contentType`, nil)
			if err != nil {
				t.Fatalf("read the tab: %v", err)
			}
			t.Logf("the tab shows %s as %v", item.Name, value)
		})
	}
}
