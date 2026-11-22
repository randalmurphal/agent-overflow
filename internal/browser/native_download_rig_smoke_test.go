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

// The fixtures TestNativeEngineRealDownloads drives its scenarios with: the
// staging folder, the files the server sends, and the page the downloads
// start from.

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

// smokeDownloadRig is a Manager on the run's window with one page open on
// server, whose paths are downloads: file.bin (payload), chart.png,
// doc.pdf, gated.bin (the first 64 KiB, the rest once gate closes),
// slow.bin (64 KiB of an announced 10 MB, then waits), and broken.bin (the
// same, then a dropped connection). downloads is the run's Downloads
// folder, which only this run writes into.
type smokeDownloadRig struct {
	t         *testing.T
	ctx       context.Context
	manager   *Manager
	access    Access
	page      PageInfo
	server    *httptest.Server
	payload   []byte
	gate      chan struct{}
	downloads string
}

func newSmokeDownloadRig(t *testing.T) *smokeDownloadRig {
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
		if err := manager.CloseThread(context.Background(), access.ThreadID()); err != nil {
			t.Errorf("close the thread's pages: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	page, err := manager.Open(ctx, access, server.URL+"/", OpenOptions{})
	if err != nil {
		t.Fatalf("open the page: %v", err)
	}
	return &smokeDownloadRig{
		t: t, ctx: ctx, manager: manager, access: access, page: page,
		server: server, payload: payload, gate: gate,
		downloads: filepath.Join(os.Getenv("HOME"), "Downloads"),
	}
}

// download starts path in the page and answers its listed download.
func (r *smokeDownloadRig) download(path string) ThreadDownload {
	t := r.t
	t.Helper()
	before := len(r.manager.CompanionState(r.access.Caller()).Downloads)
	if _, _, err := r.manager.Evaluate(r.ctx, r.access, PageID(r.page.ID), fmt.Sprintf(`location.href = %q; true`, r.server.URL+path), nil); err != nil {
		t.Fatalf("start %s: %v", path, err)
	}
	for {
		if listed := r.manager.CompanionState(r.access.Caller()).Downloads; len(listed) > before {
			return listed[0]
		}
		select {
		case <-r.ctx.Done():
			t.Fatalf("%s never started a download", path)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// settled waits for download id to be listed in a state want accepts.
func (r *smokeDownloadRig) settled(id string, want func(ThreadDownload) bool) ThreadDownload {
	t := r.t
	t.Helper()
	for {
		for _, item := range r.manager.CompanionState(r.access.Caller()).Downloads {
			if item.ID == id && want(item) {
				return item
			}
		}
		select {
		case <-r.ctx.Done():
			t.Fatalf("download %s never reached the expected state: %+v", id, r.manager.CompanionState(r.access.Caller()).Downloads)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// folder lists the Downloads folder.
func (r *smokeDownloadRig) folder() []string {
	t := r.t
	t.Helper()
	entries, err := os.ReadDir(r.downloads)
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
func (r *smokeDownloadRig) unchanged(want []string) {
	t := r.t
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		got := r.folder()
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Downloads folder holds %q, want %q", got, want)
		}
	}
}
