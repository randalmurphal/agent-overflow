package browser

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/browser/turnorder"
)

// A thread's downloads as its browser pane lists them. Each page keeps its
// own download log for browser_downloads (downloads_log.go); this is the
// thread's view of the same downloads, which outlives the page a download
// began in, so a file stays findable after its tab closes. A download's
// start is added here in the hold that adds it to its page's log
// (noteDownloadStartLocked), and every later change to its entry is noted
// in the hold that makes it (noteDownloadLocked), then published after it
// (publishDownloads): a start, a finish, a failure or a cancel pushes the
// thread's state at once, while byte progress is coalesced to one push per
// downloadPushInterval per thread. A download that settles other than by
// the user's cancel is announced once, as a Kind "download" event. The
// list is in memory only and goes with the thread (CloseThread); clearing
// it never deletes a file.

const (
	// maxThreadDownloads bounds the settled downloads the thread's list
	// shows, the most recently settled. A running download is never
	// dropped, and the quotas bound how many run.
	maxThreadDownloads = 20
	// maxResolvableDownloads bounds the settled downloads past the shown
	// ones whose id still resolves (threadDownloadList.resolvable), so the
	// action of a notification announced moments before its row left the
	// shown list still finds its download.
	maxResolvableDownloads = 256
	// downloadPushInterval is the most often byte progress pushes a thread's
	// state.
	downloadPushInterval = time.Second
)

// ThreadDownload is one download in the thread's browser pane. ID is AO's
// own key for it. Source is the address of the page it began on, or the
// download's own address when that page had none, and empty when neither
// is known. State is a DownloadInfo state. TotalBytes is zero when the
// engine does not know the size. Openable is whether a browser tab can
// show the finished file (downloadOpensInPage).
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
	// (listedDownloadFile); path the finished file and file what it was
	// when the download finished, nil for a file AO does not open.
	engineID  string
	workspace string
	path      string
	file      os.FileInfo
	// settled numbers the item among its list's settled ones in the order
	// they settled, from 1; zero while it runs.
	settled uint64
}

// threadDownloadList is a thread's downloads: shown, every running one and
// the newest maxThreadDownloads settled, by settle order, newest started
// first; and past them, resolvable by id only, the newest
// maxResolvableDownloads settled ones the shown bound dropped. Its fields
// are written only by its methods, which enforce both bounds as an item
// settles, so no caller trims it. Guarded by Manager.downloadsMu.
type threadDownloadList struct {
	shown []*threadDownload
	// resolvable is newest settled first. Its items carry no Source, which
	// nothing shows.
	resolvable []*threadDownload
	// settles counts the items that settled (threadDownload.settled).
	settles uint64
	// push is the pending coalesced state push, nil for none.
	push *time.Timer
}

// start adds a download that just started, as the newest.
func (l *threadDownloadList) start(item *threadDownload) {
	l.shown = append([]*threadDownload{item}, l.shown...)
}

// change applies a change to the item with key id, shown or only
// resolvable, and answers it and whether it ran before the change; nil
// when the list no longer has it. An item that settles in the change is
// numbered the newest settled, and the earliest settled past the shown
// bound leaves the shown items then.
func (l *threadDownloadList) change(id string, apply func(*threadDownload)) (item *threadDownload, wasRunning bool) {
	item = l.find(id)
	if item == nil {
		return nil, false
	}
	wasRunning = item.State == downloadStateRunning
	apply(item)
	if item.State == downloadStateRunning || item.settled != 0 {
		return item, wasRunning
	}
	l.settles++
	item.settled = l.settles
	l.boundShown()
	return item, wasRunning
}

// boundShown moves the earliest settled shown items past
// maxThreadDownloads to the resolvable ones, which keep the newest
// maxResolvableDownloads.
func (l *threadDownloadList) boundShown() {
	for {
		settled, earliest := 0, -1
		for i, item := range l.shown {
			if item.settled == 0 {
				continue
			}
			settled++
			if earliest < 0 || item.settled < l.shown[earliest].settled {
				earliest = i
			}
		}
		if settled <= maxThreadDownloads {
			return
		}
		dropped := l.shown[earliest]
		l.shown = slices.Delete(l.shown, earliest, earliest+1)
		dropped.Source = ""
		at, _ := slices.BinarySearchFunc(l.resolvable, dropped.settled, func(item *threadDownload, settled uint64) int {
			return cmp.Compare(settled, item.settled)
		})
		l.resolvable = slices.Insert(l.resolvable, at, dropped)
		if len(l.resolvable) > maxResolvableDownloads {
			clear(l.resolvable[maxResolvableDownloads:])
			l.resolvable = l.resolvable[:maxResolvableDownloads]
		}
	}
}

// find answers the item with key id, shown or only resolvable, or nil.
func (l *threadDownloadList) find(id string) *threadDownload {
	for _, item := range l.shown {
		if item.ID == id {
			return item
		}
	}
	for _, item := range l.resolvable {
		if item.ID == id {
			return item
		}
	}
	return nil
}

// clear drops every settled item, shown or resolvable; running ones stay.
func (l *threadDownloadList) clear() {
	l.shown = slices.DeleteFunc(l.shown, func(item *threadDownload) bool { return item.State != downloadStateRunning })
	clear(l.resolvable)
	l.resolvable = nil
}

