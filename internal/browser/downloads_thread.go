package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A thread's downloads as its browser pane lists them. Each page keeps its
// own download log for browser_downloads (downloads.go); this is the
// thread's view of the same downloads, which outlives the page a download
// began in, so a file stays findable after its tab closes. Every change to a
// page's entry is noted here under the page's downloadMu (noteDownloadLocked)
// and published after it (publishDownloads): a start, a finish, a failure or
// a cancel pushes the thread's state at once, while byte progress is
// coalesced to one push per downloadPushInterval per thread. A download that
// settles other than by the user's cancel is announced once, as a Kind
// "download" event. The list is in memory only and goes with the thread
// (CloseThread); clearing it never deletes a file.

const (
	// maxThreadDownloads bounds the thread's list. A running download is
	// never dropped to make room (trim), and the quotas bound how many run.
	maxThreadDownloads = 20
	// downloadPushInterval is the most often byte progress pushes a thread's
	// state.
	downloadPushInterval = time.Second
)

// ThreadDownload is one download in the thread's browser pane. ID is AO's
// own key for it. Source is the address of the page it began on, or the
// download's own address when that page had none. State is a DownloadInfo
// state. TotalBytes is zero when the engine does not know the size.
// Openable is whether a browser tab can show the finished file
// (downloadOpensInPage).
type ThreadDownload struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Source        string `json:"source,omitempty"`
	State         string `json:"state"`
	ReceivedBytes int64  `json:"receivedBytes"`
	TotalBytes    int64  `json:"totalBytes,omitempty"`
	Error         string `json:"error,omitempty"`
	Openable      bool   `json:"openable,omitempty"`
}

type threadDownload struct {
	ThreadDownload
	// engineID is the engine's id for the download; workspace the canonical
	// workspace of the page it began in, whose pages may open its file
	// (listedDownloadAllows); path the finished file and file what it was
	// when the download finished, nil for a file AO does not open.
	engineID  string
	workspace string
	path      string
	file      os.FileInfo
}

type threadDownloadList struct {
	// items is newest first.
	items []*threadDownload
	// push is the pending coalesced state push, nil for none.
	push *time.Timer
}

func (l *threadDownloadList) find(id string) *threadDownload {
	for _, item := range l.items {
		if item.ID == id {
			return item
		}
	}
	return nil
}

// trim drops the oldest settled items beyond maxThreadDownloads.
func (l *threadDownloadList) trim() {
	for i := len(l.items) - 1; i >= 0 && len(l.items) > maxThreadDownloads; i-- {
		if l.items[i].State != downloadStateRunning {
			l.items = append(l.items[:i], l.items[i+1:]...)
		}
	}
}

// downloadNote is what one change to a download publishes.
type downloadNote struct {
	threadID string
	// progress is a change to the bytes alone, which a coalesced push carries.
	progress bool
	// announce is the download that just settled, to announce once; nil for
	// none.
	announce *ThreadDownload
}

// noteDownloadLocked copies a page's entry into its thread's list, adding
// the item for a download that just started (source is then its Source).
// The caller holds p.downloadMu; m.downloadsMu is taken inside it.
func (m *Manager) noteDownloadLocked(p *managedPage, entry *DownloadInfo, source string) downloadNote {
	m.downloadsMu.Lock()
	defer m.downloadsMu.Unlock()
	if m.threadDownloads == nil {
		m.threadDownloads = make(map[string]*threadDownloadList)
	}
	list := m.threadDownloads[p.owner]
	if list == nil {
		list = &threadDownloadList{}
		m.threadDownloads[p.owner] = list
	}
	item := list.find(entry.key)
	started := item == nil
	if started {
		if source == "" {
			// A later change to an item the user cleared or the bound
			// dropped, which only a settled item can be: nothing to show.
			return downloadNote{}
		}
		item = &threadDownload{ThreadDownload: ThreadDownload{ID: entry.key, Source: source}, engineID: entry.ID, workspace: p.access.Workspace}
		list.items = append([]*threadDownload{item}, list.items...)
		list.trim()
	}
	wasRunning := started || item.State == downloadStateRunning
	item.Name = entry.SuggestedName
	if entry.Path != "" {
		item.path, item.file = entry.Path, entry.file
		item.Name = filepath.Base(entry.Path)
	}
	item.State, item.ReceivedBytes, item.TotalBytes, item.Error = entry.State, entry.Bytes, entry.TotalBytes, entry.Error
	item.Openable = item.State == downloadStateCompleted && item.file != nil && downloadOpensInPage(item.Name)
	note := downloadNote{threadID: p.owner}
	// The user's own cancel is not announced; a finish that beat it is.
	userCanceled := item.State == downloadStateCanceled && entry.cancelRequested
	switch {
	case !started && item.State == downloadStateRunning:
		note.progress = true
	case item.State != downloadStateRunning && wasRunning && !userCanceled:
		announced := item.ThreadDownload
		note.announce = &announced
	}
	return note
}

