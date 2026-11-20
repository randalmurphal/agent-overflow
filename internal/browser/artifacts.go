package browser

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
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

// prepareArtifacts measures, once per Manager, what the artifact root
// holds into the artifact quota, removing the artifacts older than
// artifactMaxAge as it goes. A scan it cannot finish answers the error and
// measures nothing, so every reservation is refused with the reason until
// a later call measures it all. An expired artifact it cannot remove stays
// charged and is logged; one gone meanwhile is not charged. Writes follow
// a link where the scan would not, so a root that is a link, or a link to
// a directory inside it, is refused: what the quota measures is where the
// artifacts are written.
func (m *Manager) prepareArtifacts() error {
	m.artifactInitMu.Lock()
	defer m.artifactInitMu.Unlock()
	if m.artifactReady {
		return nil
	}
	if err := os.MkdirAll(m.artifactRoot, 0o700); err != nil {
		return fmt.Errorf("browser: create the artifact folder: %w", err)
	}
	if root, err := os.Lstat(m.artifactRoot); err != nil {
		return fmt.Errorf("browser: the artifact folder: %w", err)
	} else if !root.IsDir() {
		return fmt.Errorf("browser: the artifact folder %s is a link, not a folder, so its size cannot be measured and nothing is kept there", m.artifactRoot)
	}
	cutoff := time.Now().Add(-artifactMaxAge)
	var total int64
	entries := 0
	err := filepath.WalkDir(m.artifactRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		entries++
		if entries > maxArtifactScanEntries {
			return fs.SkipAll
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if target, err := os.Stat(path); err == nil && target.IsDir() {
				return fmt.Errorf("%s is a link to a folder, whose size the artifact quota cannot measure", path)
			}
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			err := os.Remove(path)
			if err == nil || errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			log.Printf("browser: remove the expired artifact %s: %v; its %d bytes stay charged", path, err, info.Size())
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return fmt.Errorf("browser: measure the artifact folder %s: %w", m.artifactRoot, err)
	}
	if entries > maxArtifactScanEntries {
		total = maxArtifactRootBytes
	}
	m.artifactBytes.SetUsed(total)
	m.artifactReady = true
	return nil
}

// reserveArtifacts holds bytes of the artifact quota, measured on first use
// (prepareArtifacts), and of any pools besides it, all or none.
func (m *Manager) reserveArtifacts(bytes int64, pools ...*quota.Pool) (*quota.Reservation, error) {
	if err := m.prepareArtifacts(); err != nil {
		return nil, err
	}
	return quota.Reserve(bytes, append(pools, m.artifactBytes)...)
}
