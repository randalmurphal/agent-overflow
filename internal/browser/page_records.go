package browser

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"agent-overflow/internal/atomicfile"
)

// The saved copy of the suspended pages and of the thread's cookie jar
// setting: one JSON file per thread under browserPageRecordDir, named by a
// digest of the thread id, written whole and atomically whenever the thread's
// suspended pages or its jar setting change and for every open page at
// shutdown. A thread with no saved page and the default jar has no file. A thread keeps at most maxPagesPerThread pages, live and
// suspended together, so a file is bounded and a write costs one small file
// however many threads keep pages. The records are browser state like the
// profile tree beside them: the Manager alone reads and writes them, and
// deleting or archiving a thread, clearing site data, and turning the browser
// off remove them.

// browserPageRecordDir is the AO-owned directory of the saved pages.
const browserPageRecordDir = "browser-pages"

const (
	pageRecordVersion = 1
	// maxPageRecordFileBytes bounds one file: maxPagesPerThread pages, each
	// at most a bounded URL and title with worst-case JSON escaping.
	maxPageRecordFileBytes = 4 << 20
	maxPageIDBytes         = 128
	maxPageLabelRunes      = 80
)

type pageRecordFile struct {
	Version      int    `json:"version"`
	ThreadID     string `json:"threadId"`
	ActivePageID string `json:"activePageId,omitempty"`
	// Jar is the thread's jar setting (jar.go), which every one of its pages
	// opens and restores in; omitted for the default.
	Jar   string       `json:"jar,omitempty"`
	Pages []pageRecord `json:"pages"`
}

type pageRecord struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Title    string `json:"title,omitempty"`
	Label    string `json:"label,omitempty"`
	Order    int64  `json:"order"`
	Created  int64  `json:"created"`
	LastUsed int64  `json:"lastUsed"`
}

func (m *Manager) recordPath(threadID string) string {
	digest := sha256.Sum256([]byte(threadID))
	return filepath.Join(m.recordDir, hex.EncodeToString(digest[:16])+".json")
}

// persistThreads rewrites the saved copy of each thread from its current
// suspended pages and jar setting, removing the file of a thread that has no
// page and the default jar.
func (m *Manager) persistThreads(threadIDs []string) error {
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	return m.writeRecordFilesLocked(threadIDs)
}

// writeRecordFilesLocked snapshots the threads' records under m.mu and writes
// them. The caller holds persistMu, which keeps the snapshot and its write in
// order with every other write.
func (m *Manager) writeRecordFilesLocked(threadIDs []string) error {
	m.mu.Lock()
	files := make([]pageRecordFile, 0, len(threadIDs))
	for _, threadID := range threadIDs {
		files = append(files, m.recordFileLocked(threadID, false))
	}
	m.mu.Unlock()
	var errs []error
	for _, file := range files {
		if err := m.writeRecordFile(file); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// recordFileLocked is the saved copy of the thread as it is: its suspended
// pages and, with withLive, its live pages as the records a suspension
// makes of them (recordOf). The caller holds m.mu.
func (m *Manager) recordFileLocked(threadID string, withLive bool) pageRecordFile {
	file := pageRecordFile{Version: pageRecordVersion, ThreadID: threadID}
	for _, tab := range m.threadTabsLocked(threadID) {
		rec := tab.rec
		if rec == nil && withLive && tab.live != nil {
			rec = recordOf(tab.live, tab.live.cachedInfo())
		}
		if rec != nil {
			file.Pages = append(file.Pages, rec.saved())
		}
	}
	if session, ok := m.sessions[threadID]; ok {
		file.ActivePageID = session.ActivePageID
		file.Jar = savedJar(jarKindOf(session))
	}
	return file
}

// saved is the record as its thread's saved copy holds it.
func (r *suspendedPage) saved() pageRecord {
	return pageRecord{
		ID: r.id, URL: r.url, Title: r.title, Label: r.label,
		Order: r.tabOrder, Created: r.createdAt, LastUsed: r.lastUse,
	}
}

// writeRecordFile writes one thread's saved copy, or removes it when it
// holds no page and the default jar. The caller holds persistMu.
func (m *Manager) writeRecordFile(file pageRecordFile) error {
	path := m.recordPath(file.ThreadID)
	if len(file.Pages) == 0 && file.Jar == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("browser: remove saved pages: %w", err)
		}
		return nil
	}
	if err := atomicfile.WriteJSON(path, file); err != nil {
		return fmt.Errorf("browser: save pages: %w", err)
	}
	return nil
}

// savedJar is a jar kind as a saved copy writes it: the default is omitted,
// so a thread that never chose a jar saves what earlier builds saved.
func savedJar(kind string) string {
	if kind == JarWorktree {
		return ""
	}
	return kind
}

func removeRecordDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("browser: remove saved pages: %w", err)
	}
	return nil
}

