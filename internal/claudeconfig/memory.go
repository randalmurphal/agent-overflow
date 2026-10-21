package claudeconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// AutoMemoryOffEnv turns Claude Code's auto memory off when true and on
// when false, ahead of `autoMemoryEnabled`.
const AutoMemoryOffEnv = "CLAUDE_CODE_DISABLE_AUTO_MEMORY"

// AutoMemoryOn decides whether Claude Code's auto memory is on as the CLI
// decides it (docs/references/claude-wire.md, "Auto memory switch"). env and
// set are the variable as a session is spawned with it. The CLI assigns the
// `env` of ~/.claude.json and then of the user's settings.json over that, so
// the last of the three that holds the variable decides when it is a true
// or false word; otherwise autoMemoryEnabled in settings.json, on when
// absent. A file that exists and cannot be read or parsed is an error
// naming it.
func (s *Store) AutoMemoryOn(env string, set bool) (bool, error) {
	var global struct {
		Env map[string]json.RawMessage `json:"env"`
	}
	if err := readJSONFile(s.path, "Claude's config", &global); err != nil {
		return false, err
	}
	var settings struct {
		Env               map[string]json.RawMessage `json:"env"`
		AutoMemoryEnabled *bool                      `json:"autoMemoryEnabled"`
	}
	if err := readJSONFile(filepath.Join(s.home, "settings.json"), "Claude's settings", &settings); err != nil {
		return false, err
	}
	for _, layer := range []map[string]json.RawMessage{global.Env, settings.Env} {
		if value, ok := envValue(layer[AutoMemoryOffEnv]); ok {
			env, set = value, true
		}
	}
	if set {
		switch strings.ToLower(strings.TrimSpace(env)) {
		case "1", "true", "yes", "on":
			return false, nil
		case "0", "false", "no", "off":
			return true, nil
		}
	}
	return settings.AutoMemoryEnabled == nil || *settings.AutoMemoryEnabled, nil
}

// readJSONFile decodes the file at path into out; a missing file leaves out
// unchanged.
func readJSONFile(path, what string, out any) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err == nil {
		err = json.Unmarshal(raw, out)
	}
	if err != nil {
		return fmt.Errorf("read %s %s: %w", what, path, err)
	}
	return nil
}

// envValue is an `env` entry as the CLI assigns it: a string as it is, a
// number or boolean as its JSON text.
func envValue(raw json.RawMessage) (string, bool) {
	if raw == nil {
		return "", false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, true
	}
	return string(raw), true
}