// publishDownloads pushes the thread's state for notes of one thread: at
// once for any change but progress, otherwise through the thread's
// coalesced push. Then each settled download is announced, after the state
// that lists it. Call it holding no lock.
func (m *Manager) publishDownloads(notes ...downloadNote) {
	threadID, now := "", false
	for _, note := range notes {
		if note.threadID != "" {
			threadID = note.threadID
			now = now || !note.progress
		}
	}
	if threadID == "" {
		return
	}
	if !now {
		m.scheduleDownloadPush(threadID)
		return
	}
	m.downloadsMu.Lock()
	if list := m.threadDownloads[threadID]; list != nil && list.push != nil {
		list.push.Stop()
		list.push = nil
	}
	m.downloadsMu.Unlock()
	m.emitThreadState(threadID)
	for _, note := range notes {
		if note.announce != nil {
			m.emit(CompanionEvent{Kind: "download", ThreadID: threadID, Download: note.announce})
		}
	}
}

// scheduleDownloadPush starts the thread's coalesced state push unless one
// is pending.
func (m *Manager) scheduleDownloadPush(threadID string) {
	m.downloadsMu.Lock()
	defer m.downloadsMu.Unlock()
	list := m.threadDownloads[threadID]
	if list == nil || list.push != nil {
		return
	}
	interval := m.downloadPushInterval
	if interval <= 0 {
		interval = downloadPushInterval
	}
	var timer *time.Timer
	// Assigned under downloadsMu, which the callback takes first, so it
	// always sees its own timer.
	timer = time.AfterFunc(interval, func() {
		m.downloadsMu.Lock()
		if list.push == timer {
			list.push = nil
		}
		m.downloadsMu.Unlock()
		m.emitThreadState(threadID)
	})
	list.push = timer
}

// threadDownloadsLocked answers the thread's list for its state, newest
// first. The caller holds m.mu; m.downloadsMu is taken inside it.
func (m *Manager) threadDownloadsLocked(threadID string) []ThreadDownload {
	m.downloadsMu.Lock()
	defer m.downloadsMu.Unlock()
	list := m.threadDownloads[threadID]
	if list == nil || len(list.items) == 0 {
		return nil
	}
	out := make([]ThreadDownload, len(list.items))
	for i, item := range list.items {
		out[i] = item.ThreadDownload
	}
	return out
}

// forgetThreadDownloads drops the thread's list (CloseThread).
func (m *Manager) forgetThreadDownloads(threadID string) {
	m.downloadsMu.Lock()
	defer m.downloadsMu.Unlock()
	if list := m.threadDownloads[threadID]; list != nil && list.push != nil {
		list.push.Stop()
	}
	delete(m.threadDownloads, threadID)
}

var errDownloadNotFound = errors.New("browser: that download is no longer in this browser's list")

func (m *Manager) threadDownload(threadID, id string) (threadDownload, error) {
	m.downloadsMu.Lock()
	defer m.downloadsMu.Unlock()
	if list := m.threadDownloads[threadID]; list != nil {
		if item := list.find(strings.TrimSpace(id)); item != nil {
			return *item, nil
		}
	}
	return threadDownload{}, errDownloadNotFound
}