// list answers the shown items, newest started first.
func (l *threadDownloadList) list() []ThreadDownload {
	out := make([]ThreadDownload, len(l.shown))
	for i, item := range l.shown {
		out[i] = item.ThreadDownload
	}
	return out
}

// completedFiles answers the files of the shown completed items of the
// canonical workspace at path, as they were when each completed.
func (l *threadDownloadList) completedFiles(workspace, path string) []os.FileInfo {
	var files []os.FileInfo
	for _, item := range l.shown {
		if item.State == downloadStateCompleted && item.workspace == workspace && item.path == path && item.file != nil {
			files = append(files, item.file)
		}
	}
	return files
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

// threadListLocked answers the thread's list, made on first use. The
// caller holds m.downloadsMu.
func (m *Manager) threadListLocked(threadID string) *threadDownloadList {
	if m.threadDownloads == nil {
		m.threadDownloads = make(map[string]*threadDownloadList)
	}
	list := m.threadDownloads[threadID]
	if list == nil {
		list = &threadDownloadList{}
		m.threadDownloads[threadID] = list
	}
	return list
}

// noteDownloadStartLocked adds a page's new entry to its thread's list,
// with source as its Source, empty when no address is known. The caller
// holds p's download log; m.downloadsMu is taken inside it.
func (m *Manager) noteDownloadStartLocked(p *managedPage, entry *DownloadInfo, source string) downloadNote {
	m.downloadsMu.Lock()
	defer m.downloadsMu.Unlock()
	m.threadListLocked(p.owner).start(&threadDownload{
		ThreadDownload: ThreadDownload{ID: entry.key, Source: source, State: downloadStateRunning},
		engineID:       entry.ID, workspace: p.access.Workspace,
	})
	return m.noteChangeLocked(p, entry, true)
}

// noteDownloadLocked copies a change to a page's entry into its thread's
// list. The caller holds p's download log; m.downloadsMu is taken inside
// it.
func (m *Manager) noteDownloadLocked(p *managedPage, entry *DownloadInfo) downloadNote {
	m.downloadsMu.Lock()
	defer m.downloadsMu.Unlock()
	return m.noteChangeLocked(p, entry, false)
}

// noteChangeLocked is noteDownloadLocked's change, started true for the
// change that lists the item. The caller holds m.downloadsMu.
func (m *Manager) noteChangeLocked(p *managedPage, entry *DownloadInfo, started bool) downloadNote {
	item, wasRunning := m.threadListLocked(p.owner).change(entry.key, func(item *threadDownload) {
		item.Name = entry.SuggestedName
		if entry.Path != "" {
			item.path, item.file = entry.Path, entry.file
			item.Name = filepath.Base(entry.Path)
		}
		item.State, item.ReceivedBytes, item.TotalBytes, item.Error = entry.State, entry.Bytes, entry.TotalBytes, entry.Error
		item.Openable = item.State == downloadStateCompleted && item.file != nil && downloadOpensInPage(item.Name)
	})
	if item == nil {
		// A later change to an item the user cleared or the bounds dropped,
		// which only a settled item can be: nothing to show.
		return downloadNote{}
	}
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
	if list == nil || len(list.shown) == 0 {
		return nil
	}
	return list.list()
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
	held := p.downloads.hold()
	entry := held.entry(item.engineID)
	if entry == nil || entry.State != downloadStateRunning || entry.cancelRequested {
		// A cancel already with the engine stays its request: only its own
		// refusal withdraws it, never a later request's answer.
		held.release()
		return nil
	}
	entry.cancelRequested = true
	// A refusal of an earlier cancel no longer stands.
	entry.Error = ""
	held.release()
	err = scope.profile.CancelDownload(item.engineID)
	if err == nil {
		return nil
	}
	held = p.downloads.hold()
	if entry := held.entry(item.engineID); entry != nil {
		entry.cancelRequested = false
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
// artifact goes with the rest after artifactMaxAge.
func (m *Manager) ClearThreadDownloads(access Access) {
	m.downloadsMu.Lock()
	if list := m.threadDownloads[access.ThreadID]; list != nil {
		list.clear()
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
	turn, err := m.selectionTurn(ctx, access.ThreadID)
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
	if m.downloadOpening != nil {
		m.downloadOpening()
	}
	opened, err := m.openFile(ctx, access, item.path, item.file, OpenOptions{PageID: info.ID})
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

// jarDownloadingDir is the artifact subtree an engine keeping downloads as
// artifacts writes each jar's running downloads in (jarScope.downloadDir).
// It is apart from the workspaces' directories, so a file authorizeFile
// opens from one is a finished download AO moved there.
const jarDownloadingDir = "downloading"

// workspaceDownloadsDir is the artifact subtree of the workspaces' finished
// downloads. No earlier release wrote to it: one wrote its engine's running
// downloads, and the files it finished, into "downloads", whose files
// authorizeFile never grants and the artifact prune removes with age.
const workspaceDownloadsDir = "finished"

// workspaceDownloadDir is the directory a workspace's finished downloads
// are kept in (workspaceAuthority.downloadDir), for the canonical
// workspace.
func (m *Manager) workspaceDownloadDir(workspace string) string {
	return filepath.Join(m.artifactRoot, workspaceDownloadsDir, jarDirName(workspace))
}
