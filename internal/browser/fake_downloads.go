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
// reports them, then the finish. hold leaves it running at half until it
// is canceled; fail ends it as a failure the engine reports on its own. A
// cancel is reported with the bytes written by then. Reports are
// synchronous, inside the navigation.

const fakeDownloadHost = "download.invalid"

type fakeDownloadRequest struct {
	url   string
	name  string
	bytes int64
	hold  bool
	fail  bool
}

// fakeDownload is a download, kept from before its start is reported until
// its end report returns: its total and the bytes written of it, for the
// report its cancel makes, its file, and who reports that end. Nothing
// else forgets it, so a record a step expects is never missing.
type fakeDownload struct {
	total   int64
	written int64
	file    string
	phase   fakeDownloadPhase
}

// fakeDownloadPhase is who reports a fake download's end.
type fakeDownloadPhase int

const (
	// fakeRunning: its run (fakeProfile.download) still reports it, from
	// before its start on. A cancel, or the profile's disposal, now marks it
	// fakeCanceled, and the run reports the cancel once the report it is in
	// returns.
	fakeRunning fakeDownloadPhase = iota + 1
	// fakeCanceled: canceled during its run, which reports the end.
	fakeCanceled
	// fakeHeld: its run left it at half (hold); its cancel, finishHeld or
	// the profile's disposal reports the end.
	fakeHeld
	// fakeEnding: its end is decided and being reported. Its start's
	// EndReported answers true, and a cancel answers errDownloadEnded.
	fakeEnding
)

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
	start := downloadStart{Frame: frame, ID: id, URL: request.url, SuggestedName: name}
	start.EndReported = func() bool { return p.endReported(id) }
	// Cancellable from before its start is reported: a cancel made while
	// the start or a progress report is handled is taken, and this run
	// reports it as the download's one end after that report, with the
	// bytes written by then. The record goes once the run's end report
	// returns, unless the run left it held.
	p.track(id, fakeDownload{total: request.bytes, file: file, phase: fakeRunning})
	parked := false
	defer func() {
		if !parked {
			p.dropDownload(id)
		}
	}()
	if events.DownloadStarted(start) != downloadAdmitted {
		return discard()
	}
	if stop, err := p.stopIfCanceled(id); stop {
		return err
	}
	fail := func(err error) error {
		if own, takeErr := p.takeEnd(id); !own {
			return takeErr
		}
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
	if err := p.wrote(id, half); err != nil {
		return err
	}
	if stop, err := p.stopIfCanceled(id); stop {
		return err
	}
	events.DownloadProgress(downloadProgress{ID: id, Received: float64(half), Total: float64(request.bytes), State: downloadInProgress})
	if request.hold {
		held, err := p.park(id)
		parked = held
		return err
	}
	if own, err := p.takeEnd(id); !own {
		return err
	}
	if request.fail {
		if err := os.Remove(file); err != nil {
			return fmt.Errorf("browser: remove the failed fake download: %w", err)
		}
		events.DownloadProgress(downloadProgress{ID: id, Received: float64(half), Total: float64(request.bytes), State: downloadFailed, Error: "the server closed the connection"})
		return nil
	}
	if err := os.WriteFile(file, make([]byte, request.bytes), 0o600); err != nil {
		events.DownloadProgress(downloadProgress{ID: id, State: downloadFailed, Error: err.Error()})
		return discard()
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
	for i := 0; i < maxNameVariants; i++ {
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

// CancelDownload ends a held download and reports it canceled, as an
// engine reports the cancel it was asked for, its end taken first as the
// native engines release their record. One still in its run, from before
// its start is reported, is marked instead, and its run reports the cancel
// after the report it is in. One whose end is being reported, or was, has
// nothing left to cancel.
func (p *fakeProfile) CancelDownload(id downloadID) error {
	p.mu.Lock()
	d, ok := p.held[id]
	switch {
	case !ok || d.phase == fakeEnding:
		p.mu.Unlock()
		return errDownloadEnded
	case d.phase == fakeRunning:
		d.phase = fakeCanceled
		p.held[id] = d
		p.mu.Unlock()
		return nil
	case d.phase == fakeCanceled:
		p.mu.Unlock()
		return nil
	}
	d.phase = fakeEnding
	p.held[id] = d
	p.mu.Unlock()
	defer p.dropDownload(id)
	return p.reportCancel(id, d)
}

// reportCancel reports the cancel of download id, whose end, record d,
// its caller took. In the Downloads folder it first removes the partial
// file, as WebView2 does there.
func (p *fakeProfile) reportCancel(id downloadID, d fakeDownload) error {
	if p.engine.saving == downloadsSavedByEngine {
		if err := os.Remove(d.file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("browser: remove the canceled fake download: %w", err)
		}
	}
	if released := p.engine.downloadReleased; released != nil {
		released(id)
	}
	if p.engine.events.DownloadProgress != nil {
		p.engine.events.DownloadProgress(downloadProgress{ID: id, Received: float64(d.written), Total: float64(d.total), State: downloadCanceled})
	}
	return nil
}

// finishHeld finishes a held download the way the native engines do: the
// file is complete, the engine's record of it is released, and only then is
// the finish reported.
func (p *fakeProfile) finishHeld(id downloadID) error {
	p.mu.Lock()
	held, ok := p.held[id]
	ok = ok && held.phase == fakeHeld
	if ok {
		held.phase = fakeEnding
		p.held[id] = held
	}
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("browser: %s is not a held fake download", id)
	}
	defer p.dropDownload(id)
	if err := os.WriteFile(held.file, make([]byte, held.total), 0o600); err != nil {
		return err
	}
	if released := p.engine.downloadReleased; released != nil {
		released(id)
	}
	p.reportFinish(id, held.total, held.file)
	return nil
}

// disposeDownloads ends the profile's downloads at its disposal: one in its
// run is marked canceled, which its run reports with its own record, and
// one held is taken and its cancel reported here. Each record goes once its
// end report returns.
func (p *fakeProfile) disposeDownloads() error {
	p.mu.Lock()
	ended := make(map[downloadID]fakeDownload)
	for id, d := range p.held {
		switch d.phase {
		case fakeRunning:
			d.phase = fakeCanceled
		case fakeHeld:
			d.phase = fakeEnding
			ended[id] = d
		case fakeCanceled, fakeEnding:
			// Its run, or whoever took its end, reports it.
			continue
		}
		p.held[id] = d
	}
	p.mu.Unlock()
	var errs []error
	for id, d := range ended {
		errs = append(errs, p.reportCancel(id, d))
		p.dropDownload(id)
	}
	return errors.Join(errs...)
}

// track records download id, running, before its start is reported.
func (p *fakeProfile) track(id downloadID, d fakeDownload) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held == nil {
		p.held = make(map[downloadID]fakeDownload)
	}
	p.held[id] = d
}

// recordLocked is download id's record, which its run expects to be there
// until its end report returns. The caller holds p.mu.
func (p *fakeProfile) recordLocked(id downloadID) (fakeDownload, error) {
	d, ok := p.held[id]
	if !ok {
		return fakeDownload{}, fmt.Errorf("browser: the record of fake download %s went before its end was reported", id)
	}
	return d, nil
}

// wrote records that n bytes of download id are on disk.
func (p *fakeProfile) wrote(id downloadID, n int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, err := p.recordLocked(id)
	if err != nil {
		return err
	}
	d.written = n
	p.held[id] = d
	return nil
}

// stopIfCanceled answers whether the run of download id stops here: when
// it was canceled during the run, the run's end is that cancel, which it
// reports here.
func (p *fakeProfile) stopIfCanceled(id downloadID) (bool, error) {
	p.mu.Lock()
	d, err := p.recordLocked(id)
	if err != nil || d.phase != fakeCanceled {
		p.mu.Unlock()
		return err != nil, err
	}
	d.phase = fakeEnding
	p.held[id] = d
	p.mu.Unlock()
	return true, p.reportCancel(id, d)
}

// takeEnd takes download id's end for its run, which reports it as its own,
// and answers false when a cancel was taken first, which it reports here.
func (p *fakeProfile) takeEnd(id downloadID) (bool, error) {
	return p.leaveRun(id, fakeEnding)
}

// park leaves download id held at half, for its cancel or finishHeld, and
// answers false when a cancel was taken first, which it reports here.
func (p *fakeProfile) park(id downloadID) (bool, error) {
	return p.leaveRun(id, fakeHeld)
}

// leaveRun moves download id out of its run to next, or, canceled during
// the run, to its end, which it reports, answering false.
func (p *fakeProfile) leaveRun(id downloadID, next fakeDownloadPhase) (bool, error) {
	p.mu.Lock()
	d, err := p.recordLocked(id)
	if err != nil {
		p.mu.Unlock()
		return false, err
	}
	canceled := d.phase == fakeCanceled
	d.phase = next
	if canceled {
		d.phase = fakeEnding
	}
	p.held[id] = d
	p.mu.Unlock()
	if canceled {
		return false, p.reportCancel(id, d)
	}
	return true, nil
}

// endReported is the EndReported of download id's start: whether its end
// is being reported.
func (p *fakeProfile) endReported(id downloadID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, ok := p.held[id]
	return ok && d.phase == fakeEnding
}

// dropDownload drops download id's record once its end report returned.
func (p *fakeProfile) dropDownload(id downloadID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.held, id)
}
