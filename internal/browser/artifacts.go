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

// The artifact root is opened once per Manager and held (artifactDir), and
// the quota's measurement and every write the Manager makes in it go
// through the held directory (artifactsIn, openArtifactDir): what is
// measured is where the Manager writes, whatever the root's name, or a
// directory's in it, names later. A link in it leaving it is followed by
// nothing that goes through it. A path handed out in it, to a caller or
// to Chromium, is checked to reach what is held when it is handed out
// (nameReaches). Headless Chromium alone writes in it by name: a running
// download into its room, made through the held root, by the path checked
// at its launch (openDownloadRoom), and the Manager reads the finished
// file only through the held root, to link it into a workspace's
// directory there (placeArtifact).

// errArtifactsClosed refuses artifact work once the Manager has closed.
var errArtifactsClosed = errors.New("browser: the artifact folder is closed")

// prepareArtifacts holds the artifact root (holdArtifactRoot) and measures,
// once per Manager, what it holds into the artifact quota, removing the
// artifacts older than artifactMaxAge as it goes. A scan it cannot finish
// answers the error and measures nothing, so every reservation is refused
// with the reason until a later call measures it all. An expired artifact
// it cannot remove stays charged and is logged; one gone meanwhile is not
// charged. Chromium writes by name and follows a link, so a link in the
// root to a directory, or out of it, is refused: what the quota measures is
// where the artifacts are written.
func (m *Manager) prepareArtifacts() error {
	m.artifactInitMu.Lock()
	defer m.artifactInitMu.Unlock()
	if m.artifactReady {
		return nil
	}
	root, err := m.holdArtifactRootLocked()
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-artifactMaxAge)
	var total int64
	entries := 0
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
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
		rel := filepath.FromSlash(name)
		path := filepath.Join(m.artifactRoot, rel)
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := root.Stat(rel)
			switch {
			case errors.Is(err, fs.ErrNotExist):
			case err != nil:
				return fmt.Errorf("%s is a link the artifact quota cannot follow: %w", path, err)
			case target.IsDir():
				return fmt.Errorf("%s is a link to a folder, whose size the artifact quota cannot measure", path)
			}
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			err := root.Remove(rel)
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

// holdArtifactRootLocked answers the held artifact root, opening it the
// first time: made if it is missing, refused while its name is a link, and
// held only once the directory opened is the one its name named. The caller
// holds artifactInitMu.
func (m *Manager) holdArtifactRootLocked() (*os.Root, error) {
	if m.artifactsClosed {
		return nil, errArtifactsClosed
	}
	if root := m.artifactDir.Load(); root != nil {
		return root, nil
	}
	if err := os.MkdirAll(m.artifactRoot, 0o700); err != nil {
		return nil, fmt.Errorf("browser: create the artifact folder: %w", err)
	}
	named, err := os.Lstat(m.artifactRoot)
	if err != nil {
		return nil, fmt.Errorf("browser: the artifact folder: %w", err)
	}
	if !named.IsDir() {
		return nil, fmt.Errorf("browser: the artifact folder %s is a link, not a folder, so its size cannot be measured and nothing is kept there", m.artifactRoot)
	}
	root, err := os.OpenRoot(m.artifactRoot)
	if err != nil {
		return nil, fmt.Errorf("browser: open the artifact folder: %w", err)
	}
	opened, err := root.Stat(".")
	if err == nil && !os.SameFile(named, opened) {
		err = fmt.Errorf("%s changed as it was opened", m.artifactRoot)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("browser: open the artifact folder: %w", err), root.Close())
	}
	m.artifactDir.Store(root)
	return root, nil
}

// artifactsIn answers the held artifact root and path, a name inside the
// artifact root, relative to it. It refuses a path outside the root, and
// answers why the root is not held when it is not.
func (m *Manager) artifactsIn(path string) (*os.Root, string, error) {
	rel, err := filepath.Rel(m.artifactRoot, path)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, "", fmt.Errorf("browser: %s is outside the artifact folder", path)
	}
	if root := m.artifactDir.Load(); root != nil {
		return root, rel, nil
	}
	m.artifactInitMu.Lock()
	defer m.artifactInitMu.Unlock()
	root, err := m.holdArtifactRootLocked()
	if err != nil {
		return nil, "", err
	}
	return root, rel, nil
}

// openArtifactDir makes dir, a directory inside the artifact root, if it
// is missing, and opens it, both through the held root.
func (m *Manager) openArtifactDir(dir string) (*os.Root, error) {
	root, rel, err := m.artifactsIn(dir)
	if err != nil {
		return nil, err
	}
	if err := root.MkdirAll(rel, 0o700); err != nil {
		return nil, fmt.Errorf("browser: create %s: %w", dir, err)
	}
	held, err := root.OpenRoot(rel)
	if err != nil {
		return nil, fmt.Errorf("browser: open %s: %w", dir, err)
	}
	return held, nil
}

// makeArtifactDir makes dir, a directory inside the artifact root, if it
// is missing, through the held root.
func (m *Manager) makeArtifactDir(dir string) error {
	root, rel, err := m.artifactsIn(dir)
	if err != nil {
		return err
	}
	if err := root.MkdirAll(rel, 0o700); err != nil {
		return fmt.Errorf("browser: create %s: %w", dir, err)
	}
	return nil
}

// removeArtifactDir removes dir, a directory inside the artifact root, and
// everything in it, through the held root.
func (m *Manager) removeArtifactDir(dir string) error {
	root, rel, err := m.artifactsIn(dir)
	if err != nil {
		return err
	}
	if err := root.RemoveAll(rel); err != nil {
		return fmt.Errorf("browser: remove %s: %w", dir, err)
	}
	return nil
}

// nameReaches answers nil when path, a name AO hands to a caller or to
// Chromium for a file or folder it holds, reaches held, that file or
// folder as read through its handle; otherwise why it does not, so the
// name is not handed out. A name that reaches another file or folder
// would send its reader to something AO neither wrote nor measures: the
// artifact folder's name, or a folder's in it, was moved since it was
// held.
func nameReaches(path string, held os.FileInfo) error {
	named, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("browser: %s no longer reaches what AO wrote there: %w", path, err)
	}
	if !os.SameFile(named, held) {
		return fmt.Errorf("browser: %s names another file or folder than the one AO wrote there, so it is not handed out", path)
	}
	return nil
}

// closeArtifacts lets the held artifact root go, once nothing writes in it;
// nothing opens it again.
func (m *Manager) closeArtifacts() error {
	m.artifactInitMu.Lock()
	defer m.artifactInitMu.Unlock()
	m.artifactsClosed = true
	if root := m.artifactDir.Swap(nil); root != nil {
		if err := root.Close(); err != nil {
			return fmt.Errorf("browser: close the artifact folder: %w", err)
		}
	}
	return nil
}

// closeArtifactDir closes a directory openArtifactDir opened, logging a
// failure: the work in it is done by then.
func closeArtifactDir(dir *os.Root, name string) {
	if err := dir.Close(); err != nil {
		log.Printf("browser: close %s: %v", name, err)
	}
}

// reserveArtifacts holds bytes of the artifact quota, measured on first use
// (prepareArtifacts), and of any pools besides it, all or none.
func (m *Manager) reserveArtifacts(bytes int64, pools ...*quota.Pool) (*quota.Reservation, error) {
	if err := m.prepareArtifacts(); err != nil {
		return nil, err
	}
	return quota.Reserve(bytes, append(pools, m.artifactBytes)...)
}