// CancelThreadDownload is the pane's cancel for a running download. It only
// asks: the engine's terminal report settles the download, as a cancel when
// the cancel won (downloadProgress removes a partial file in AO's
// directory; an engine saving to the Downloads folder itself removes its
// own there) or as a
// finish when the download finished first, so a cancel racing a finish never
// deletes the finished file. The engine's refusal is the answer and the
// download keeps running and listed as such.
func (m *Manager) CancelThreadDownload(access Access, id string) error {
	item, err := m.threadDownload(access.ThreadID, id)
	if err != nil {
		return err
	}
	p, scope := m.pageForDownload(item.engineID)
	if p == nil || scope == nil {
		// Its page is gone, and closing it settled the download.
		return nil
	}
	p.downloadMu.Lock()
	entry := p.downloadLocked(item.engineID)
	if entry == nil || entry.State != downloadStateRunning || entry.cancelRequested {
		// A cancel already with the engine stays its request: only its own
		// refusal withdraws it, never a later request's answer.
		p.downloadMu.Unlock()
		return nil
	}
	entry.cancelRequested = true
	// A refusal of an earlier cancel no longer stands.
	entry.Error = ""
	p.downloadMu.Unlock()
	err = scope.profile.CancelDownload(item.engineID)
	if err == nil {
		return nil
	}
	p.downloadMu.Lock()
	if entry := p.downloadLocked(item.engineID); entry != nil {
		entry.cancelRequested = false
	}
	p.downloadMu.Unlock()
	if errors.Is(err, errDownloadEnded) {
		// The download finished first; its report is the answer.
		return nil
	}
	return fmt.Errorf("browser: cancel the download of %s: %w", item.Name, err)
}

// ClearThreadDownloads removes the thread's settled downloads from its list.
// Running ones stay, so they can still be canceled. No file is deleted:
// one in the user's Downloads folder is the user's, and an artifact goes
// with the rest after artifactMaxAge.
func (m *Manager) ClearThreadDownloads(access Access) {
	m.downloadsMu.Lock()
	if list := m.threadDownloads[access.ThreadID]; list != nil {
		kept := list.items[:0]
		for _, item := range list.items {
			if item.State == downloadStateRunning {
				kept = append(kept, item)
			}
		}
		clear(list.items[len(kept):])
		list.items = kept
	}
	m.downloadsMu.Unlock()
	m.emitThreadState(access.ThreadID)
}

// finishedDownloadPath answers the file of one of the thread's finished
// downloads.
func (m *Manager) finishedDownloadPath(access Access, id string) (threadDownload, error) {
	item, err := m.threadDownload(access.ThreadID, id)
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
func (m *Manager) RevealThreadDownload(ctx context.Context, access Access, id string) error {
	item, err := m.finishedDownloadPath(access, id)
	if err != nil {
		return err
	}
	if m.revealFileInFileManager != nil {
		return m.revealFileInFileManager(ctx, item.path)
	}
	return revealFileInFileManager(ctx, item.path)
}

// OpenThreadDownload shows a finished download in a new tab of the thread's
// browser, for a type a tab shows (downloadOpensInPage). A page may open
// its workspace's downloads (authorizeFile). With ctx's numbered selection
// passed (selection.go) it opens nothing.
func (m *Manager) OpenThreadDownload(ctx context.Context, access Access, id string) (PageInfo, error) {
	turn, err := m.awaitSelectionTurn(ctx, access.ThreadID, selectionOf(ctx))
	if err != nil || turn.passed {
		return PageInfo{}, err
	}
	defer m.releaseSelectionTurn(turn)
	item, err := m.finishedDownloadPath(access, id)
	if err != nil {
		return PageInfo{}, err
	}
	if !downloadOpensInPage(item.path) {
		return PageInfo{}, fmt.Errorf("browser: a browser tab does not show %s; use Show in folder", item.Name)
	}
	info, err := m.openUserTab(ctx, access, turn, item.path, "", -1)
	if errors.Is(err, errSelectionPassed) {
		return PageInfo{}, nil
	}
	return info, err
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

// workspaceDownloadDir is the directory a workspace's finished downloads
// are kept in (workspaceAuthority.downloadDir), for the canonical
// workspace.
func (m *Manager) workspaceDownloadDir(workspace string) string {
	return filepath.Join(m.artifactRoot, "downloads", jarDirName(workspace))
}
