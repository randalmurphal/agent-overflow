//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// smokeStagingDir is the directory the engine writes the run's downloads
// into before the Manager publishes them: the app's staging directory in
// the run's Downloads folder.
func smokeStagingDir() string {
	return filepath.Join(os.Getenv("HOME"), "Downloads", stagingDirName)
}

// smokeStagedNames lists the staging directory, empty while it is absent:
// a file in it by its name, and the files in a run directory but its lock
// as "<run>/<name>".
func smokeStagedNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(smokeStagingDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
			continue
		}
		inRun, err := os.ReadDir(filepath.Join(smokeStagingDir(), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range inRun {
			if file.Name() != runLockName {
				names = append(names, entry.Name()+"/"+file.Name())
			}
		}
	}
	return names
}

// smokePNG is a 1x1 PNG.
var smokePNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")

// smokePDF is a one-page PDF.
const smokePDF = "%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]>>endobj\ntrailer<</Root 1 0 R>>\n%%EOF\n"

// TestNativeEngineRealDownloads drives this engine's downloads through the
// pane's list: the size the response announced, a finished file published
// into the Downloads folder of the run's temporary home under a name that
// replaces nothing, staged files an earlier run left behind, a running
// download showing nothing in Downloads but the staging folder, the user's
// cancel of a running one and an engine failure, each leaving Downloads as
// it was, a cancel after the user put a file at the download's name, a
// staged file swapped for a link while its finish waits in the engine's
// queue, a name taken while the download runs, a staging folder that is a
// link, a missing Downloads folder, and a finished file opened in a tab and
// by browser_open_file.
func TestNativeEngineRealDownloads(t *testing.T) {
	window := smokeWindow(t)
	release, gate := make(chan struct{}), make(chan struct{})
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
		case "/gated.bin":
			attach("gated.bin", "application/octet-stream", len(payload))
			_, _ = w.Write(payload[:65536])
			w.(http.Flusher).Flush()
			select {
			case <-gate:
				_, _ = w.Write(payload[65536:])
			case <-r.Context().Done():
			}
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
	access := manager.Bind(Caller{ThreadID: "native-smoke-downloads", Workspace: resolvedTempDir(t)})
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
		before := len(manager.CompanionState(access.Caller()).Downloads)
		if _, _, err := manager.Evaluate(ctx, access, page.ID, fmt.Sprintf(`location.href = %q; true`, server.URL+path), nil); err != nil {
			t.Fatalf("start %s: %v", path, err)
		}
		for {
			if listed := manager.CompanionState(access.Caller()).Downloads; len(listed) > before {
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
			for _, item := range manager.CompanionState(access.Caller()).Downloads {
				if item.ID == id && want(item) {
					return item
				}
			}
			select {
			case <-ctx.Done():
				t.Fatalf("download %s never reached the expected state: %+v", id, manager.CompanionState(access.Caller()).Downloads)
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
		if _, err := manager.authorizeFile(access.Caller(), saved); err != nil {
			t.Fatalf("the finished download is refused: %v", err)
		}
		neighbor := filepath.Join(downloads, "neighbor.txt")
		if err := os.WriteFile(neighbor, []byte("not a download"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.authorizeFile(access.Caller(), neighbor); err == nil {
			t.Fatal("a file beside the download is allowed")
		}
		if err := os.Remove(neighbor); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("staged files an earlier run left never fail a download, and an ended run's go", func(t *testing.T) {
		staging := smokeStagingDir()
		if names := smokeStagedNames(t); len(names) != 0 {
			t.Fatalf("with no download running the staging folder holds %q", names)
		}
		// An ended run's directory, its lock free, and a file an earlier
		// release staged without run directories.
		ended := filepath.Join(staging, "run-000000000000")
		if err := os.MkdirAll(ended, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{filepath.Join(ended, runLockName), filepath.Join(ended, "dl-1"), filepath.Join(staging, "dl-1")} {
			if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		item := settled(download("/file.bin").ID, notRunning)
		if item.State != downloadStateCompleted || item.Name != "file (2).bin" {
			t.Fatalf("the download after the leftovers settled as %+v", item)
		}
		if names := smokeStagedNames(t); !slices.Equal(names, []string{"dl-1"}) {
			t.Fatalf("the staging folder holds %q, want only the earlier release's file", names)
		}
		if err := os.Remove(filepath.Join(staging, "dl-1")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(staging); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the user's cancel stops a running download", func(t *testing.T) {
		before := folder()
		item := settled(download("/slow.bin").ID, func(item ThreadDownload) bool { return item.ReceivedBytes > 0 })
		if item.TotalBytes != 10_000_000 {
			t.Errorf("a running download reports a total of %d, want the announced 10000000", item.TotalBytes)
		}
		// Nothing but finished files is ever directly in Downloads: the
		// running download is in the staging folder.
		during := slices.Sorted(slices.Values(append(slices.Clone(before), stagingDirName)))
		if got := folder(); !slices.Equal(got, during) {
			t.Fatalf("while the download runs the Downloads folder holds %q, want %q", got, during)
		}
		// WebKitGTK writes "<staged>.wkdownload" beside the staged file
		// until the download finishes.
		names := smokeStagedNames(t)
		if len(names) == 0 || slices.ContainsFunc(names, func(name string) bool {
			return !stagedName(filepath.Base(name)) || strings.TrimSuffix(name, ".wkdownload") != names[0]
		}) {
			t.Fatalf("while the download runs the staging folder holds %q", names)
		}
		t.Logf("while the download runs the staging folder holds %q", names)
		if err := manager.CancelThreadDownload(access, DownloadKey(item.ID)); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if got := settled(item.ID, notRunning); got.State != downloadStateCanceled {
			t.Fatalf("a canceled download settled as %+v", got)
		}
		unchanged(before)
	})
	t.Run("a cancel never removes a file put at the download's name", func(t *testing.T) {
		item := settled(download("/slow.bin").ID, func(item ThreadDownload) bool { return item.ReceivedBytes > 0 })
		// The user renames the download's file and saves an unrelated one
		// under its name. A running download writes nothing in Downloads,
		// so there is nothing to rename.
		name := filepath.Join(downloads, "slow.bin")
		if _, err := os.Lstat(name); err == nil {
			if err := os.Rename(name, name+".original"); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(name, []byte("unrelated"), 0o600); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = os.Remove(name)
			_ = os.Remove(name + ".original")
		}()
		// The staging folder goes with the canceled download's staged file.
		before := slices.DeleteFunc(folder(), func(name string) bool { return name == stagingDirName })
		if err := manager.CancelThreadDownload(access, DownloadKey(item.ID)); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if got := settled(item.ID, notRunning); got.State != downloadStateCanceled {
			t.Fatalf("a canceled download settled as %+v", got)
		}
		unchanged(before)
		if got, err := os.ReadFile(name); err != nil || string(got) != "unrelated" {
			t.Fatalf("the unrelated file reads %q, %v", got, err)
		}
	})
	t.Run("a staged file swapped for a link before its finish is handled grants nothing", func(t *testing.T) {
		private := filepath.Join(t.TempDir(), "private.txt")
		if err := os.WriteFile(private, []byte("private"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := folder()
		staging := smokeStagingDir()
		known := smokeStagedNames(t)
		release := holdDownloadEvents(t, manager)
		released := false
		defer func() {
			if !released {
				release()
			}
		}()
		count := len(manager.CompanionState(access.Caller()).Downloads)
		if _, _, err := manager.Evaluate(ctx, access, page.ID, fmt.Sprintf(`location.href = %q; true`, server.URL+"/file.bin"), nil); err != nil {
			t.Fatal(err)
		}
		staged := ""
		for staged == "" {
			for _, name := range smokeStagedNames(t) {
				info, err := os.Lstat(filepath.Join(staging, name))
				if err == nil && !slices.Contains(known, name) && info.Size() == int64(len(payload)) && !strings.Contains(name, ".") {
					staged = filepath.Join(staging, name)
				}
			}
			select {
			case <-ctx.Done():
				t.Fatalf("the engine never finished writing into %s", staging)
			case <-time.After(50 * time.Millisecond):
			}
		}
		// The engine's finish is now behind the held queue.
		time.Sleep(time.Second)
		if err := os.Remove(staged); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(private, staged); err != nil {
			t.Fatal(err)
		}
		release()
		released = true
		var item ThreadDownload
		for item.ID == "" {
			if listed := manager.CompanionState(access.Caller()).Downloads; len(listed) > count {
				item = listed[0]
			}
			select {
			case <-ctx.Done():
				t.Fatal("the held download never listed")
			case <-time.After(50 * time.Millisecond):
			}
		}
		item = settled(item.ID, notRunning)
		if item.State != downloadStateFailed || !strings.Contains(item.Error, "not a regular file") {
			t.Fatalf("the swapped download settled as %+v, want failed", item)
		}
		if _, err := manager.authorizeFile(access.Caller(), private); err == nil {
			t.Fatal("the link's target is openable")
		}
		unchanged(before)
		if got, _ := os.ReadFile(private); string(got) != "private" {
			t.Fatalf("the link's target reads %q", got)
		}
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
	t.Run("a name taken while the download runs keeps that file and takes the next", func(t *testing.T) {
		item := settled(download("/gated.bin").ID, func(item ThreadDownload) bool { return item.ReceivedBytes > 0 })
		theirs := filepath.Join(downloads, "gated.bin")
		if err := os.WriteFile(theirs, []byte("theirs"), 0o600); err != nil {
			t.Fatal(err)
		}
		close(gate)
		item = settled(item.ID, notRunning)
		if item.State != downloadStateCompleted || item.Name != "gated (1).bin" {
			t.Fatalf("the download settled as %+v, want gated (1).bin", item)
		}
		if got, err := os.ReadFile(theirs); err != nil || string(got) != "theirs" {
			t.Fatalf("the other program's file reads %q, %v", got, err)
		}
		for _, name := range []string{theirs, filepath.Join(downloads, "gated (1).bin")} {
			if err := os.Remove(name); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("a cancel as soon as the download starts leaves no file", func(t *testing.T) {
		before := folder()
		item := download("/slow.bin")
		if err := manager.CancelThreadDownload(access, DownloadKey(item.ID)); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if got := settled(item.ID, notRunning); got.State != downloadStateCanceled {
			t.Fatalf("the download settled as %+v", got)
		}
		unchanged(before)
	})
	t.Run("a staging folder that is a link fails the download before the engine writes", func(t *testing.T) {
		staging := smokeStagingDir()
		if err := os.Remove(staging); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("with no download running the staging folder is not empty: %v", err)
		}
		target := t.TempDir()
		if err := os.Symlink(target, staging); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.Remove(staging); err != nil {
				t.Error(err)
			}
		}()
		before := folder()
		item := settled(download("/chart.png").ID, notRunning)
		if item.State != downloadStateFailed || !strings.Contains(item.Error, "is not a folder") {
			t.Fatalf("a download through a linked staging folder settled as %+v", item)
		}
		if names, err := os.ReadDir(target); err != nil || len(names) != 0 {
			t.Fatalf("the link's target holds %d entries, %v", len(names), err)
		}
		unchanged(before)
	})
	t.Run("a missing Downloads folder is created", func(t *testing.T) {
		parked := downloads + ".parked"
		if err := os.Rename(downloads, parked); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.RemoveAll(downloads); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(parked, downloads); err != nil {
				t.Fatal(err)
			}
		}()
		item := settled(download("/chart.png").ID, notRunning)
		if item.State != downloadStateCompleted {
			t.Fatalf("the download settled as %+v, want completed in a created folder", item)
		}
		if got := folder(); !slices.Equal(got, []string{"chart.png"}) {
			t.Fatalf("the created Downloads folder holds %q", got)
		}
	})
	for _, file := range []struct{ path, contentType string }{{"/chart.png", "image/png"}, {"/doc.pdf", "application/pdf"}} {
		t.Run("a finished "+file.contentType+" opens in a tab", func(t *testing.T) {
			item := settled(download(file.path).ID, notRunning)
			if item.State != downloadStateCompleted || !item.Openable {
				t.Fatalf("the download settled as %+v, want completed and openable", item)
			}
			before := len(manager.CompanionState(access.Caller()).Downloads)
			opened, err := manager.OpenThreadDownload(ctx, access, DownloadKey(item.ID))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if !strings.HasPrefix(opened.URL, "file://") || !strings.HasSuffix(opened.URL, item.Name) {
				t.Fatalf("the tab is at %s, want the downloaded file", opened.URL)
			}
			// A type the engine cannot show would download again instead.
			time.Sleep(time.Second)
			if after := len(manager.CompanionState(access.Caller()).Downloads); after != before {
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
