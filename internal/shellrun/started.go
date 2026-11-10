package shellrun

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// startedMarkersDir is the directory under Config.Dir that holds the markers.
const startedMarkersDir = "started"

// startedMarkers records on disk the id of every command this service
// started, so the next process can tell a start repeated across a restart
// from a new one. A marker is an empty file named by the run id. The newest
// kept markers survive; a repeated start arrives within a reconnect of the
// first.
type startedMarkers struct {
	dir  string
	kept int

	mu    sync.Mutex
	order []string // oldest first
}

// loadStartedMarkers reads the markers an earlier process left, removes the
// ones past kept, and returns the ids that remain.
func loadStartedMarkers(dir string, kept int) (*startedMarkers, map[string]bool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create started markers directory: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read started markers: %w", err)
	}
	type marker struct {
		runID string
		at    time.Time
	}
	found := make([]marker, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read started marker %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		found = append(found, marker{runID: entry.Name(), at: info.ModTime()})
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].at.Before(found[j].at) })
	markers := &startedMarkers{dir: dir, kept: kept}
	for len(found) > kept {
		if err := os.Remove(filepath.Join(dir, found[0].runID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("remove old started marker: %w", err)
		}
		found = found[1:]
	}
	earlier := make(map[string]bool, len(found))
	for _, marker := range found {
		earlier[marker.runID] = true
		markers.order = append(markers.order, marker.runID)
	}
	return markers, earlier, nil
}

// add records that the run is about to start. The command must not start
// when it fails: nothing would stop a later process from running it again.
func (m *startedMarkers) add(runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.WriteFile(filepath.Join(m.dir, runID), nil, 0o600); err != nil {
		return err
	}
	m.order = append(m.order, runID)
	for len(m.order) > m.kept {
		if err := os.Remove(filepath.Join(m.dir, m.order[0])); err != nil && !errors.Is(err, os.ErrNotExist) {
			// The next start of the service removes it.
			log.Printf("shellrun: remove old started marker: %v", err)
		}
		m.order = m.order[1:]
	}
	return nil
}
