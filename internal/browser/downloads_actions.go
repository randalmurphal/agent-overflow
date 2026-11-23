package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/browser/turnorder"
)

// The pane's actions on a row of the thread's download list
// (downloads_thread.go): cancel, clear, reveal and open. Each resolves its
// row by its DownloadKey in the list as it stands.

// ErrDownloadUnavailable is the answer to an action on a download the
// thread's list no longer resolves: it was cleared, the thread's browser
// closed, or the app restarted since it was announced.
var ErrDownloadUnavailable = errors.New("browser: that download is no longer in this browser's list")

func (m *Manager) threadDownload(access Access, id DownloadKey) (threadDownload, error) {
	if err := access.End(); err != nil {
		return threadDownload{}, err
	}
	m.downloadsMu.Lock()
	defer m.downloadsMu.Unlock()
	if list := access.tb.downloads; list != nil {
		if item := list.find(DownloadKey(strings.TrimSpace(string(id)))); item != nil {
			return *item, nil
		}
	}
	return threadDownload{}, ErrDownloadUnavailable
}

// CancelThreadDownload is the pane's cancel for a running download. It only
// asks: the engine's terminal report settles the download, as a cancel when
// the cancel won (downloadProgress removes a partial file in AO's
// directory; an engine saving to the Downloads folder itself removes its
// own there) or as a
// finish when the download finished first, so a cancel racing a finish never
// deletes the finished file. The engine's refusal is the answer and the
// download keeps running and listed as such.
func (m *Manager) CancelThreadDownload(access Access, id DownloadKey) error {
	if err := m.bound(access); err != nil {
		return err
	}
	item, err := m.threadDownload(access, id)
	if err != nil {
		return err
	}
	p, scope := m.pageForDownload(item.engineID)
	if p == nil || scope == nil {
		// Its page is gone, and closing it settled the download.
		return nil
	}
	held := p.downloads.hold()
	entry := held.entry(item.engineID)
	if entry == nil || entry.endReceived() || entry.advance(rowCancelRequested) != nil {
		// Only a running row is canceled: a cancel already with the engine
		// stays its request, which only its own refusal withdraws, never a
		// later request's answer, and a finished file being published, an
		// ended download, or one whose end was received and is on its way
		// to the row, has its end.
		held.release()
		return nil
	}
	// A refusal of an earlier cancel no longer stands.
	entry.Error = ""
	held.release()
	err = scope.profile.CancelDownload(item.engineID)
	if err == nil {
		return nil
	}
	held = p.downloads.hold()
	if entry := held.entry(item.engineID); entry != nil && entry.phase == rowCancelRequested {
		// Withdrawn, unless the download moved on meanwhile: its finish is
		// being published, or it ended.
		if advanceErr := entry.advance(rowRunning); advanceErr != nil {
			log.Printf("browser: %v", advanceErr)
		}
	}
	held.release()
	if errors.Is(err, errDownloadEnded) {
		// The download finished first; its report is the answer.
		return nil
	}
	return fmt.Errorf("browser: cancel the download of %s: %w", item.Name, err)
}

// ClearThreadDownloads removes the thread's settled downloads from its list,
// shown or resolvable. Running ones stay, so they can still be canceled. No
// file is deleted: one in the user's Downloads folder is the user's, and an
// artifact goes with the rest after artifactMaxAge. A lifetime the
// thread's close detached is refused with ErrThreadClosed.
func (m *Manager) ClearThreadDownloads(access Access) error {
	if err := m.bound(access); err != nil {
		return err
	}
	if err := access.End(); err != nil {
		return err
	}
	m.downloadsMu.Lock()
	if list := access.tb.downloads; list != nil {
		list.clear(m.nextDownloadGenLocked())
	}
	m.downloadsMu.Unlock()
	m.emitThreadState(access.ThreadID())
	return nil
}

