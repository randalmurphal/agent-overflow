package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/eventchan"

	"github.com/fsnotify/fsnotify"
)

const workflowDefinitionsDebounce = 250 * time.Millisecond

type workflowDefinitionsWatcher struct {
	root      string
	debounce  time.Duration
	emit      func()
	watcher   *fsnotify.Watcher
	watched   map[string]struct{}
	done      chan struct{}
	closeOnce sync.Once
	waitGroup sync.WaitGroup
}

func newWorkflowDefinitionsWatcher(root string, debounce time.Duration, emit func()) (*workflowDefinitionsWatcher, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("workflow definitions watcher: root is required")
	}
	if debounce <= 0 {
		return nil, errors.New("workflow definitions watcher: debounce must be positive")
	}
	if emit == nil {
		return nil, errors.New("workflow definitions watcher: emit callback is required")
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("workflow definitions watcher: inspect root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workflow definitions watcher: root %q is not a directory", root)
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("workflow definitions watcher: create watcher: %w", err)
	}
	definitionsWatcher := &workflowDefinitionsWatcher{
		root: filepath.Clean(root), debounce: debounce, emit: emit, watcher: watcher,
		watched: make(map[string]struct{}), done: make(chan struct{}),
	}
	if err := definitionsWatcher.refreshWatches(); err != nil {
		return nil, errors.Join(err, watcher.Close())
	}
	definitionsWatcher.waitGroup.Add(1)
	go definitionsWatcher.loop()
	return definitionsWatcher, nil
}

func (a *App) startWorkflowDefinitionsWatcher(root string) {
	watcher, err := newWorkflowDefinitionsWatcher(root, workflowDefinitionsDebounce, func() {
		a.emit(eventchan.WorkflowDefinitionsChanged, nil)
	})
	if err != nil {
		log.Printf("workflow definitions watcher unavailable: %v", err)
		return
	}
	a.workflowDefinitionsWatcher = watcher
}

func (w *workflowDefinitionsWatcher) Close() error {
	var closeErr error
	w.closeOnce.Do(func() {
		close(w.done)
		closeErr = w.watcher.Close()
		w.waitGroup.Wait()
	})
	return closeErr
}

func (w *workflowDefinitionsWatcher) loop() {
	defer w.waitGroup.Done()
	var timer *time.Timer
	var timerChannel <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	queueEmit := func() {
		if timer == nil {
			timer = time.NewTimer(w.debounce)
			timerChannel = timer.C
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(w.debounce)
		timerChannel = timer.C
	}

	for {
		select {
		case <-w.done:
			return
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			// The watch on the root itself exists only to catch the creation of
			// the workflows/ and projects/ trees. Everything else in the data
			// root (SQLite WAL rotation, atomic settings renames) churns
			// constantly and must not trigger tree re-walks.
			if !w.inDefinitionTree(event.Name) {
				continue
			}
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				w.forgetWatchTree(event.Name)
			}
			if event.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
				if err := w.refreshWatches(); err != nil {
					log.Printf("workflow definitions watcher refresh: %v", err)
				}
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0 && w.relevant(event.Name) {
				queueEmit()
			}
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("workflow definitions watcher: %v", err)
		case <-timerChannel:
			timerChannel = nil
			w.emit()
		}
	}
}

func (w *workflowDefinitionsWatcher) refreshWatches() error {
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

func (w *workflowDefinitionsWatcher) addWorkflowTree(root string) error {
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

func (w *workflowDefinitionsWatcher) addWatch(path string) error {
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

func (w *workflowDefinitionsWatcher) forgetWatchTree(path string) {
	path = filepath.Clean(path)
	prefix := path + string(filepath.Separator)
	for watched := range w.watched {
		if watched == path || strings.HasPrefix(watched, prefix) {
			delete(w.watched, watched)
		}
	}
}

// inDefinitionTree reports whether path sits under the workflows/ or
// projects/ subtrees the watcher manages. Wider than relevant: it admits
// structural events (e.g. projects/ itself appearing) that warrant a watch
// refresh but not an emit.
func (w *workflowDefinitionsWatcher) inDefinitionTree(path string) bool {
	parts := w.rootRelativeParts(path)
	return parts != nil && (parts[0] == "workflows" || parts[0] == "projects")
}

func (w *workflowDefinitionsWatcher) relevant(path string) bool {
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

func (w *workflowDefinitionsWatcher) rootRelativeParts(path string) []string {
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
