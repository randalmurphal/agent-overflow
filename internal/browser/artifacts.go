package browser

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"agent-overflow/internal/browser/quota"
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
	m.artifactBytes.SetUsed(total)
	m.artifactReady = true
}

// reserveArtifacts holds bytes of the artifact quota, measured on first use
// (prepareArtifacts), and of any pools besides it, all or none.
func (m *Manager) reserveArtifacts(bytes int64, pools ...*quota.Pool) (*quota.Reservation, error) {
	m.prepareArtifacts()
	return quota.Reserve(bytes, append(pools, m.artifactBytes)...)
}
