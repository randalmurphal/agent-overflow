package browser

import (
	"errors"
	"fmt"
	"io/fs"
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
// It saves where the engine it stands in for does (fakeEngine.saving): the
// staged file the Manager names, as WebKitGTK does, canceling before a
// write when there is none; the profile's directory under the download's
// id, as headless Chromium does; or the user's Downloads folder under a
// free name, as WebView2 does, removing its own partial file there on a
// cancel or a failure. It reports the start, writes half the bytes and
// reports them, then the finish. hold leaves it running at half until it is canceled; fail ends it
// as a failure the engine reports on its own. Reports are synchronous,
// inside the navigation.

const fakeDownloadHost = "download.invalid"

type fakeDownloadRequest struct {
	url   string
	name  string
	bytes int64
	hold  bool
	fail  bool
}

// fakeDownload is a running download, kept from before its first report
// until whoever takes it reports its end: its total, for the report its
// cancel makes, and its file.
type fakeDownload struct {
	total int64
	file  string
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
func (p *fakeProfile) download(frame frameID, request fakeDownloadRequest) error {
	events := p.engine.events
	if events.DownloadStarted == nil || events.DownloadProgress == nil {
		return errors.New("browser: this fake engine was built without engine events and cannot download")
	}
	id := downloadID(fmt.Sprintf("fake-dl-%d", p.engine.seq.Add(1)))
	selfSaved := p.engine.saving == downloadsSavedByEngine
	file, name, destinationErr := filepath.Join(p.downloadDir, string(id)), request.name, error(nil)
	if p.engine.saving == downloadsStaged {
		if p.stage == nil {
			return errors.New("browser: this fake profile stages downloads and was given nowhere to stage them")
		}
		// Named before the start is reported, as WebKit asks for the
		// destination first.
		file, destinationErr = p.stage(id)
	}
	if selfSaved {
		file, destinationErr = fakeSaveInDownloads(request.name)
		if destinationErr == nil {
			name = filepath.Base(file)
		}
	}
	// discard removes the download's own file in the Downloads folder.
	discard := func() error {
		if !selfSaved || destinationErr != nil {
			return nil
		}
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("browser: remove the fake download's file: %w", err)
		}
		return nil
	}
	if events.DownloadStarted(downloadStart{Frame: frame, ID: id, URL: request.url, SuggestedName: name}) != downloadAdmitted {
		return discard()
	}
	fail := func(err error) error {
		events.DownloadProgress(downloadProgress{ID: id, State: downloadFailed, Error: err.Error()})
		return discard()
	}
	if destinationErr != nil {
		return fail(destinationErr)
	}
	half := request.bytes / 2
	if err := os.WriteFile(file, make([]byte, half), 0o600); err != nil {
		return fail(err)
	}
	// Cancellable from its first report on: a cancel made while the
	// progress is handled takes the download and reports its end, and the
	// download goes no further.
	p.mu.Lock()
	if p.held == nil {
		p.held = make(map[downloadID]fakeDownload)
	}
	p.held[id] = fakeDownload{total: request.bytes, file: file}
	p.mu.Unlock()
	events.DownloadProgress(downloadProgress{ID: id, Received: float64(half), Total: float64(request.bytes), State: downloadInProgress})
	if request.hold {
		return nil
	}
	if _, running := p.takeHeld(id); !running {
		return nil
	}
	if request.fail {
		if err := os.Remove(file); err != nil {
			return fmt.Errorf("browser: remove the failed fake download: %w", err)
		}
		events.DownloadProgress(downloadProgress{ID: id, Received: float64(half), Total: float64(request.bytes), State: downloadFailed, Error: "the server closed the connection"})
		return nil
	}
	if err := os.WriteFile(file, make([]byte, request.bytes), 0o600); err != nil {
		return fail(err)
	}
	p.reportFinish(id, request.bytes, file)
	return nil
}

// fakeSaveInDownloads creates the file of a download saved into the user's
// Downloads folder under the first free name, as WebView2 names one.
func fakeSaveInDownloads(suggested string) (string, error) {
	dir, err := userDownloadsDir()
	if err != nil {
		return "", err
	}
	name := safeArtifactName(suggested, "download")
	ext := filepath.Ext(name)
	for i := 0; i < maxDownloadNameCollisions; i++ {
		candidate := filepath.Join(dir, name)
		if i > 0 {
			candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), i, ext))
		}
		file, err := os.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return candidate, file.Close()
	}
	return "", fmt.Errorf("browser: no free name for %q", name)
}

// reportFinish reports a finished download whose file is complete, after
// the beforeFinishReport seam.
func (p *fakeProfile) reportFinish(id downloadID, total int64, file string) {
	if hook := p.engine.beforeFinishReport; hook != nil {
		hook(file)
	}
	p.engine.events.DownloadProgress(downloadProgress{ID: id, Received: float64(total), Total: float64(total), State: downloadCompleted, FilePath: file})
}

// CancelDownload ends a held download and reports it canceled, as an engine
// reports the cancel it was asked for, releasing its record first as the
// native engines do. In the Downloads folder it first removes the partial
// file, as WebView2 does there. Any other id has already been let go, and
// its terminal report is out or on the way.
func (p *fakeProfile) CancelDownload(id downloadID) error {
	held, ok := p.takeHeld(id)
	if !ok {
		return errDownloadEnded
	}
	if p.engine.saving == downloadsSavedByEngine {
		if err := os.Remove(held.file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("browser: remove the canceled fake download: %w", err)
		}
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
func (p *fakeProfile) finishHeld(id downloadID) error {
	held, ok := p.takeHeld(id)
	if !ok {
		return fmt.Errorf("browser: %s is not a held fake download", id)
	}
	if err := os.WriteFile(held.file, make([]byte, held.total), 0o600); err != nil {
		return err
	}
	if released := p.engine.downloadReleased; released != nil {
		released(id)
	}
	p.reportFinish(id, held.total, held.file)
	return nil
}

// takeHeld takes the running download id, once: whoever takes it (its
// cancel, its finish, or the download's own run past its first report)
// reports its end.
func (p *fakeProfile) takeHeld(id downloadID) (fakeDownload, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	held, ok := p.held[id]
	delete(p.held, id)
	return held, ok
}
