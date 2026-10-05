package worktreewatch

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"agent-overflow/internal/gitroot"
)

const (
	// DefaultDebounce coalesces the burst of events one git command writes
	// into the common dir (`index.lock`, `HEAD`, a registration's files)
	// into one registry read.
	DefaultDebounce = 250 * time.Millisecond

	// DefaultPollInterval is the read cadence when a filesystem watch could
	// not be installed (Linux inotify limit exhausted, a filesystem that
	// never delivers). A registry read is a readdir plus one stat per
	// registration, so the fallback is cheap enough to keep the detection
	// prompt.
	DefaultPollInterval = 3 * time.Second

	// DefaultLivenessInterval re-reads every registry regardless of events.
	// It is what catches a registered worktree deleted with `rm -rf` at a
	// path nothing watches, and a watch that installed but went silent
	// (WSL drvfs, a macOS stream that died in sleep). Its cost is one
	// readdir per project per minute.
	DefaultLivenessInterval = 60 * time.Second
)

// Config wires a Manager.
type Config struct {
	// OnChange receives the project path exactly as SetProjects supplied it:
	// once when the project is first watched, and once per observed change
	// to its registrations afterwards. Calls for one project never overlap;
	// a change observed while a call runs yields exactly one more call after
	// it returns. Required.
	//
	// removed lists the worktrees that vanished since the previous read:
	// registered and present on disk then, not both now. The first read of
	// a project has nothing to compare against and reports none. Removals
	// observed while a call runs are carried into the next call, and a
	// path that reappears before that call is dropped from it. Each path is
	// spelled as the registry records it (git writes the symlink-resolved
	// path), which can differ from the spelling a caller holds; compare with
	// git.CanonicalPath.
	OnChange func(project string, removed []string)

	// ExtraDir names a second directory whose direct entries are worktrees
	// of project (the app's own cut location), so their deletion is seen
	// without waiting for the liveness read. Optional; "" means none.
	ExtraDir func(project string) string

	Debounce         time.Duration
	PollInterval     time.Duration
	LivenessInterval time.Duration

	// disableFSWatch forces the polling path; tests use it to prove the
	// fallback observes the same changes the watches do.
	disableFSWatch bool
}

// Registration is one entry of a project's worktree registry: the working
// tree path git recorded, and whether that directory is on disk right now.
type Registration struct {
	Path    string
	Present bool
}

// Manager owns the watches, the read loop and the callback goroutines for
// every project handed to SetProjects. Safe for concurrent use.
type Manager struct {
	cfg Config
	fs  *fsnotify.Watcher

	mu       sync.Mutex
	projects map[string]*projectWatch
	// byDir maps a watched directory to the project keys that asked for it.
	// Two project rows can share one registry (a project rooted at a linked
	// worktree of another project's repository), so the watch is dropped
	// only when its last project leaves.
	byDir   map[string]map[string]struct{}
	polling bool
	closed  bool

	done     chan struct{}
	loopDone chan struct{}
	dispatch sync.WaitGroup
	// pending is the set of project keys whose watches delivered an event
	// since the last read; the loop owns it.
	pending map[string]struct{}
}

type projectWatch struct {
	key         string
	root        string
	registryDir string
	extraDir    string
	last        []Registration
	initialized bool
	running     bool
	rerun       bool
	// removed holds the vanished paths no OnChange call has reported yet.
	removed map[string]struct{}
}

// NewManager starts the read loop. It never fails: when the platform cannot
// hand out a filesystem watcher every project is polled instead.
func NewManager(cfg Config) *Manager {
	if cfg.OnChange == nil {
		panic("worktreewatch: NewManager requires OnChange")
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = DefaultDebounce
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.LivenessInterval <= 0 {
		cfg.LivenessInterval = DefaultLivenessInterval
	}
	m := &Manager{
		cfg:      cfg,
		projects: make(map[string]*projectWatch),
		byDir:    make(map[string]map[string]struct{}),
		done:     make(chan struct{}),
		loopDone: make(chan struct{}),
		pending:  make(map[string]struct{}),
	}
	if !cfg.disableFSWatch {
		fs, err := fsnotify.NewWatcher()
		if err != nil {
			log.Printf("worktreewatch: fs watch unavailable (%v); polling every %s", err, cfg.PollInterval)
		} else {
			m.fs = fs
		}
	}
	if m.fs == nil {
		m.polling = true
	}
	go m.loop()
	return m
}

// SetProjects makes the watched set equal to projects. Paths are compared
// verbatim, so a caller must spell a project the same way every time; the
// store's project row is that spelling. Newly added projects are read at
// once and reported through OnChange; removed ones stop being watched and
// receive no further calls after any call already running returns.
func (m *Manager) SetProjects(projects []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	want := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		if strings.TrimSpace(project) == "" {
			continue
		}
		want[project] = struct{}{}
	}
	for key, pw := range m.projects {
		if _, keep := want[key]; keep {
			continue
		}
		m.forgetLocked(pw)
	}
	for key := range want {
		if _, ok := m.projects[key]; ok {
			continue
		}
		pw := &projectWatch{key: key, root: filepath.Clean(key)}
		if m.cfg.ExtraDir != nil {
			pw.extraDir = filepath.Clean(m.cfg.ExtraDir(key))
			if pw.extraDir == "." {
				pw.extraDir = ""
			}
		}
		m.projects[key] = pw
		m.evaluateLocked(pw)
	}
}

