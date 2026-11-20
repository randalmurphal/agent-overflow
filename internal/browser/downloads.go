package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// The states a DownloadInfo, and the ThreadDownload behind it, moves through.
const (
	downloadStateRunning   = "in_progress"
	downloadStateCompleted = "completed"
	downloadStateFailed    = "failed"
	downloadStateCanceled  = "canceled"
)

// maxPageDownloads bounds the settled entries of one page's download log
// (pageDownloadLog).
const maxPageDownloads = 100

// downloadsToArtifacts is whether this Manager's finished downloads stay
// AO's (downloadsAsArtifacts), which quotas, a per-download cap and
// artifact pruning bound. Every other engine's end up in the user's
// Downloads folder, where AO bounds, moves and deletes nothing it did not
// put there.
func (m *Manager) downloadsToArtifacts() bool {
	return m.engine.DownloadSaving() == downloadsAsArtifacts
}

// downloadsStagedHere is whether this Manager's engine writes downloads into
// the staging directory in the user's Downloads folder (downloadsStaged).
func (m *Manager) downloadsStagedHere() bool {
	return m.engine.DownloadSaving() == downloadsStaged
}

// stageDownload is profileOptions.StageDownload: the staged file the
// engine writes download id into (stageDownload in download_staging.go),
// kept until the download settles (releaseStagedDownload).
func (m *Manager) stageDownload(id downloadID) (string, error) {
	path, err := stageDownload()
	if err != nil {
		return "", err
	}
	m.stagedMu.Lock()
	if m.staged == nil {
		m.staged = make(map[downloadID]string)
	}
	if previous := m.staged[id]; previous != "" {
		// An engine id is not reused while its download runs; one that is
		// takes the new file, and the old one goes.
		defer releaseStaged(previous)
	}
	m.staged[id] = path
	m.stagedMu.Unlock()
	return path, nil
}

// stagedPath answers the staged file of download id, "" for none.
func (m *Manager) stagedPath(id downloadID) string {
	m.stagedMu.Lock()
	defer m.stagedMu.Unlock()
	return m.staged[id]
}

// releaseStagedDownload ends download id's staged file: it goes if it is
// still there, and the staging directory with it once empty
// (releaseStaged).
func (m *Manager) releaseStagedDownload(id downloadID) {
	m.stagedMu.Lock()
	path := m.staged[id]
	delete(m.staged, id)
	m.stagedMu.Unlock()
	if path != "" {
		releaseStaged(path)
	}
}

// pageForFrame walks the registry the Manager owns and asks each driver whether
// the frame is one of its own. Ownership stays here; frame bookkeeping does not.
func (m *Manager) pageForFrame(frame frameID) (*managedPage, *jarScope) {
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

// pageForDownload answers the listed page download id runs in, with its
// jar, or the page that left with it waiting for its end, with no jar.
func (m *Manager) pageForDownload(id downloadID) (*managedPage, *jarScope) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.downloads.has(id) {
				return p, scope
			}
		}
	}
	return m.endPending[id], nil
}

// endDelivered forgets that p, a page that left, waits for the end of
// download id, once its row no longer does; a page waiting for none is
// left as it is.
func (m *Manager) endDelivered(p *managedPage, id downloadID) {
	held := p.downloads.hold()
	entry := held.entry(id)
	waits := entry != nil && !entry.phase.ended()
	held.release()
	if waits {
		return
	}
	m.mu.Lock()
	if m.endPending[id] == p {
		delete(m.endPending, id)
	}
	m.mu.Unlock()
}

// endedDownloads are the downloads a page left running when it stopped being
// the Manager's (pageLeftLocked): already settled as canceled, still to be
// published, stopped in the engine and discarded.
type endedDownloads struct {
	scope    *jarScope
	notes    []downloadNote
	discards []downloadDiscard
}

// receivingDriver is a page driver whose engine receives a download's
// reports before it processes them (the hosted engine's report lanes), so
// a download it received may not be admitted yet when the page leaves.
type receivingDriver interface {
	// TakeReceivedDownloads answers the starts of the page's downloads its
	// engine received and has not had admitted, but for those listed says
	// the page lists, oldest first, and hands each to the caller: the
	// engine applies its later reports to the row the caller lists for it.
	TakeReceivedDownloads(listed func(downloadID) bool) []downloadStart
}

