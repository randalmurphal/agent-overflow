package codexconfig

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/BurntSushi/toml"
)

// MemoriesOn reads `[features] memories` from the config, off when the file
// or the key is absent (docs/specs/memory.md, "Settings"). A file that
// exists and cannot be read or parsed is an error naming it.
func (s *Store) MemoriesOn() (bool, error) {
	var config struct {
		Features struct {
			Memories bool `toml:"memories"`
		} `toml:"features"`
	}
	if _, err := toml.DecodeFile(s.path, &config); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read Codex's config %s: %w", s.path, err)
	}
	return config.Features.Memories, nil
}
