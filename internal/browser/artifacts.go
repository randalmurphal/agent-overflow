package browser

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	maxArtifactRootBytes   = int64(4 << 30)
	artifactMaxAge         = 7 * 24 * time.Hour
	maxArtifactScanEntries = 100_000
)

func (m *Manager) prepareArtifacts() {
	m.artifactInitMu.Lock()
	defer m.artifactInitMu.Unlock()
	if m.artifactReady {
		return
	}
	_ = os.MkdirAll(m.artifactRoot, 0o700)
	cutoff := time.Now().Add(-artifactMaxAge)
	var total int64
	entries := 0
	_ = filepath.WalkDir(m.artifactRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		entries++
		if entries > maxArtifactScanEntries {
			return fs.SkipAll
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if os.Remove(path) == nil {
				return nil
			}
		}
		total += info.Size()
		return nil
	})
	if entries > maxArtifactScanEntries {
		total = maxArtifactRootBytes
	}
	m.artifactBytes.Store(total)
	m.artifactReady = true
}

func (m *Manager) reserveArtifacts(bytes int64) bool {
	m.prepareArtifacts()
	for {
		current := m.artifactBytes.Load()
		if bytes < 0 || current > maxArtifactRootBytes-bytes {
			return false
		}
		if m.artifactBytes.CompareAndSwap(current, current+bytes) {
			return true
		}
	}
}

func (m *Manager) settleArtifactReservation(reserved, actual int64) {
	m.artifactBytes.Add(actual - reserved)
}
