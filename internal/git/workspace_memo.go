package git

import (
	"sync"
	"time"
)

// workspaceMemoTTL bounds how long a workspace's memo survives without a
// status scan touching it. Closed workspaces stop scanning, so their
// values age out on the next store's sweep instead of pinning one per
// historical cwd for the process lifetime.
const workspaceMemoTTL = 30 * time.Minute

// workspaceMemo holds what one status scan of a workspace leaves for the
// next, keyed by cwd. A value is immutable once stored: every scan builds
// a fresh one and replaces the last wholesale, so a snapshot handed to a
// concurrent scan is always safe to read. Concurrent scans of one cwd (a
// watcher refresh racing an RPC Status call) both store complete values
// and the last writer wins. The zero value is ready to use.
type workspaceMemo[T any] struct {
	mu      sync.Mutex
	entries map[string]workspaceMemoEntry[T]
}

type workspaceMemoEntry[T any] struct {
	lastUsed time.Time
	value    T
}

// snapshot returns cwd's stored value.
func (m *workspaceMemo[T]) snapshot(cwd string) (T, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[cwd]
	return entry.value, ok
}

// store replaces cwd's value and drops workspaces whose last store is
// past workspaceMemoTTL.
func (m *workspaceMemo[T]) store(cwd string, value T, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = make(map[string]workspaceMemoEntry[T])
	}
	m.entries[cwd] = workspaceMemoEntry[T]{lastUsed: now, value: value}
	for key, entry := range m.entries {
		if now.Sub(entry.lastUsed) > workspaceMemoTTL {
			delete(m.entries, key)
		}
	}
}
