package browser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// The states a DownloadInfo, and the ThreadDownload behind it, moves through.
const (
	downloadStateRunning   = "in_progress"
	downloadStateCompleted = "completed"
	downloadStateFailed    = "failed"
	downloadStateCanceled  = "canceled"
)

// maxPageDownloads bounds one page's download log.
const maxPageDownloads = 100

// downloadStarted is the quota decision. The engine reports that a download
// began; whether it is allowed to consume workspace and artifact bytes is
// policy, and so is the sanitized name it will eventually be renamed to. It
// answers false, and the engine cancels the download, when the download is
// over quota or no managed page owns the frame it began in. The quota is the
// page's workspace's (workspaceAuthority), whichever jar the page is in.
func (m *Manager) downloadStarted(event downloadStart) bool {
	p, _ := m.pageForFrame(event.Frame)
	if p == nil {
		return false
	}
	authority := p.authority
	source := p.cachedInfo().URL
	if source == "" || source == "about:blank" {
		source = event.URL
	}
	if m.downloadAdmitting != nil {
		m.downloadAdmitting()
	}
	p.downloadMu.Lock()
	if p.downloadsEnded {
		// The page left after it was looked up; nothing would settle this.
		p.downloadMu.Unlock()
		return false
	}
	p.downloadSeq++
	entry := DownloadInfo{
		Sequence: p.downloadSeq, ID: event.ID, URL: event.URL,
		SuggestedName: safeArtifactName(event.SuggestedName, "download"),
		State:         downloadStateRunning, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), reservedBytes: maxDownloadBytes,
		key: fmt.Sprintf("d%d", m.downloadKeys.Add(1)),
	}
	scopeReserved := authority != nil && authority.downloadBytes.Add(maxDownloadBytes) <= maxWorkspaceDownloadBytes
	globalReserved := scopeReserved && m.reserveArtifacts(maxDownloadBytes)
	if !scopeReserved || !globalReserved {
		if scopeReserved {
			authority.downloadBytes.Add(-maxDownloadBytes)
		} else if authority != nil && authority.downloadBytes.Load() > maxWorkspaceDownloadBytes {
			authority.downloadBytes.Add(-maxDownloadBytes)
		}
		entry.State, entry.Error, entry.reservedBytes = downloadStateCanceled, fmt.Sprintf("workspace downloads exceed %d bytes", maxWorkspaceDownloadBytes), 0
		if scopeReserved && !globalReserved {
			entry.Error = fmt.Sprintf("browser artifacts exceed %d bytes", maxArtifactRootBytes)
		}
	}
	p.downloads = append(p.downloads, entry)
	p.trimDownloadsLocked()
	note := m.noteDownloadLocked(p, &p.downloads[len(p.downloads)-1], source)
	p.signalDownloadLocked()
	p.downloadMu.Unlock()
	m.publishDownloads(note)
	return entry.State != downloadStateCanceled
}

// trimDownloadsLocked keeps the page's log within maxPageDownloads by
// dropping its oldest settled entries. A running one stays: its progress,
// its reservation and its cancel all need it, and the quotas bound how many
// can run. The caller holds p.downloadMu.
func (p *managedPage) trimDownloadsLocked() {
	excess := len(p.downloads) - maxPageDownloads
	if excess <= 0 {
		return
	}
	kept := make([]DownloadInfo, 0, maxPageDownloads)
	for _, entry := range p.downloads {
		if excess > 0 && entry.State != downloadStateRunning {
			excess--
			continue
		}
		kept = append(kept, entry)
	}
	p.downloads = kept
}

// downloadLocked answers the page's entry for an engine download id, nil for
// none. The pointer is valid until p.downloadMu is released. The caller holds
// p.downloadMu.
func (p *managedPage) downloadLocked(id string) *DownloadInfo {
	for i := len(p.downloads) - 1; i >= 0; i-- {
		if p.downloads[i].ID == id {
			return &p.downloads[i]
		}
	}
	return nil
}

// releaseDownloadLocked returns a download's whole reservation, for one that
// ends with no file kept. The caller holds p.downloadMu.
func (m *Manager) releaseDownloadLocked(p *managedPage, entry *DownloadInfo) {
	if p.authority != nil && entry.reservedBytes > 0 {
		p.authority.downloadBytes.Add(-entry.reservedBytes)
		m.settleArtifactReservation(entry.reservedBytes, 0)
		entry.reservedBytes = 0
	}
}

