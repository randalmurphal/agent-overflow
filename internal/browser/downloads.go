package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
)

func (m *Manager) downloadStarted(event *cdpbrowser.EventDownloadWillBegin) {
	p, scope := m.pageForFrame(event.FrameID)
	if p == nil {
		return
	}
	p.downloadMu.Lock()
	p.downloadSeq++
	entry := DownloadInfo{
		Sequence: p.downloadSeq, ID: event.GUID, URL: event.URL,
		SuggestedName: safeArtifactName(event.SuggestedFilename, "download"),
		State:         "in_progress", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), reservedBytes: maxDownloadBytes,
	}
	scopeReserved := scope != nil && scope.downloadBytes.Add(maxDownloadBytes) <= maxWorkspaceDownloadBytes
	globalReserved := scopeReserved && m.reserveArtifacts(maxDownloadBytes)
	if !scopeReserved || !globalReserved {
		if scopeReserved {
			scope.downloadBytes.Add(-maxDownloadBytes)
		} else if scope != nil && scope.downloadBytes.Load() > maxWorkspaceDownloadBytes {
			scope.downloadBytes.Add(-maxDownloadBytes)
		}
		entry.State, entry.Error, entry.reservedBytes = "canceled", fmt.Sprintf("workspace downloads exceed %d bytes", maxWorkspaceDownloadBytes), 0
		if scopeReserved && !globalReserved {
			entry.Error = fmt.Sprintf("browser artifacts exceed %d bytes", maxArtifactRootBytes)
		}
	}
	p.downloads = append(p.downloads, entry)
	if len(p.downloads) > 100 {
		p.downloads = append([]DownloadInfo(nil), p.downloads[len(p.downloads)-100:]...)
	}
	p.signalDownloadLocked()
	p.downloadMu.Unlock()
	if entry.State == "canceled" && scope != nil {
		go func() {
			_ = cdpbrowser.CancelDownload(event.GUID).WithBrowserContextID(scope.contextID).Do(browserCommandContext(scope.ctx))
		}()
	}
}

func (m *Manager) downloadProgress(event *cdpbrowser.EventDownloadProgress) {
	p, scope := m.pageForDownload(event.GUID)
	if p == nil {
		return
	}
	p.downloadMu.Lock()
	index := -1
	for i := len(p.downloads) - 1; i >= 0; i-- {
		if p.downloads[i].ID == event.GUID {
			index = i
			break
		}
	}
	if index < 0 {
		p.downloadMu.Unlock()
		return
	}
	entry := &p.downloads[index]
	if entry.State == "canceled" && entry.reservedBytes == 0 {
		p.downloadMu.Unlock()
		return
	}
	entry.Bytes = int64(event.ReceivedBytes)
	if entry.Bytes > maxDownloadBytes && event.State == cdpbrowser.DownloadProgressStateInProgress {
		entry.State = "canceled"
		entry.Error = fmt.Sprintf("download exceeds %d bytes", maxDownloadBytes)
		if scope != nil && entry.reservedBytes > 0 {
			scope.downloadBytes.Add(-entry.reservedBytes)
			m.settleArtifactReservation(entry.reservedBytes, 0)
			entry.reservedBytes = 0
		}
		p.signalDownloadLocked()
		p.downloadMu.Unlock()
		if scope != nil {
			go func() {
				_ = cdpbrowser.CancelDownload(event.GUID).WithBrowserContextID(scope.contextID).Do(browserCommandContext(scope.ctx))
				_ = os.Remove(filepath.Join(scope.downloadDir, filepath.Base(event.GUID)))
			}()
		}
		return
	}
	switch event.State {
	case cdpbrowser.DownloadProgressStateCompleted:
		entry.State = "completed"
		if scope != nil && entry.reservedBytes > 0 {
			scope.downloadBytes.Add(-entry.reservedBytes + entry.Bytes)
			m.settleArtifactReservation(entry.reservedBytes, entry.Bytes)
			entry.reservedBytes = 0
		}
		if scope != nil {
			source := event.FilePath
			if source == "" {
				source = filepath.Join(scope.downloadDir, filepath.Base(event.GUID))
			}
			if !pathInside(scope.downloadDir, source) {
				entry.State, entry.Error = "failed", "browser returned a download path outside its artifact directory"
				break
			}
			destination, renameErr := uniqueArtifactPath(scope.downloadDir, entry.SuggestedName)
			if renameErr == nil {
				renameErr = os.Rename(source, destination)
			}
			if renameErr != nil {
				entry.State, entry.Error = "failed", renameErr.Error()
			} else {
				entry.Path = destination
			}
		}
	case cdpbrowser.DownloadProgressStateCanceled:
		entry.State = "canceled"
		if scope != nil && entry.reservedBytes > 0 {
			scope.downloadBytes.Add(-entry.reservedBytes)
			m.settleArtifactReservation(entry.reservedBytes, 0)
			entry.reservedBytes = 0
		}
	}
	p.signalDownloadLocked()
	p.downloadMu.Unlock()
}

func pathInside(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func (m *Manager) pageForFrame(frameID cdp.FrameID) (*managedPage, *workspaceScope) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			p.frameMu.RLock()
			_, ok := p.frames[frameID]
			p.frameMu.RUnlock()
			if ok {
				return p, scope
			}
		}
	}
	return nil, nil
}

func (m *Manager) pageForDownload(guid string) (*managedPage, *workspaceScope) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			p.downloadMu.Lock()
			found := false
			for _, download := range p.downloads {
				if download.ID == guid {
					found = true
					break
				}
			}
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

func (m *Manager) cancelPageDownloads(p *managedPage, scope *workspaceScope) {
	p.downloadMu.Lock()
	ids := make([]string, 0)
	for i := range p.downloads {
		entry := &p.downloads[i]
		if entry.State != "in_progress" {
			continue
		}
		entry.State, entry.Error = "canceled", "page closed"
		if entry.reservedBytes > 0 {
			scope.downloadBytes.Add(-entry.reservedBytes)
			m.settleArtifactReservation(entry.reservedBytes, 0)
			entry.reservedBytes = 0
		}
		ids = append(ids, entry.ID)
	}
	if len(ids) > 0 {
		p.signalDownloadLocked()
	}
	p.downloadMu.Unlock()
	for _, id := range ids {
		_ = cdpbrowser.CancelDownload(id).WithBrowserContextID(scope.contextID).Do(browserCommandContext(scope.ctx))
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
			if download.Sequence > after && download.State != "in_progress" {
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
