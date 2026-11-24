package workflowdefs

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"agent-overflow/internal/fswatch"
)

// Watcher watches the shared workflow tree and every project-local workflow
// tree beneath one configuration root.
type Watcher struct {
	root    string
	watcher *fsnotify.Watcher
	// watched is the set of directories under watch. Only the loop
	// goroutine touches it after newWatcher returns: fswatch never calls
	// the handler concurrently.
	watched map[string]struct{}
	loop    *fswatch.Loop
}

// NewWatcher starts a recursive workflow-definition watcher. emit is called
// after a trailing-edge debounce whenever a relevant tree changes.
func NewWatcher(root string, debounce time.Duration, emit func()) (*Watcher, error) {
	return newWatcher(root, fswatch.Config{Debounce: debounce, Emit: emit}, fswatch.FromWatcher)
}

// newWatcher arms the watches and starts the loop. config carries the
// debounce and emit (and, in tests, a held clock); its label is set here.
// source turns the armed fsnotify watcher into what the loop reads:
// fswatch.FromWatcher in production, a relay in tests.
func newWatcher(root string, config fswatch.Config, source func(*fsnotify.Watcher) fswatch.Source) (*Watcher, error) {
	config.Label = "workflow definitions watcher"
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("workflow definitions watcher: root is required")
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("workflow definitions watcher: inspect root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workflow definitions watcher: root %q is not a directory", root)
	}
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("workflow definitions watcher: create watcher: %w", err)
	}
	definitionsWatcher := &Watcher{root: filepath.Clean(root), watcher: fsWatcher, watched: make(map[string]struct{})}
	if err := definitionsWatcher.refreshWatches(); err != nil {
		return nil, errors.Join(err, fsWatcher.Close())
	}
	if definitionsWatcher.loop, err = fswatch.Start(config, source(fsWatcher), definitionsWatcher.handleEvent); err != nil {
		return nil, err
	}
	return definitionsWatcher, nil
}

// Close stops the watcher. It is safe to call more than once.
func (w *Watcher) Close() error { return w.loop.Close() }

// handleEvent keeps the watches in step with one event and reports whether
// it changed a definition tree.
func (w *Watcher) handleEvent(event fsnotify.Event, _ <-chan struct{}) bool {
	// The root watch exists only to discover the workflows/ and projects/
	// trees. Database and settings churn at the root is irrelevant.
	if !w.inDefinitionTree(event.Name) {
		return false
	}
	if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		w.forgetWatchTree(event.Name)
	}
	if event.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
		if err := w.refreshWatches(); err != nil {
			log.Printf("workflow definitions watcher refresh: %v", err)
		}
	}
	return w.relevant(event.Name)
}

func (w *Watcher) refreshWatches() error {
	if err := w.addWatch(w.root); err != nil {
		return err
	}
	if err := w.addWorkflowTree(filepath.Join(w.root, "workflows")); err != nil {
		return err
	}
	projectsRoot := filepath.Join(w.root, "projects")
	exists, err := directoryExists(projectsRoot)
	if err != nil || !exists {
		return err
	}
	if err := w.addWatch(projectsRoot); err != nil {
		return err
	}
	entries, err := os.ReadDir(projectsRoot)
	if err != nil {
		return fmt.Errorf("workflow definitions watcher: list projects: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projectRoot := filepath.Join(projectsRoot, entry.Name())
		if err := w.addWatch(projectRoot); err != nil {
			return err
		}
		if err := w.addWorkflowTree(filepath.Join(projectRoot, "workflows")); err != nil {
			return err
		}
	}
	return nil
}

func (w *Watcher) addWorkflowTree(root string) error {
	exists, err := directoryExists(root)
	if err != nil || !exists {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("workflow definitions watcher: walk %s: %w", path, walkErr)
		}
		if entry.IsDir() {
			return w.addWatch(path)
		}
		return nil
	})
}

func (w *Watcher) addWatch(path string) error {
	path = filepath.Clean(path)
	if _, exists := w.watched[path]; exists {
		return nil
	}
	if err := w.watcher.Add(path); err != nil {
		return fmt.Errorf("workflow definitions watcher: watch %s: %w", path, err)
	}
	w.watched[path] = struct{}{}
	return nil
}

func (w *Watcher) forgetWatchTree(path string) {
	path = filepath.Clean(path)
	prefix := path + string(filepath.Separator)
	for watched := range w.watched {
		if watched == path || strings.HasPrefix(watched, prefix) {
			delete(w.watched, watched)
		}
	}
}

func (w *Watcher) inDefinitionTree(path string) bool {
	parts := w.rootRelativeParts(path)
	return parts != nil && (parts[0] == "workflows" || parts[0] == "projects")
}

func (w *Watcher) relevant(path string) bool {
	parts := w.rootRelativeParts(path)
	if parts == nil {
		return false
	}
	if parts[0] == "workflows" {
		return true
	}
	if len(parts) >= 2 && parts[0] == "projects" {
		return len(parts) == 2 || len(parts) >= 3 && parts[2] == "workflows"
	}
	return false
}

func (w *Watcher) rootRelativeParts(path string) []string {
	relative, err := filepath.Rel(w.root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil
	}
	return strings.Split(relative, string(filepath.Separator))
}

func directoryExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("workflow definitions watcher: inspect %s: %w", path, err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("workflow definitions watcher: expected directory at %s", path)
	}
	return true, nil
}