// pageLeftLocked takes p out of scope's live pages. Every way a page stops
// being the Manager's goes through it (a close, a jar switch, a suspend, the
// engine's own close, the browser's close), so none can leave the page's
// downloads running with nothing to settle them: a download its engine
// received and has not had admitted is listed first (receivingDriver), and
// they settle here as canceled with reason, but for one whose end its
// engine received and will deliver (downloadStart.EndReported), which that
// end settles, and the page admits none after (downloadStarted).
// The caller holds m.mu, publishes the answer once it is released
// (publishDownloads), and stops it (stopEndedDownloads) before the profile
// is disposed.
func (m *Manager) pageLeftLocked(scope *jarScope, p *managedPage, reason string) endedDownloads {
	delete(scope.pages, p.id)
	if p.driver != nil {
		// A popup it opened is not adopted (awaitOpener), and its own
		// admission, if it was a popup, keeps its chain only while a popup
		// of it is live.
		m.unlistedLocked().put(p.driver.Handle(), struct{}{})
		m.admissions.Leave(p.driver.Handle())
	}
	ended := endedDownloads{scope: scope}
	held := p.downloads.hold()
	held.end()
	if receiving, ok := p.driver.(receivingDriver); ok {
		listed := func(id downloadID) bool { return held.entry(id) != nil }
		for _, start := range receiving.TakeReceivedDownloads(listed) {
			start = boundedStart(start)
			entry := m.downloadEntry(start)
			entry.source = m.downloadFileOf(scope, start.ID)
			ended.notes = append(ended.notes, m.noteDownloadStartLocked(p, held.add(entry), downloadSource(p, start)))
		}
	}
	for _, entry := range held.running() {
		// One being published settles when its file is in place
		// (publishFinishedDownload).
		if !entry.takesReports() {
			continue
		}
		if entry.endReported != nil && entry.endReported() {
			to := rowEndPending
			if entry.phase == rowCancelRequested {
				to = rowEndPendingCancelRequested
			}
			if err := entry.advance(to); err != nil {
				log.Printf("browser: %v", err)
				continue
			}
			if m.endPending == nil {
				m.endPending = make(map[downloadID]*managedPage)
			}
			m.endPending[downloadID(entry.ID)] = p
			continue
		}
		if err := entry.settle(downloadStateCanceled); err != nil {
			log.Printf("browser: %v", err)
			continue
		}
		entry.Error = reason
		ended.notes = append(ended.notes, m.noteDownloadLocked(p, entry))
		ended.discards = append(ended.discards, entry.discard())
	}
	held.release()
	return ended
}

// stopEndedDownloads asks the engine to stop the downloads a page left
// running, then discards each (finishDiscard). On an engine writing into
// AO's directory that removes what they wrote: their entries already say
// canceled, so a file that finished meanwhile goes too. An engine saving
// to the user's Downloads folder itself removes its own partial file on
// the cancel, and a file that finished first stays there.
func (m *Manager) stopEndedDownloads(ended endedDownloads) {
	for _, discard := range ended.discards {
		if err := ended.scope.profile.CancelDownload(discard.id); err != nil && !errors.Is(err, errDownloadEnded) {
			log.Printf("browser: cancel download %s of a page that went: %v", discard.id, err)
		}
		m.finishDiscard(discard)
	}
}

func (m *Manager) Downloads(ctx context.Context, access Access, opts DownloadOptions) (any, error) {
	if err := access.bound(); err != nil {
		return nil, err
	}
	p, _, err := m.lookupOrSelectPage(ctx, access, opts.PageID)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.TrimSpace(opts.Action)) {
	case "", "list":
		return p.downloads.list(), nil
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
	return p.downloads.settledAfter(ctx, after)
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

// artifactNames answers an artifact's name and its "-1", "-2" variants, by
// index.
func artifactNames(name string) func(i int) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	return func(i int) string {
		if i == 0 {
			return name
		}
		return fmt.Sprintf("%s-%d%s", stem, i, ext)
	}
}

// uniqueArtifactPath answers the first variant of name not in dir. A caller
// creates it exclusively; one that moves a file there uses placeNoReplace.
func uniqueArtifactPath(dir, name string) (string, error) {
	names := artifactNames(name)
	for i := 0; i < 1000; i++ {
		candidate := filepath.Join(dir, names(i))
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("browser: too many files named %q", name)
}
