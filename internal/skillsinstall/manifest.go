package skillsinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	// have written, so Update and Remove still find it.
	Complete bool     `json:"complete"`
	Skills   []string `json:"skills"`
	Files    []string `json:"files"`
}

// LockName is the file in the provider home whose advisory lock every
// Install, Update and Remove holds exclusively, and a status read shared,
// from reading the manifest to writing it, so two processes sharing a home
// (two app profiles, two versions) cannot interleave their copies and a read
// never sees one part way. It sits beside `skills/`, not in it, so a refused
// install writes nothing there. It stays: removing a lock file races its
// users.
const LockName = ".agent-overflow-pstack-skills.lock"

// skillsDir is where both providers look for skills, one level down.
func skillsDir(home string) string { return filepath.Join(home, "skills") }

// readManifest returns the manifest in the skills root, or nil when there
// is none.
func readManifest(root *os.Root) (*manifest, error) {
	raw, err := root.ReadFile(ManifestName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the skills manifest: %w", err)
	}
	name := filepath.Join(root.Name(), ManifestName)
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("read the skills manifest %s: %w", name, err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("read the skills manifest %s: %w", name, err)
	}
	return &m, nil
}

// validate refuses a manifest that names anything outside its own skill
// directories. The manifest is shared input on disk (other profiles, other
// app versions, the user), and Update and Remove delete what it names.
func (m manifest) validate() error {
	skills := make(map[string]bool, len(m.Skills))
	for _, skill := range m.Skills {
		if !fs.ValidPath(skill) || skill == "." || strings.Contains(skill, "/") || strings.HasPrefix(skill, ".") {
			return fmt.Errorf("it names %q, which is not a skill directory", skill)
		}
		skills[skill] = true
	}
	for _, file := range m.Files {
		if !fs.ValidPath(file) || !strings.Contains(file, "/") || !skills[skillOf(file)] {
			return fmt.Errorf("it names %q, which is not a file in one of its skill directories", file)
		}
	}
	return nil
}

func writeManifest(root *os.Root, m manifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(root, ManifestName, bytes.NewReader(raw), 0o644); err != nil {
		return fmt.Errorf("write the skills manifest: %w", err)
	}
	return nil
}

// lockHome takes home's skills lock, shared when shared is set. With create
// it makes the home and the lock file as needed. Without it, found is false
// and no lock is taken when there is no lock file and no manifest: nothing
// was installed, and a read leaves no file in a home it only looked at. An
// install creates the lock file before it writes, so a racing one is
// ordered after this.
func lockHome(home string, create, shared bool) (unlock func() error, found bool, err error) {
	name := filepath.Join(home, LockName)
	if create {
		if err := os.MkdirAll(home, 0o755); err != nil {
			return nil, false, fmt.Errorf("create %s: %w", home, err)
		}
	}
	file, err := os.OpenFile(name, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			if _, statErr := os.Lstat(filepath.Join(skillsDir(home), ManifestName)); errors.Is(statErr, fs.ErrNotExist) {
				return nil, false, nil
			}
		}
		file, err = os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o644)
	}
	if err != nil {
		return nil, false, fmt.Errorf("open the skills lock: %w", err)
	}
	if err := lockFile(file, shared); err != nil {
		return nil, false, errors.Join(fmt.Errorf("take the skills lock: %w", err), file.Close())
	}
	return func() error { return errors.Join(unlockFile(file), file.Close()) }, true, nil
}
