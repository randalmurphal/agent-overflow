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

	"agent-overflow/internal/browser/ids"
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

// downloadOwnerLocked answers, with its jar, the first page that can own
// a download and that owns answers true for: a listed page, or a page
// being created or restored, which owns what its load downloads from its
// attach on, its listing keeping it and its abandonment settling it
// (abandonPage). A popup being adopted lists its downloads at its fate
// (popup_parked.go). A download's start (pageForFrame) and its later
// reports (pageForDownload) find their page through it alone, so whatever
// page admits a start receives what follows it. Ownership stays here;
// frame bookkeeping does not. The caller holds m.mu.
func (m *Manager) downloadOwnerLocked(owns func(*managedPage) bool) (*managedPage, *jarScope) {
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if owns(p) {
				return p, scope
			}
		}
		for p := range scope.creating {
			if p.driver != nil && !m.adoptingLocked(p) && owns(p) {
				return p, scope
			}
		}
	}
	return nil, nil
}

// pageForFrame answers the page whose driver owns frame
// (downloadOwnerLocked).
func (m *Manager) pageForFrame(frame frameID) (*managedPage, *jarScope) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.downloadOwnerLocked(func(p *managedPage) bool { return p.driver.OwnsFrame(frame) })
}

// pageForDownload answers the page whose log lists download id, with its
// jar (downloadOwnerLocked), or the page that left with it waiting for its
// end, with no jar.
func (m *Manager) pageForDownload(id downloadID) (*managedPage, *jarScope) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, scope := m.downloadOwnerLocked(func(p *managedPage) bool { return p.downloads.has(id) }); p != nil {
		return p, scope
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
	// TakeReceivedDownloads answers the starts of the downloads its engine
	// received and has not had admitted, but for those listed says the page
	// lists, oldest first, and hands each to the caller: the engine applies
	// its later reports to the row the caller lists for it. They are the
	// page's own and those of popups, the popups never listed whose
	// downloads the page's leave carries (popupCarrier).
	TakeReceivedDownloads(listed func(downloadID) bool, popups []ids.PageHandle) []downloadStart
}

// pageLeftLocked takes p out of scope's live pages. Every way a page stops
// being the Manager's goes through it (a close, a jar switch, a suspend, the
// engine's own close, the browser's close), so none can leave the page's
// downloads running with nothing to settle them (downloadsLeftLocked).
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
	return m.downloadsLeftLocked(scope, p, reason)
}

// downloadsLeftLocked ends p's download log as p leaves scope, listed or
// not: it carries the downloads of each ended popup it is the nearest
// listed opener of (carryEndedPopupsLocked), a download its engine
// received and has not had admitted, its own or such a popup's, is listed
// first (receivingDriver), and a carried popup's parked downloads are
// listed at that popup's fate as this leave would have (popupCarrier);
// every row then running settles as the leave settles it (departLocked),
// and the page admits none after (downloadStarted). The caller holds m.mu.
func (m *Manager) downloadsLeftLocked(scope *jarScope, p *managedPage, reason string) endedDownloads {
	ended := endedDownloads{scope: scope}
	held := p.downloads.hold()
	held.end()
	var carried []ids.PageHandle
	if p.driver != nil {
		carried = m.carryEndedPopupsLocked(p, reason)
	}
	if receiving, ok := p.driver.(receivingDriver); ok {
		listed := func(id downloadID) bool { return held.entry(id) != nil }
		for _, start := range receiving.TakeReceivedDownloads(listed, carried) {
			start = boundedStart(start)
			entry := m.downloadEntry(start)
			entry.source = m.downloadFileOf(scope, start.ID)
			ended.notes = append(ended.notes, m.noteDownloadStartLocked(p, held.add(entry), downloadSource(p, start)))
		}
	}
	for _, entry := range held.running() {
		if note, discard, canceled := m.departLocked(p, entry, reason); canceled {
			ended.notes = append(ended.notes, note)
			ended.discards = append(ended.discards, discard)
		}
	}
	held.release()
	return ended
}

// departLocked settles entry, a running row of p, as p leaves for reason:
// one whose end its engine received and will deliver
// (downloadStart.EndReported) waits for that end, which publishes a staged
// finish from its kept staged file, and any other is
// canceled with reason and the newest counts its engine received
// (downloadStart.NewestCounts). It answers the note and the discard of a
// row it canceled. One being published settles when its file is in place
// (publishFinishedDownload). The caller holds m.mu and p's download log.
func (m *Manager) departLocked(p *managedPage, entry *DownloadInfo, reason string) (note downloadNote, discard downloadDiscard, canceled bool) {
	if !entry.takesReports() {
		return downloadNote{}, downloadDiscard{}, false
	}
	if entry.endReported != nil && entry.endReported() {
		to := rowEndPending
		if entry.phase == rowCancelRequested {
			to = rowEndPendingCancelRequested
		}
		if err := entry.advance(to); err != nil {
			log.Printf("browser: %v", err)
			return downloadNote{}, downloadDiscard{}, false
		}
		if m.endPending == nil {
			m.endPending = make(map[downloadID]*managedPage)
		}
		m.endPending[downloadID(entry.ID)] = p
		return downloadNote{}, downloadDiscard{}, false
	}
	if err := entry.settle(downloadStateCanceled); err != nil {
		log.Printf("browser: %v", err)
		return downloadNote{}, downloadDiscard{}, false
	}
	entry.Error = reason
	if entry.newestCounts != nil {
		received, total := entry.newestCounts()
		entry.Bytes = received
		if total > 0 {
			entry.TotalBytes = total
		}
	}
	return m.noteDownloadLocked(p, entry), entry.discard(), true
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
	if err := m.bound(access); err != nil {
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
		return p.downloads.settledAfter(waitCtx, opts.After)
	default:
		return nil, fmt.Errorf("browser: download action must be list or wait")
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
	for i := 0; i < maxNameVariants; i++ {
		candidate := filepath.Join(dir, names(i))
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("browser: too many files named %q", name)
}