// Projects returns the watched project paths in no particular order.
func (m *Manager) Projects() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.projects))
	for key := range m.projects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Polling reports whether the manager is reading registries on a timer
// because a filesystem watch could not be installed.
func (m *Manager) Polling() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.polling
}

// Close stops the loop, drops every watch and waits for OnChange calls in
// flight to return. Idempotent.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.done)
	m.mu.Unlock()
	<-m.loopDone
	if m.fs != nil {
		if err := m.fs.Close(); err != nil {
			log.Printf("worktreewatch: close fs watcher: %v", err)
		}
	}
	m.dispatch.Wait()
}

func (m *Manager) forgetLocked(pw *projectWatch) {
	delete(m.projects, pw.key)
	pw.rerun = false
	pw.removed = nil
	for _, dir := range pw.watchedDirs() {
		keys := m.byDir[dir]
		delete(keys, pw.key)
		if len(keys) > 0 {
			continue
		}
		delete(m.byDir, dir)
		if m.fs != nil {
			// A directory that already vanished took its watch with it;
			// Remove reporting that is not a failure.
			if err := m.fs.Remove(dir); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
				log.Printf("worktreewatch: drop watch on %s: %v", dir, err)
			}
		}
	}
}

// watchedDirs lists the directories whose events concern this project: the
// common dir (where `worktrees/` itself appears and disappears), the registry
// (where registrations do) and the app's cut location (where the checkouts
// do).
func (pw *projectWatch) watchedDirs() []string {
	var dirs []string
	if pw.registryDir != "" {
		dirs = append(dirs, filepath.Dir(pw.registryDir), pw.registryDir)
	}
	if pw.extraDir != "" {
		dirs = append(dirs, pw.extraDir)
	}
	return dirs
}

// evaluateLocked re-arms the project's watches, reads its registry and
// reports a change. Caller holds m.mu.
func (m *Manager) evaluateLocked(pw *projectWatch) {
	if pw.registryDir == "" {
		// A root that was not a checkout when first seen can become one
		// (`git init` after the project row was created).
		if dir, ok := gitroot.RegistryDir(pw.root); ok {
			pw.registryDir = dir
		}
	}
	m.armWatchesLocked(pw)
	current, err := snapshot(pw.root)
	if err != nil {
		// A registry that exists and cannot be read says nothing about the
		// worktrees; reporting it as "none registered" would reattach every
		// thread of the project.
		log.Printf("worktreewatch: read registry of %s: %v", pw.key, err)
		return
	}
	if pw.initialized && slices.Equal(current, pw.last) {
		return
	}
	if pw.initialized {
		pw.noteRemovedLocked(current)
	}
	pw.last = current
	pw.initialized = true
	m.dispatchLocked(pw)
}

// armWatchesLocked (re)installs the project's directory watches. A watch
// dies with its directory (inotify reports IN_IGNORED and fsnotify forgets
// it), so re-adding on every read is what brings a recreated `worktrees/`
// back under watch; fsnotify treats adding a path it already watches as a
// no-op. A directory that does not exist yet is skipped; any other install
// failure switches the manager to polling for good, since the watches can
// no longer be trusted to be complete.
func (m *Manager) armWatchesLocked(pw *projectWatch) {
	if m.fs == nil {
		return
	}
	for _, dir := range pw.watchedDirs() {
		if err := m.fs.Add(dir); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if !m.polling {
				log.Printf("worktreewatch: fs watch on %s unavailable (%v); polling every %s", dir, err, m.cfg.PollInterval)
				m.polling = true
			}
			continue
		}
		keys := m.byDir[dir]
		if keys == nil {
			keys = make(map[string]struct{})
			m.byDir[dir] = keys
		}
		keys[pw.key] = struct{}{}
	}
}