// removePartialDownload removes the handle-named file an engine was
// streaming a download into, once AO stopped the download.
func removePartialDownload(scope *jarScope, id string) {
	if err := os.Remove(filepath.Join(scope.downloadDir, filepath.Base(id))); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("browser: remove the partial download %s: %v", id, err)
	}
}

func (m *Manager) downloadProgress(event downloadProgress) {
	p, scope := m.pageForDownload(event.ID)
	if p == nil {
		return
	}
	p.downloadMu.Lock()
	entry := p.downloadLocked(event.ID)
	// A settled download takes no further report: AO stopped it (a quota,
	// its size, the user, its page closing) or the engine already ended it.
	if entry == nil || entry.State != downloadStateRunning {
		p.downloadMu.Unlock()
		return
	}
	entry.Bytes = int64(event.Received)
	if event.Total > 0 {
		entry.TotalBytes = int64(event.Total)
	}
	if entry.Bytes > maxDownloadBytes && event.State == downloadInProgress {
		entry.State = downloadStateCanceled
		entry.Error = fmt.Sprintf("download exceeds %d bytes", maxDownloadBytes)
		m.releaseDownloadLocked(p, entry)
		note := m.noteDownloadLocked(p, entry, "")
		p.signalDownloadLocked()
		p.downloadMu.Unlock()
		m.publishDownloads(note)
		if scope != nil {
			go func() {
				if err := scope.profile.CancelDownload(event.ID); err != nil {
					log.Printf("browser: cancel download %s over its size cap: %v", event.ID, err)
				}
				removePartialDownload(scope, event.ID)
			}()
		}
		return
	}
	removePartial := false
	switch event.State {
	case downloadCompleted:
		entry.State = downloadStateCompleted
		if p.authority != nil && entry.reservedBytes > 0 {
			p.authority.downloadBytes.Add(-entry.reservedBytes + entry.Bytes)
			m.settleArtifactReservation(entry.reservedBytes, entry.Bytes)
			entry.reservedBytes = 0
		}
		if scope != nil && p.authority != nil {
			// The engine writes into its jar's directory; the finished file
			// moves to the page's workspace's.
			source := event.FilePath
			if source == "" {
				source = filepath.Join(scope.downloadDir, filepath.Base(event.ID))
			}
			if !pathInside(scope.downloadDir, source) {
				entry.State, entry.Error = downloadStateFailed, "browser returned a download path outside its artifact directory"
				break
			}
			destination, renameErr := uniqueArtifactPath(p.authority.downloadDir, entry.SuggestedName)
			if renameErr == nil {
				renameErr = os.Rename(source, destination)
			}
			var file os.FileInfo
			if renameErr == nil {
				file, renameErr = os.Stat(destination)
			}
			if renameErr != nil {
				entry.State, entry.Error = downloadStateFailed, renameErr.Error()
			} else {
				entry.Path, entry.file = destination, file
			}
		}
	case downloadCanceled:
		// An engine reports the cancel AO asked for and its own failures (a
		// dropped connection, a server error) the same way; only the first
		// is a cancel.
		if entry.cancelRequested {
			entry.State = downloadStateCanceled
			removePartial = true
		} else {
			entry.State, entry.Error = downloadStateFailed, event.Error
			if entry.Error == "" {
				entry.Error = "the download stopped before it finished"
			}
		}
		m.releaseDownloadLocked(p, entry)
	}
	note := m.noteDownloadLocked(p, entry, "")
	p.signalDownloadLocked()
	p.downloadMu.Unlock()
	if removePartial && scope != nil {
		// The engine took the user's cancel; what it wrote is a partial file.
		removePartialDownload(scope, event.ID)
	}
	m.publishDownloads(note)
}

func pathInside(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// pageForFrame walks the registry the Manager owns and asks each driver whether
// the frame is one of its own. Ownership stays here; frame bookkeeping does not.
func (m *Manager) pageForFrame(frame string) (*managedPage, *jarScope) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.driver.OwnsFrame(frame) {
				return p, scope
			}
		}
	}
	return nil, nil
}

func (m *Manager) pageForDownload(id string) (*managedPage, *jarScope) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			p.downloadMu.Lock()
			found := p.downloadLocked(id) != nil
			p.downloadMu.Unlock()
			if found {
				return p, scope
			}
		}
	}
	return nil, nil
}

