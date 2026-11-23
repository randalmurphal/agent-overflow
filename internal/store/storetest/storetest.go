package storetest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"agent-overflow/internal/ownedtemp"
	"agent-overflow/internal/store"
)

// template is the migrated database the clones copy, built by the first
// clone of a run. One run, Run's, owns it at a time.
var template struct {
	mu sync.Mutex
	// running is set while Run's m.Run is.
	running bool
	// path is the built template, release removes its directory, and err
	// is the failed build every later clone reports.
	path    string
	release func() error
	err     error
}

// templatePrefix names the template directories, one per test process.
const templatePrefix = "agent-overflow-storetest-template-"

// Run runs the package's tests, removes the template directory the first
// clone built, and removes those of test processes that exited without
// removing theirs (ownedtemp): killed, timed out, or a helper process that
// ended in os.Exit. A run with no clone, such as a helper process the
// package's tests start, migrates nothing. Use it as the whole body of
// TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }
func Run(m *testing.M) int {
	sweepTemplates()
	template.mu.Lock()
	template.running = true
	template.mu.Unlock()

	code := m.Run()

	template.mu.Lock()
	template.running = false
	release := template.release
	template.path, template.release, template.err = "", nil, nil
	template.mu.Unlock()
	if release != nil {
		if err := release(); err != nil {
			fmt.Fprintf(os.Stderr, "storetest: remove template: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}
	sweepTemplates()
	return code
}

// templateFile is the template's path, built on the first call of a run.
func templateFile(tb testing.TB) string {
	tb.Helper()
	template.mu.Lock()
	defer template.mu.Unlock()
	if !template.running {
		tb.Fatalf("storetest: no template; add to this package:\n\nfunc TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }")
	}
	if template.path == "" && template.err == nil {
		template.path, template.release, template.err = makeTemplate()
	}
	if template.err != nil {
		tb.Fatalf("storetest: %v", template.err)
	}
	return template.path
}

// makeTemplate builds the template in a directory of its own.
func makeTemplate() (path string, release func() error, err error) {
	dir, release, err := ownedtemp.Make("", templatePrefix)
	if err != nil {
		return "", nil, fmt.Errorf("create template directory: %w", err)
	}
	path = filepath.Join(dir, "template.sqlite")
	if err := buildTemplate(path); err != nil {
		if removeErr := release(); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove failed template: %w", removeErr))
		}
		return "", nil, fmt.Errorf("build template: %w", err)
	}
	return path, release, nil
}

// sweepTemplates removes the template directories of exited test
// processes. A failure leaves a directory, not a wrong result, so it is
// reported rather than failing the run.
func sweepTemplates() {
	if err := ownedtemp.Sweep("", templatePrefix); err != nil {
		fmt.Fprintf(os.Stderr, "storetest: remove the templates of exited test processes: %v\n", err)
	}
}

// buildTemplate opens a store at path (which applies the migration chain) and
// closes it. Store.Close runs a TRUNCATE checkpoint, so the WAL is merged back
// into the main file and the template is a single copyable file; the assertion
// below is what keeps that true if Close ever stops checkpointing.
func buildTemplate(path string) error {
	template, err := store.New(path)
	if err != nil {
		return fmt.Errorf("new store: %w", err)
	}
	if err := template.Close(); err != nil {
		return fmt.Errorf("close template: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		info, statErr := os.Stat(path + suffix)
		if statErr == nil && info.Size() > 0 {
			return fmt.Errorf("template %s survived Close with %d bytes: the clone copy would lose it", suffix, info.Size())
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("stat template %s: %w", suffix, statErr)
		}
	}
	return nil
}

// Clone copies the migrated template into tb.TempDir, opens it, and closes it
// on cleanup. The clone is bare-migrated: no project, no thread, no items.
func Clone(tb testing.TB) *store.Store {
	tb.Helper()
	path := ClonePath(tb)
	s, err := store.New(path)
	if err != nil {
		tb.Fatalf("storetest: open clone: %v", err)
	}
	tb.Cleanup(func() {
		// No writer may store a meta it read back: a read serves the
		// subagent cards in the meta, and a stored copy would freeze one.
		rows, err := s.RowsStoringServedSubagentKeys(10)
		if closeErr := s.Close(); closeErr != nil {
			tb.Errorf("storetest: close clone: %v", closeErr)
		}
		if err != nil {
			// The test closed the store: check the file.
			rows, err = rowsStoringServedSubagentKeysAt(path)
		}
		if err != nil {
			tb.Errorf("storetest: check stored served subagent keys: %v", err)
		} else if len(rows) > 0 {
			tb.Errorf("storetest: stored meta holds served subagent keys: %v", rows)
		}
	})
	return s
}

// WithParentCard runs write with item carrying the card of its parent,
// as the store requires of a visible row with a parent written outside a
// bulk writer (store.OpenSubagentCard). A top-level row is written as it
// is. The card is closed after write, which flushes it. For fixtures that
// seed rows straight to the store.
func WithParentCard(s *store.Store, item store.Item, write func(store.Item) error) error {
	if item.ParentID == "" {
		return write(item)
	}
	return s.WithSubagentCard(item.ThreadID, item.ParentID, func(card *store.SubagentCard) error {
		item.SubagentCard = card
		return write(item)
	})
}

func rowsStoringServedSubagentKeysAt(path string) ([]string, error) {
	s, err := store.New(path)
	if err != nil {
		return nil, fmt.Errorf("reopen clone: %w", err)
	}
	rows, err := s.RowsStoringServedSubagentKeys(10)
	return rows, errors.Join(err, s.Close())
}

// ClonePath copies the migrated template into tb.TempDir and returns the path
// without opening it, for tests that open (or reopen) the file themselves.
func ClonePath(tb testing.TB) string {
	tb.Helper()
	source := templateFile(tb)
	path := filepath.Join(tb.TempDir(), "store.sqlite")
	if err := copyTemplate(source, path); err != nil {
		tb.Fatalf("storetest: clone template: %v", err)
	}
	return path
}

func copyTemplate(templatePath, destination string) error {
	source, err := os.Open(templatePath)
	if err != nil {
		return fmt.Errorf("open template: %w", err)
	}
	clone, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.Join(fmt.Errorf("create clone: %w", err), source.Close())
	}
	_, copyErr := io.Copy(clone, source)
	sourceCloseErr := source.Close()
	cloneCloseErr := clone.Close()
	if copyErr != nil {
		copyErr = fmt.Errorf("copy template: %w", copyErr)
	}
	if sourceCloseErr != nil {
		sourceCloseErr = fmt.Errorf("close template: %w", sourceCloseErr)
	}
	if cloneCloseErr != nil {
		cloneCloseErr = fmt.Errorf("close clone: %w", cloneCloseErr)
	}
	return errors.Join(copyErr, sourceCloseErr, cloneCloseErr)
}
