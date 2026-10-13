package skillsinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// ManifestName is the file in `<home>/skills` that records an install. It
// lives in the provider home, so every app profile sharing that home sees
// the same install.
const ManifestName = ".agent-overflow-pstack.json"

type manifest struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Digest  string `json:"digest"`
	// Complete is false while an install or update is writing, and stays
	// false when one failed: Skills and Files then name everything it may
	// have written, so Remove and the next Install still find it.
	Complete bool     `json:"complete"`
	Skills   []string `json:"skills"`
	Files    []string `json:"files"`
}

// Status is what a provider home holds of the tree.
type Status struct {
	Installed bool   `json:"installed"`
	Complete  bool   `json:"complete"`
	Current   bool   `json:"current"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
}

// ErrNotInstalled refuses an Update of a home with no install.
var ErrNotInstalled = errors.New("the skills are not installed here")

// ConflictError refuses an install that would write over skill directories
// or files the manifest does not name.
type ConflictError struct {
	Paths []string
}

func (e *ConflictError) Error() string {
	return "these skills are already in the home and were not installed by Agent Overflow: " +
		strings.Join(e.Paths, ", ") + ". Nothing was installed."
}

// skillsDir is where both providers look for skills, one level down.
func skillsDir(home string) string { return filepath.Join(home, "skills") }

func manifestPath(home string) string { return filepath.Join(skillsDir(home), ManifestName) }

// readManifest returns the home's manifest, or nil when there is none.
func readManifest(home string) (*manifest, error) {
	raw, err := os.ReadFile(manifestPath(home))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the skills manifest: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("read the skills manifest %s: %w", manifestPath(home), err)
	}
	return &m, nil
}

func writeManifest(home string, m manifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(manifestPath(home), raw, 0o644); err != nil {
		return fmt.Errorf("write the skills manifest: %w", err)
	}
	return nil
}

// GetStatus reads what home holds of tree.
func GetStatus(home string, tree Tree) (Status, error) {
	m, err := readManifest(home)
	if err != nil || m == nil {
		return Status{}, err
	}
	return Status{
		Installed: true,
		Complete:  m.Complete,
		Current:   m.Complete && m.Digest == tree.Digest,
		Version:   m.Version,
		Commit:    m.Commit,
	}, nil
}

// Install copies tree into home. A home that already holds a complete,
// current install is left as it is; one that holds another or an
// unfinished install is updated. A skill directory or file the manifest does
// not name refuses the whole install before anything is written.
func Install(home string, tree Tree) error {
	previous, err := readManifest(home)
	if err != nil {
		return err
	}
	if previous != nil && previous.Complete && previous.Digest == tree.Digest {
		return nil
	}
	return apply(home, tree, previous)
}

// Update replaces what the manifest names with tree, deleting the files the
// manifest names that tree no longer has. Refused for a home with no install.
func Update(home string, tree Tree) error {
	previous, err := readManifest(home)
	if err != nil {
		return err
	}
	if previous == nil {
		return ErrNotInstalled
	}
	return apply(home, tree, previous)
}

func apply(home string, tree Tree, previous *manifest) error {
	if previous == nil {
		previous = &manifest{}
	}
	ownedSkills := setOf(previous.Skills)
	ownedFiles := setOf(previous.Files)
	dir := skillsDir(home)
	if err := conflicts(dir, tree, ownedSkills, ownedFiles); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	newFiles := tree.Files()
	// Before the first write, the manifest names everything this run may
	// leave behind, so a failure part way leaves nothing it cannot remove.
	pending := manifest{
		Version: tree.Version, Commit: tree.Commit, Digest: tree.Digest,
		Skills: union(previous.Skills, tree.Skills),
		Files:  union(previous.Files, newFiles),
	}
	if err := writeManifest(home, pending); err != nil {
		return err
	}
	for _, file := range tree.files {
		if err := copyFile(tree.fsys, file, dir); err != nil {
			return fmt.Errorf("install %s: %w", file.path, err)
		}
	}
	keep := setOf(newFiles)
	var stale []string
	for _, name := range previous.Files {
		if !keep[name] {
			stale = append(stale, name)
		}
	}
	if failed, err := removeFiles(dir, stale); err != nil {
		pending.Files = union(newFiles, failed)
		return errors.Join(err, writeManifest(home, pending))
	}
	pruneDirs(dir, previous.Skills, stale)
	return writeManifest(home, manifest{
		Version: tree.Version, Commit: tree.Commit, Digest: tree.Digest,
		Complete: true, Skills: tree.Skills, Files: newFiles,
	})
}

// conflicts refuses a skill directory or file in the way that the previous
// manifest does not name.
func conflicts(dir string, tree Tree, ownedSkills, ownedFiles map[string]bool) error {
	// Stat, not Lstat: a skills directory linked elsewhere is the user's
	// choice of where skills live, and the install goes there.
	if info, err := os.Stat(dir); err == nil && !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var found []string
	for _, skill := range tree.Skills {
		if ownedSkills[skill] {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, skill)); err == nil {
			found = append(found, filepath.Join(dir, skill))
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for _, file := range tree.files {
		if !ownedSkills[skillOf(file.path)] || ownedFiles[file.path] {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(file.path))
		if _, err := os.Lstat(target); err == nil {
			found = append(found, target)
		} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return err
		}
	}
	if len(found) > 0 {
		return &ConflictError{Paths: found}
	}
	return nil
}

func copyFile(fsys fs.FS, file treeFile, dir string) error {
	target := filepath.Join(dir, filepath.FromSlash(file.path))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	source, err := fsys.Open(file.path)
	if err != nil {
		return err
	}
	defer source.Close()
	mode := fs.FileMode(0o644)
	if file.executable {
		mode = 0o755
	}
	return writeAtomic(target, source, mode)
}

// Remove deletes what home's manifest names: its files, then the
// directories they leave empty, then the manifest. A directory still holding
// a file the manifest does not name stays. A file that cannot be deleted is
// reported, and the manifest keeps naming it so a later Remove finishes.
func Remove(home string) error {
	m, err := readManifest(home)
	if err != nil || m == nil {
		return err
	}
	dir := skillsDir(home)
	failed, err := removeFiles(dir, m.Files)
	if err != nil {
		m.Complete = false
		m.Files = failed
		return errors.Join(err, writeManifest(home, *m))
	}
	pruneDirs(dir, m.Skills, m.Files)
	if err := os.Remove(manifestPath(home)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the skills manifest: %w", err)
	}
	return nil
}

// removeFiles deletes each named file under dir, returning the ones that
// could not be deleted. A file already gone is not a failure.
func removeFiles(dir string, names []string) ([]string, error) {
	var failed []string
	var errs []error
	for _, name := range names {
		err := os.Remove(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			failed = append(failed, name)
			errs = append(errs, err)
		}
	}
	return failed, errors.Join(errs...)
}

// pruneDirs removes, deepest first, the directories the named files sat in
// and the named skill directories, each only when it is empty.
func pruneDirs(dir string, skills, files []string) {
	seen := map[string]bool{}
	var dirs []string
	for _, name := range files {
		for _, parent := range parentDirs(name) {
			if !seen[parent] {
				seen[parent] = true
				dirs = append(dirs, parent)
			}
		}
	}
	for _, skill := range skills {
		if !seen[skill] {
			seen[skill] = true
			dirs = append(dirs, skill)
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], "/") > strings.Count(dirs[j], "/")
	})
	for _, name := range dirs {
		// A directory that is not empty holds something the manifest does
		// not name, and stays; one already gone needs nothing.
		_ = os.Remove(filepath.Join(dir, filepath.FromSlash(name)))
	}
}

func writeFileAtomic(target string, data []byte, mode fs.FileMode) error {
	return writeAtomic(target, bytes.NewReader(data), mode)
}

// writeAtomic writes beside target and renames into place, so a reader never
// sees half a file and a link at target is replaced, not written through.
func writeAtomic(target string, source io.Reader, mode fs.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(target), ".ao-skill-*")
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(temp, source)
	closeErr := temp.Close()
	if err := errors.Join(copyErr, closeErr, os.Chmod(temp.Name(), mode)); err != nil {
		return errors.Join(err, os.Remove(temp.Name()))
	}
	if err := os.Rename(temp.Name(), target); err != nil {
		return errors.Join(err, os.Remove(temp.Name()))
	}
	return nil
}

func setOf(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}

func union(a, b []string) []string {
	seen := setOf(a)
	out := append([]string(nil), a...)
	for _, value := range b {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}
