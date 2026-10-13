package skillsinstall

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

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
// or files the manifest does not name, or into a link or file where one of
// its directories should be.
type ConflictError struct {
	Paths []string
}

func (e *ConflictError) Error() string {
	return "these skills are already in the home and were not installed by Agent Overflow: " +
		strings.Join(e.Paths, ", ") + ". Nothing was installed."
}

// GetStatus reads what home holds of tree. It waits for a change running
// on the home, so it never reports one part way.
func GetStatus(home string, tree Tree) (status Status, err error) {
	unlock, found, err := lockHome(home, false, true)
	if err != nil || !found {
		return Status{}, err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	root, found, err := openSkills(home, false)
	if err != nil || !found {
		return Status{}, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	m, err := readManifest(root)
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
// not name, or a link or file where a directory it writes into should be,
// refuses the whole install before anything is written.
func Install(home string, tree Tree) (err error) {
	unlock, _, err := lockHome(home, true, false)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	// A skills directory this creates holds nothing, so nothing in it can
	// refuse the install.
	root, _, err := openSkills(home, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	previous, err := readManifest(root)
	if err != nil {
		return err
	}
	if previous != nil && previous.Complete && previous.Digest == tree.Digest {
		return nil
	}
	return apply(root, tree, previous)
}

// Update replaces what the manifest names with tree, deleting the files the
// manifest names that tree no longer has. Refused for a home with no install.
func Update(home string, tree Tree) (err error) {
	unlock, found, err := lockHome(home, false, false)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotInstalled
	}
	defer func() { err = errors.Join(err, unlock()) }()
	root, found, err := openSkills(home, false)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotInstalled
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	previous, err := readManifest(root)
	if err != nil {
		return err
	}
	if previous == nil {
		return ErrNotInstalled
	}
	return apply(root, tree, previous)
}

func apply(root *os.Root, tree Tree, previous *manifest) error {
	if previous == nil {
		previous = &manifest{}
	}
	if err := conflicts(root, tree, setOf(previous.Skills), setOf(previous.Files)); err != nil {
		return err
	}
	newFiles := tree.Files()
	// Before the first write, the manifest names everything this run may
	// leave behind, so a failure part way leaves nothing it cannot remove.
	pending := manifest{
		Version: tree.Version, Commit: tree.Commit, Digest: tree.Digest,
		Skills: union(previous.Skills, tree.Skills),
		Files:  union(previous.Files, newFiles),
	}
	if err := writeManifest(root, pending); err != nil {
		return err
	}
	for _, file := range tree.files {
		if err := copyFile(root, tree.fsys, file); err != nil {
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
	if failed, err := removeFiles(root, stale); err != nil {
		pending.Files = union(newFiles, failed)
		return errors.Join(err, writeManifest(root, pending))
	}
	pruneDirs(root, previous.Skills, stale)
	return writeManifest(root, manifest{
		Version: tree.Version, Commit: tree.Commit, Digest: tree.Digest,
		Complete: true, Skills: tree.Skills, Files: newFiles,
	})
}

// conflicts refuses what is in the way: a skill directory the previous
// manifest does not own, a file it does not name inside one it does, and a
// link or file where a directory of an owned skill should be. The root
// would follow a link that stays inside it, and the manifest's names would
// then no longer each mean one file.
func conflicts(root *os.Root, tree Tree, ownedSkills, ownedFiles map[string]bool) error {
	var found []string
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			found = append(found, rootPath(root, name))
		}
	}
	for _, skill := range tree.Skills {
		if ownedSkills[skill] {
			continue
		}
		if _, err := root.Lstat(skill); err == nil {
			add(skill)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for _, file := range tree.files {
		if !ownedSkills[skillOf(file.path)] {
			continue
		}
		blocked, err := blockedAt(root, file.path)
		if err != nil {
			return err
		}
		if blocked != "" {
			add(blocked)
			continue
		}
		if ownedFiles[file.path] {
			continue
		}
		if _, err := root.Lstat(filepath.FromSlash(file.path)); err == nil {
			add(file.path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if len(found) > 0 {
		return &ConflictError{Paths: found}
	}
	return nil
}

// Remove deletes what home's manifest names: its files, then the
// directories they leave empty, then the manifest. A directory still holding
// a file the manifest does not name stays. A file that cannot be deleted is
// reported, and the manifest keeps naming it so a later Remove finishes.
func Remove(home string) (err error) {
	unlock, found, err := lockHome(home, false, false)
	if err != nil || !found {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	root, found, err := openSkills(home, false)
	if err != nil || !found {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	m, err := readManifest(root)
	if err != nil || m == nil {
		return err
	}
	failed, err := removeFiles(root, m.Files)
	if err != nil {
		m.Complete = false
		m.Files = failed
		return errors.Join(err, writeManifest(root, *m))
	}
	pruneDirs(root, m.Skills, m.Files)
	if err := root.Remove(ManifestName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the skills manifest: %w", err)
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
