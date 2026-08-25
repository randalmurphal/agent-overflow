package storetest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/store"
)

// templatePath holds the migrated template built by Run. Written once before
// m.Run and read from every test goroutine, so it is an atomic rather than a
// plain package var.
var templatePath atomic.Pointer[string]

// Run builds one migrated template database, runs the package's tests against
// it, and removes the template directory. Use it as the whole body of TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }
func Run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "agent-overflow-storetest-template-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "storetest: create template directory: %v\n", err)
		return 1
	}
	path := filepath.Join(dir, "template.sqlite")
	if err := buildTemplate(path); err != nil {
		fmt.Fprintf(os.Stderr, "storetest: build template: %v\n", err)
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			fmt.Fprintf(os.Stderr, "storetest: remove failed template: %v\n", removeErr)
		}
		return 1
	}
	templatePath.Store(&path)

	code := m.Run()

	templatePath.Store(nil)
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(os.Stderr, "storetest: remove template: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
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
		if err := s.Close(); err != nil {
			tb.Errorf("storetest: close clone: %v", err)
		}
	})
	return s
}

// ClonePath copies the migrated template into tb.TempDir and returns the path
// without opening it, for tests that open (or reopen) the file themselves.
func ClonePath(tb testing.TB) string {
	tb.Helper()
	source := templatePath.Load()
	if source == nil {
		tb.Fatalf("storetest: no template; add to this package:\n\nfunc TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }")
	}
	path := filepath.Join(tb.TempDir(), "store.sqlite")
	if err := copyTemplate(*source, path); err != nil {
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
