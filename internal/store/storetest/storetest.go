package storetest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/fixturedir"
)

// template is the migrated database the clones copy, built by the first
// clone of a run in the run's fixture directory.
var template = fixturedir.New("storetest-template.sqlite", func(tb testing.TB, path string) {
	if err := buildTemplate(path); err != nil {
		tb.Fatalf("storetest: build template: %v", err)
	}
})

// Run runs the package's tests with the fixture directory the template is
// built in (fixturedir.Guard). A run with no clone, such as a helper
// process the package's tests start, migrates nothing. Use it as the whole
// body of TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }
func Run(m *testing.M) int { return fixturedir.Guard(m.Run) }

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
	source := template.Path(tb)
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