func (m *Manager) dispatchLocked(pw *projectWatch) {
	if m.closed {
		return
	}
	if pw.running {
		pw.rerun = true
		return
	}
	pw.running = true
	m.dispatch.Add(1)
	go m.run(pw)
}

// noteRemovedLocked adds to the pending removals every path that was
// present at the previous read and is not present in current, and drops any
// pending path that is present again. Caller holds m.mu.
func (pw *projectWatch) noteRemovedLocked(current []Registration) {
	present := make(map[string]struct{}, len(current))
	for _, registration := range current {
		if registration.Present {
			present[registration.Path] = struct{}{}
			delete(pw.removed, registration.Path)
		}
	}
	for _, registration := range pw.last {
		if !registration.Present {
			continue
		}
		if _, ok := present[registration.Path]; ok {
			continue
		}
		if pw.removed == nil {
			pw.removed = make(map[string]struct{})
		}
		pw.removed[registration.Path] = struct{}{}
	}
}

// takeRemovedLocked returns the pending removals in path order and clears
// them. Caller holds m.mu.
func (pw *projectWatch) takeRemovedLocked() []string {
	if len(pw.removed) == 0 {
		return nil
	}
	paths := make([]string, 0, len(pw.removed))
	for path := range pw.removed {
		paths = append(paths, path)
	}
	pw.removed = nil
	sort.Strings(paths)
	return paths
}

func (m *Manager) run(pw *projectWatch) {
	defer m.dispatch.Done()
	m.mu.Lock()
	removed := pw.takeRemovedLocked()
	m.mu.Unlock()
	for {
		m.cfg.OnChange(pw.key, removed)
		m.mu.Lock()
		if pw.rerun && !m.closed {
			pw.rerun = false
			removed = pw.takeRemovedLocked()
			m.mu.Unlock()
			continue
		}
		pw.running = false
		m.mu.Unlock()
		return
	}
}

// snapshot reads the registrations of the repository at root and whether
// each recorded directory is present, sorted by path so two reads of one
// state compare equal.
func snapshot(root string) ([]Registration, error) {
	paths, err := gitroot.RegisteredWorktrees(root)
	if err != nil {
		return nil, err
	}
	registrations := make([]Registration, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		registrations = append(registrations, Registration{Path: path, Present: err == nil && info.IsDir()})
	}
	slices.SortFunc(registrations, func(a, b Registration) int { return strings.Compare(a.Path, b.Path) })
	return registrations, nil
}

// projectsForEvent resolves an event path to the projects watching the
// directory it landed in. The path is the entry the event names; its parent
// is a watched directory. An event naming a watched directory itself (the
// directory was removed or renamed) resolves through the same table.
func (m *Manager) projectsForEvent(name string) []string {
	clean := filepath.Clean(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for _, dir := range []string{filepath.Dir(clean), clean} {
		for key := range m.byDir[dir] {
			keys = append(keys, key)
		}
	}
	return keys
}

func (m *Manager) evaluateAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, pw := range m.projects {
		m.evaluateLocked(pw)
	}
}

func (m *Manager) evaluatePending() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.pending {
		if pw, ok := m.projects[key]; ok {
			m.evaluateLocked(pw)
		}
	}
	clear(m.pending)
}

func (m *Manager) loop() {
	defer close(m.loopDone)

	debounce := time.NewTimer(m.cfg.Debounce)
	if !debounce.Stop() {
		<-debounce.C
	}
	armed := false
	liveness := time.NewTicker(m.cfg.LivenessInterval)
	defer liveness.Stop()
	poll := time.NewTicker(m.cfg.PollInterval)
	defer poll.Stop()

	var events <-chan fsnotify.Event
	var errs <-chan error
	if m.fs != nil {
		events = m.fs.Events
		errs = m.fs.Errors
	}
	for {
		select {
		case <-m.done:
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			keys := m.projectsForEvent(ev.Name)
			if len(keys) == 0 {
				continue
			}
			for _, key := range keys {
				m.pending[key] = struct{}{}
			}
			if armed && !debounce.Stop() {
				<-debounce.C
			}
			debounce.Reset(m.cfg.Debounce)
			armed = true
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			log.Printf("worktreewatch: %v", err)
		case <-debounce.C:
			armed = false
			m.evaluatePending()
		case <-liveness.C:
			m.evaluateAll()
		case <-poll.C:
			if m.Polling() {
				m.evaluateAll()
			}
		}
	}
}
