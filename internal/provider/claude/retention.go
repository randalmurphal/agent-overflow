package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
)

// Transcript retention.
//
// Claude Code deletes session transcripts older than `cleanupPeriodDays`
// (default 30) in a background sweep. The sweep belongs to the config home,
// not to a session: any CLI process that outlives its start by a few seconds
// can run it, a headless session and an account probe included, and it
// deletes every old transcript under the home using that process's own
// resolved value. The shortest value among the processes that sweep is
// therefore the home's effective retention, so every process Agent Overflow
// starts against the user's home carries an answer: a session and an MCP
// sign-in (Config.CleanupPeriodDays), an account probe
// (ProbeConfig.CleanupPeriodDays), the interactive TUI
// (claudetui.Config.CleanupPeriodDays) and a text-generation run
// (textgen.Config.ClaudeCleanupPeriodDays). `--no-session-persistence` does
// not stop the sweep. `claude mcp list` and `--version` never start it, and
// a sign-in runs in its own empty home.
//
// Each of those launches clears ConfigHomeUnsetEnv, so the config home it
// sweeps is `.claude` under the HOME it runs with, and the caller passes
// that home to ReadCleanupPeriod. The exception is a probe pinned to an
// isolated home, which holds no transcripts.
//
// The value travels in the `--settings` block, whose flagSettings source
// outranks the user, project and local settings files. CleanupPeriodOverride
// sends it only when it lengthens what those files resolve to for the
// launch's working directory, so a longer period chosen in any of them
// stands. Verified against claude 2.1.321 in an isolated home.

// CleanupPeriodDaysSetting is the Claude Code settings key.
const CleanupPeriodDaysSetting = "cleanupPeriodDays"

// DefaultCleanupPeriodDays is the CLI's value when no settings source sets
// the key.
const DefaultCleanupPeriodDays = 30

// maxSettingsFileBytes bounds the read of one settings file.
const maxSettingsFileBytes = 8 << 20

// ConfigHomeUnsetEnv lists the variables that move the CLI's config home or
// its credential storage. Every launch against the user's home removes them
// from the child's environment.
func ConfigHomeUnsetEnv() []string {
	return []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR"}
}

// CleanupPeriod is the transcript retention a CLI process resolves from its
// settings files, with no `--settings` value.
type CleanupPeriod struct {
	// Days is the resolved value: the highest-precedence file's, or
	// DefaultCleanupPeriodDays when no file sets the key.
	Days int
	// Explicit reports whether a file sets the key.
	Explicit bool
}

// UserSettingsPathForHome returns `<home>/.claude/settings.json`.
func UserSettingsPathForHome(home string) string {
	return filepath.Join(home, ".claude", "settings.json")
}

// ReadCleanupPeriod resolves `cleanupPeriodDays` as a CLI started in workDir
// under home does: the user file, then the workspace's project and local
// files, a later one that sets the key winning. An empty workDir resolves
// the user file alone. It never writes.
//
// An error means the value is unknown: a file could not be read or parsed,
// or sets a value the CLI rejects. A caller must then send no override,
// because any value it sent could shorten the real one.
func ReadCleanupPeriod(home, workDir string) (CleanupPeriod, error) {
	paths := []string{UserSettingsPathForHome(home)}
	if workDir != "" {
		paths = append(paths,
			filepath.Join(workDir, ".claude", "settings.json"),
			filepath.Join(workDir, ".claude", "settings.local.json"),
		)
	}
	period := CleanupPeriod{Days: DefaultCleanupPeriodDays}
	for _, path := range paths {
		days, set, err := readCleanupPeriodFile(path)
		if err != nil {
			return CleanupPeriod{}, err
		}
		if set {
			period = CleanupPeriod{Days: days, Explicit: true}
		}
	}
	return period, nil
}

// readCleanupPeriodFile reads the key from one settings file. A missing or
// empty file, or one without the key, reports set false.
func readCleanupPeriodFile(path string) (days int, set bool, err error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read Claude settings: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxSettingsFileBytes+1))
	if closeErr := file.Close(); readErr == nil {
		readErr = closeErr
	}
	if readErr != nil {
		return 0, false, fmt.Errorf("read Claude settings %s: %w", path, readErr)
	}
	if len(data) > maxSettingsFileBytes {
		return 0, false, fmt.Errorf("read Claude settings %s: larger than %d bytes", path, maxSettingsFileBytes)
	}

	data = bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	if len(data) == 0 {
		return 0, false, nil
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return 0, false, fmt.Errorf("parse Claude settings %s: %w", path, err)
	}
	if keys == nil {
		return 0, false, fmt.Errorf("parse Claude settings %s: not a JSON object", path)
	}
	raw, ok := keys[CleanupPeriodDaysSetting]
	if !ok {
		return 0, false, nil
	}

	// The CLI's schema is a positive integer; JSON null decodes to zero
	// here and is refused with the other values below one.
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil || value < 1 || value != math.Trunc(value) {
		return 0, false, fmt.Errorf(
			"%s sets %s to %s; Claude Code accepts a whole number of days, 1 or more",
			path, CleanupPeriodDaysSetting, clipSettingValue(raw))
	}
	return int(math.Min(value, math.MaxInt32)), true, nil
}

// clipSettingValue bounds a raw settings value quoted in an error.
func clipSettingValue(raw json.RawMessage) string {
	const limit = 40
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "..."
}

// CleanupPeriodOverride returns the `cleanupPeriodDays` a Claude process
// should carry so it keeps transcripts for keepDays, or 0 to send none. It
// is 0 unless keepDays is longer than the period the process resolves
// itself.
func CleanupPeriodOverride(keepDays int, resolved CleanupPeriod) int {
	if keepDays <= resolved.Days {
		return 0
	}
	return keepDays
}

// CleanupPeriodArgs returns the `--settings` argv pair that carries days,
// for a launch that sends no other settings key. It is nil when days is not
// positive.
func CleanupPeriodArgs(days int) []string {
	settingsJSON, ok := inlineSettingsForCLI(Config{CleanupPeriodDays: days})
	if !ok {
		return nil
	}
	return []string{"--settings", settingsJSON}
}
