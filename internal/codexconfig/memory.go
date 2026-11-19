package codexconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"agent-overflow/internal/errorsx"

	"github.com/BurntSushi/toml"
)

// MemoriesOn reads `[features] memories` from the config, off when the file
// or the key is absent (docs/specs/memory.md, "Settings"). A file that
// exists and cannot be read or parsed is an error naming it. Its public
// message (errorsx.Public) names the file and the line, never the parser's
// text, which can quote a value from any field.
func (s *Store) MemoriesOn() (bool, error) {
	var config struct {
		Features struct {
			Memories bool `toml:"memories"`
		} `toml:"features"`
	}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errorsx.Public("config_unreadable", fmt.Sprintf("Codex's config %s could not be read.", s.path),
			fmt.Errorf("read Codex's config %s: %w", s.path, err))
	}
	if _, err := toml.Decode(string(raw), &config); err != nil {
		message := fmt.Sprintf("Codex's config %s could not be parsed", s.path)
		var parse toml.ParseError
		if errors.As(err, &parse) && parse.Position.Line > 0 {
			message += fmt.Sprintf(" at line %d", parse.Position.Line)
		}
		return false, errorsx.Public("config_unparsable", message+".", fmt.Errorf("parse Codex's config %s: %w", s.path, err))
	}
	return config.Features.Memories, nil
}