// finishedDownloadPath answers the file of one of the thread's finished
// downloads.
func (m *Manager) finishedDownloadPath(access Access, id DownloadKey) (threadDownload, error) {
	item, err := m.threadDownload(access, id)
	if err != nil {
		return threadDownload{}, err
	}
	if item.State != downloadStateCompleted || item.path == "" {
		return threadDownload{}, fmt.Errorf("browser: the download of %s did not finish", item.Name)
	}
	if item.file == nil {
		return threadDownload{}, fmt.Errorf("browser: %s: %s", item.Name, item.Error)
	}
	current, err := os.Stat(item.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && m.downloadsToArtifacts() {
			return threadDownload{}, fmt.Errorf("browser: %s is gone; downloads are removed after %d days", item.Name, int(artifactMaxAge.Hours()/24))
		}
		if errors.Is(err, os.ErrNotExist) {
			return threadDownload{}, fmt.Errorf("browser: %s is no longer at %s", item.Name, item.path)
		}
		return threadDownload{}, fmt.Errorf("browser: read the download %s: %w", item.Name, err)
	}
	// A name is reused once its file is moved away, so the path alone may
	// name a later download's file.
	if !sameDownloadedFile(item.file, current) {
		return threadDownload{}, fmt.Errorf("browser: %s was moved, changed or replaced since it downloaded", item.Name)
	}
	return item, nil
}

// sameDownloadedFile reports whether current is the file a download wrote,
// as recorded when it finished. Device and inode alone can match a later
// download that reuses a deleted file's inode; that one cannot also match
// the size and modification time, which a rename keeps.
func sameDownloadedFile(recorded, current os.FileInfo) bool {
	return recorded != nil && os.SameFile(recorded, current) &&
		recorded.Size() == current.Size() && recorded.ModTime().Equal(current.ModTime())
}

// RevealThreadDownload opens the OS file manager with a finished download
// selected, through the same hand-off as RevealPageFile.
func (m *Manager) RevealThreadDownload(ctx context.Context, access Access, id DownloadKey) error {
	if err := m.bound(access); err != nil {
		return err
	}
	item, err := m.finishedDownloadPath(access, id)
	if err != nil {
		return err
	}
	return m.revealFile(ctx, item.path)
}

// OpenThreadDownload shows a finished download in a new tab of the thread's
// browser, for a type a tab shows (downloadOpensInPage). A page may open
// its workspace's downloads (authorizeFile). With ctx's numbered selection
// passed (selection.go) it opens nothing.
func (m *Manager) OpenThreadDownload(ctx context.Context, access Access, id DownloadKey) (PageInfo, error) {
	if err := m.bound(access); err != nil {
		return PageInfo{}, err
	}
	turn, err := m.selectionTurn(ctx, access)
	if err != nil || turn.Standing() == turnorder.Passed {
		return PageInfo{}, err
	}
	defer turn.Release()
	item, err := m.finishedDownloadPath(access, id)
	if err != nil {
		return PageInfo{}, err
	}
	if !downloadOpensInPage(item.path) {
		return PageInfo{}, fmt.Errorf("browser: a browser tab does not show %s; use Show in folder", item.Name)
	}
	// The tab is made first, then the file opened in it only while it is
	// still the file the download wrote.
	info, err := m.openUserTab(ctx, access, turn, "", "", -1, nil)
	if errors.Is(err, errSelectionPassed) || (err == nil && info.ID == "") {
		return PageInfo{}, nil
	}
	if err != nil {
		return info, err
	}
	opened, err := m.openFile(ctx, access, item.path, item.file, OpenOptions{PageID: PageID(info.ID)})
	if err != nil {
		return info, err
	}
	return opened, nil
}

// downloadOpensInPage is whether every engine shows a file of this name in a
// tab rather than downloading it again: passive documents and images. Types
// that run script from a file:// address (HTML, SVG, XML) are left to the
// file manager.
func downloadOpensInPage(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".pdf", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".txt", ".json":
		return true
	}
	return false
}