// loadPageRecords brings the saved pages back as suspended pages at boot. A
// deployment with no engine leaves them for one that has it; a disabled
// browser keeps no browser state, so they are removed. A file that cannot be
// read, is not a valid record of its thread, or belongs to a thread keep
// rejects is removed; each such outcome is logged, because the pages it held
// are gone from their thread.
func (m *Manager) loadPageRecords(keep func(threadID string) (bool, error)) {
	if !m.Available() {
		return
	}
	if !m.config.Enabled {
		if err := removeRecordDir(m.recordDir); err != nil {
			log.Printf("%v", err)
		}
		return
	}
	entries, err := os.ReadDir(m.recordDir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("browser: read saved pages: %v", err)
		return
	}
	for _, entry := range entries {
		path := filepath.Join(m.recordDir, entry.Name())
		file, err := m.readRecordFile(path)
		if err == nil && keep != nil {
			var kept bool
			if kept, err = keep(file.ThreadID); err != nil {
				log.Printf("browser: saved pages of thread %s kept for the next boot: %v", file.ThreadID, err)
				continue
			}
			if !kept {
				if err := os.Remove(path); err != nil {
					log.Printf("browser: remove the saved pages of thread %s: %v", file.ThreadID, err)
				}
				continue
			}
		}
		if err != nil {
			log.Printf("browser: drop saved pages %s: %v", entry.Name(), err)
			if err := os.RemoveAll(path); err != nil {
				log.Printf("browser: remove saved pages %s: %v", entry.Name(), err)
			}
			continue
		}
		m.addLoadedRecords(file)
	}
}

// loadedJar is a saved jar kind as the Manager holds it.
func loadedJar(saved string) string {
	if saved == "" {
		return JarWorktree
	}
	return saved
}

// readRecordFile reads and validates one saved file. Pages beyond the
// thread cap are dropped with a log line; any other defect rejects the file.
func (m *Manager) readRecordFile(path string) (pageRecordFile, error) {
	var file pageRecordFile
	if strings.Contains(filepath.Base(path), ".tmp-") {
		return file, errors.New("an interrupted write")
	}
	handle, err := os.Open(path)
	if err != nil {
		return file, err
	}
	defer handle.Close()
	data, err := io.ReadAll(io.LimitReader(handle, maxPageRecordFileBytes+1))
	if err != nil {
		return file, err
	}
	if len(data) > maxPageRecordFileBytes {
		return file, fmt.Errorf("larger than %d bytes", maxPageRecordFileBytes)
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return file, err
	}
	if file.Version != pageRecordVersion {
		return file, fmt.Errorf("unknown version %d", file.Version)
	}
	if file.ThreadID == "" || m.recordPath(file.ThreadID) != path {
		return file, errors.New("not the saved pages of the thread it names")
	}
	if file.Jar != "" && !validJarKind(file.Jar) {
		return file, fmt.Errorf("unknown cookie jar %q", truncateUTF8(file.Jar, 32))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := make(map[string]bool, len(file.Pages))
	for _, page := range file.Pages {
		if page.ID == "" || len(page.ID) > maxPageIDBytes || !utf8.ValidString(page.ID) || seen[page.ID] || m.suspended[page.ID] != nil {
			return file, fmt.Errorf("invalid or repeated page id %q", truncateUTF8(page.ID, maxPageIDBytes))
		}
		seen[page.ID] = true
		if len(page.URL) > maxBrowserURLBytes || len(page.Title) > maxBrowserTitleBytes {
			return file, fmt.Errorf("page %s exceeds the address or title bound", page.ID)
		}
		if utf8.RuneCountInString(page.Label) > maxPageLabelRunes || strings.IndexFunc(page.Label, unicode.IsControl) >= 0 {
			return file, fmt.Errorf("page %s has an invalid label", page.ID)
		}
	}
	if len(file.Pages) > maxPagesPerThread {
		log.Printf("browser: thread %s saved %d pages; keeping the first %d", file.ThreadID, len(file.Pages), maxPagesPerThread)
		file.Pages = file.Pages[:maxPagesPerThread]
	}
	return file, nil
}

// addLoadedRecords registers one thread's saved pages as suspended, sets its
// jar setting, and selects its saved active page, or its most recently used
// one.
func (m *Manager) addLoadedRecords(file pageRecordFile) {
	m.mu.Lock()
	for _, page := range file.Pages {
		m.suspended[page.ID] = &suspendedPage{
			id: page.ID, owner: file.ThreadID,
			url: page.URL, title: page.Title, label: page.Label,
			tabOrder: page.Order, createdAt: page.Created, lastUse: page.LastUsed,
		}
	}
	info := m.sessionLocked(file.ThreadID)
	info.ActivePageID = file.ActivePageID
	info.Jar = loadedJar(file.Jar)
	m.sessions[file.ThreadID] = info
	m.mu.Unlock()
	m.repairActivePage(file.ThreadID)
}
