package browser

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// The fake engine's downloads, so the harness can drive the pane's download
// list with no browser behind it. A navigation to an address on
// fakeDownloadHost is a response the fake saves rather than shows, as a real
// engine saves one it cannot show:
//
//	https://download.invalid/<name>?bytes=<n>[&hold][&fail]
//
// It reports the start, writes the file into the profile's download
// directory and reports half the bytes, then the finish. hold leaves it
// running at half until it is canceled; fail ends it as a failure the
// engine reports on its own. Reports are synchronous, inside the navigation.

const fakeDownloadHost = "download.invalid"

type fakeDownloadRequest struct {
	url   string
	name  string
	bytes int64
	hold  bool
	fail  bool
}

// fakeDownload is a held download: its total, for the report its cancel makes.
type fakeDownload struct {
	total int64
}

func fakeDownloadAddress(rawURL string) (fakeDownloadRequest, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(parsed.Hostname(), fakeDownloadHost) {
		return fakeDownloadRequest{}, false
	}
	query := parsed.Query()
	size, err := strconv.ParseInt(query.Get("bytes"), 10, 64)
	if err != nil || size < 0 {
		size = 1024
	}
	return fakeDownloadRequest{
		url: rawURL, name: path.Base(parsed.Path), bytes: size,
		hold: query.Has("hold"), fail: query.Has("fail"),
	}, true
}

// download runs one fake download begun in frame.
func (p *fakeProfile) download(frame string, request fakeDownloadRequest) error {
	events := p.engine.events
	if events.DownloadStarted == nil || events.DownloadProgress == nil {
		return errors.New("browser: this fake engine was built without engine events and cannot download")
	}
	id := fmt.Sprintf("fake-dl-%d", p.engine.seq.Add(1))
	if !events.DownloadStarted(downloadStart{Frame: frame, ID: id, URL: request.url, SuggestedName: request.name}) {
		return nil
	}
	file := filepath.Join(p.downloadDir, id)
	half := request.bytes / 2
	if err := os.WriteFile(file, make([]byte, half), 0o600); err != nil {
		events.DownloadProgress(downloadProgress{ID: id, State: downloadCanceled, Error: err.Error()})
		return nil
	}
	events.DownloadProgress(downloadProgress{ID: id, Received: float64(half), Total: float64(request.bytes), State: downloadInProgress})
	switch {
	case request.hold:
		p.mu.Lock()
		if p.held == nil {
			p.held = make(map[string]fakeDownload)
		}
		p.held[id] = fakeDownload{total: request.bytes}
		p.mu.Unlock()
		return nil
	case request.fail:
		if err := os.Remove(file); err != nil {
			return fmt.Errorf("browser: remove the failed fake download: %w", err)
		}
		events.DownloadProgress(downloadProgress{ID: id, Received: float64(half), Total: float64(request.bytes), State: downloadCanceled, Error: "the server closed the connection"})
		return nil
	}
	if err := os.WriteFile(file, make([]byte, request.bytes), 0o600); err != nil {
		events.DownloadProgress(downloadProgress{ID: id, State: downloadCanceled, Error: err.Error()})
		return nil
	}
	events.DownloadProgress(downloadProgress{ID: id, Received: float64(request.bytes), Total: float64(request.bytes), State: downloadCompleted, FilePath: file})
	return nil
}

// CancelDownload ends a held download and reports it canceled, as an engine
// reports the cancel it was asked for, releasing its record first as the
// native engines do. Any other id has already been let go, and its terminal
// report is out or on the way.
func (p *fakeProfile) CancelDownload(id string) error {
	p.mu.Lock()
	held, ok := p.held[id]
	delete(p.held, id)
	p.mu.Unlock()
	if !ok {
		return errDownloadEnded
	}
	if released := p.engine.downloadReleased; released != nil {
		released(id)
	}
	if p.engine.events.DownloadProgress != nil {
		p.engine.events.DownloadProgress(downloadProgress{ID: id, Received: float64(held.total / 2), Total: float64(held.total), State: downloadCanceled})
	}
	return nil
}

// finishHeld finishes a held download the way the native engines do: the
// file is complete, the engine's record of it is released, and only then is
// the finish reported.
func (p *fakeProfile) finishHeld(id string) error {
	p.mu.Lock()
	held, ok := p.held[id]
	delete(p.held, id)
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("browser: %s is not a held fake download", id)
	}
	file := filepath.Join(p.downloadDir, id)
	if err := os.WriteFile(file, make([]byte, held.total), 0o600); err != nil {
		return err
	}
	if released := p.engine.downloadReleased; released != nil {
		released(id)
	}
	p.engine.events.DownloadProgress(downloadProgress{ID: id, Received: float64(held.total), Total: float64(held.total), State: downloadCompleted, FilePath: file})
	return nil
}
