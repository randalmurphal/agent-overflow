package skillsinstall

import (
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

// lockName is the file in `<home>/skills` whose advisory lock every
// Install, Update and Remove holds from reading the manifest to writing it,
// so two processes sharing a home (two app profiles, two versions) cannot
// interleave their copies. It stays: removing a lock file races its users.
const lockName = ".agent-overflow-pstack.lock"

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
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("read the skills manifest %s: %w", manifestPath(home), err)
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

// lockHome takes the lock on home's skills directory, creating the
// directory when create is set. found is false, with no lock taken, when the
// directory does not exist and create is not set.
func lockHome(home string, create bool) (unlock func() error, found bool, err error) {
	dir := skillsDir(home)
	if create {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, false, fmt.Errorf("create %s: %w", dir, err)
		}
	} else if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	file, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("open the skills lock: %w", err)
	}
	if err := lockFile(file); err != nil {
		return nil, false, errors.Join(fmt.Errorf("take the skills lock: %w", err), file.Close())
	}
	return func() error { return errors.Join(unlockFile(file), file.Close()) }, true, nil
}