func (p *managedPage) signalDownloadLocked() {
	close(p.downloadWait)
	p.downloadWait = make(chan struct{})
}

// endedDownloads are the downloads a page left running when it stopped being
// the Manager's (pageLeftLocked): already settled as canceled with their
// reservations released, still to be published and stopped in the engine.
type endedDownloads struct {
	scope *jarScope
	notes []downloadNote
	ids   []string
}

// pageLeftLocked takes p out of scope's live pages. Every way a page stops
// being the Manager's goes through it (a close, a jar switch, a suspend, the
// engine's own close, the browser's close), so none can leave the page's
// downloads running with nothing to settle them: they settle here as
// canceled with reason, and the page admits none after (downloadStarted).
// The caller holds m.mu, publishes the answer once it is released
// (publishDownloads), and stops it (stopEndedDownloads) before the profile
// is disposed.
func (m *Manager) pageLeftLocked(scope *jarScope, p *managedPage, reason string) endedDownloads {
	delete(scope.pages, p.id)
	ended := endedDownloads{scope: scope}
	p.downloadMu.Lock()
	p.downloadsEnded = true
	for i := range p.downloads {
		entry := &p.downloads[i]
		if entry.State != downloadStateRunning {
			continue
		}
		entry.State, entry.Error = downloadStateCanceled, reason
		m.releaseDownloadLocked(p, entry)
		ended.notes = append(ended.notes, m.noteDownloadLocked(p, entry, ""))
		ended.ids = append(ended.ids, entry.ID)
	}
	if len(ended.ids) > 0 {
		p.signalDownloadLocked()
	}
	p.downloadMu.Unlock()
	return ended
}

// stopEndedDownloads asks the engine to stop the downloads a page left
// running and removes what they wrote: their entries already say canceled,
// so a file that finished meanwhile goes too.
func (m *Manager) stopEndedDownloads(ended endedDownloads) {
	for _, id := range ended.ids {
		if err := ended.scope.profile.CancelDownload(id); err != nil && !errors.Is(err, errDownloadEnded) {
			log.Printf("browser: cancel download %s of a page that went: %v", id, err)
		}
		removePartialDownload(ended.scope, id)
	}
}

func (m *Manager) Downloads(ctx context.Context, access Access, opts DownloadOptions) (any, error) {
	p, _, err := m.lookupOrSelectPage(ctx, access, opts.PageID)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.TrimSpace(opts.Action)) {
	case "", "list":
		p.downloadMu.Lock()
		out := append([]DownloadInfo(nil), p.downloads...)
		p.downloadMu.Unlock()
		return out, nil
	case "wait":
		timeout, timeoutErr := boundedTimeout(opts.TimeoutMS)
		if timeoutErr != nil {
			return nil, timeoutErr
		}
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return waitDownloadPage(waitCtx, p, opts.After)
	default:
		return nil, fmt.Errorf("browser: download action must be list or wait")
	}
}

func waitDownloadPage(ctx context.Context, p *managedPage, after uint64) (DownloadInfo, error) {
	for {
		p.downloadMu.Lock()
		for _, download := range p.downloads {
			if download.Sequence > after && download.State != downloadStateRunning {
				p.downloadMu.Unlock()
				return download, nil
			}
		}
		signal := p.downloadWait
		p.downloadMu.Unlock()
		select {
		case <-signal:
		case <-ctx.Done():
			return DownloadInfo{}, fmt.Errorf("browser: wait for download: %w", ctx.Err())
		}
	}
}

func safeArtifactName(name, fallback string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimRight(name, ". ")
	if name == "" || name == "." {
		name = fallback
	}
	base := strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))
	reserved := base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9')
	if reserved {
		name = "_" + name
	}
	if len(name) > 180 {
		for len(name) > 180 {
			_, size := utf8.DecodeLastRuneInString(name)
			name = name[:len(name)-size]
		}
	}
	return name
}

func uniqueArtifactPath(dir, name string) (string, error) {
	for i := 0; i < 1000; i++ {
		candidate := filepath.Join(dir, name)
		if i > 0 {
			ext := filepath.Ext(name)
			candidate = filepath.Join(dir, fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), i, ext))
		}
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("browser: too many files named %q", name)
}
