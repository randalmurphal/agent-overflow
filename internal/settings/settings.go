package settings

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Settings holds all user-configurable preferences.
type Settings struct {
	Theme              string   `json:"theme"`
	TimestampFormat    string   `json:"timestampFormat"`
	DefaultProvider    string   `json:"defaultProvider"`
	DefaultModelClaude string   `json:"defaultModelClaude"`
	DefaultModelCodex  string   `json:"defaultModelCodex"`
	RecentWorkspaces   []string `json:"recentWorkspaces"`
	DiffWordWrap       bool     `json:"diffWordWrap"`
	StreamingEnabled   bool     `json:"streamingEnabled"`
	ConfirmArchive     bool     `json:"confirmArchive"`
	ConfirmDelete      bool     `json:"confirmDelete"`
	ClaudeBinaryPath   string   `json:"claudeBinaryPath"`
	CodexBinaryPath    string   `json:"codexBinaryPath"`
	ClaudeEnabled      bool     `json:"claudeEnabled"`
	CodexEnabled       bool     `json:"codexEnabled"`

	// Observability — all opt-in. Empty/false defaults leave the app quiet.
	ObservabilityTracingEnabled  bool   `json:"observabilityTracingEnabled"`
	ObservabilityOtlpEndpoint    string `json:"observabilityOtlpEndpoint"`
	ObservabilityEventLogEnabled bool   `json:"observabilityEventLogEnabled"`
}

// DefaultSettings provides sane defaults for all settings fields.
var DefaultSettings = Settings{
	Theme:              "system",
	TimestampFormat:    "locale",
	DefaultProvider:    "claude",
	DefaultModelClaude: "claude-sonnet-4-6",
	DefaultModelCodex:  "gpt-5.4",
	DiffWordWrap:       false,
	StreamingEnabled:   true,
	ConfirmArchive:     true,
	ConfirmDelete:      true,
	ClaudeBinaryPath:   "claude",
	CodexBinaryPath:    "codex",
	ClaudeEnabled:      true,
	CodexEnabled:       true,
	// Observability defaults to off so there is zero runtime cost for users
	// who don't opt in. The OTLP endpoint is only meaningful when tracing
	// is enabled; we leave it blank so a misconfigured endpoint can't cause
	// silent failures for default users.
	ObservabilityTracingEnabled:  false,
	ObservabilityOtlpEndpoint:    "",
	ObservabilityEventLogEnabled: false,
}

// Service manages reading and writing the settings JSON file.
type Service struct {
	path        string
	mu          sync.RWMutex
	cached      *Settings
	cachedState fileState
}

type fileState struct {
	exists  bool
	modTime time.Time
	size    int64
}

// NewService creates a settings service that reads/writes configDir/settings.json.
// The file is not created until the first write.
func NewService(configDir string) *Service {
	return &Service{
		path: filepath.Join(configDir, "settings.json"),
	}
}

// Path returns the full path to the settings file.
func (s *Service) Path() string {
	return s.path
}

// Get returns the current settings, merging file contents over defaults.
// If the file is missing or malformed, defaults are returned.
func (s *Service) Get() Settings {
	currentState := readFileState(s.path)

	s.mu.RLock()
	if s.cached != nil && s.cachedState.equal(currentState) {
		result := *s.cached
		s.mu.RUnlock()
		return result
	}
	s.mu.RUnlock()

	// Cache miss: load from file under write lock.
	s.mu.Lock()
	defer s.mu.Unlock()

	// Double-check after acquiring write lock.
	currentState = readFileState(s.path)
	if s.cached != nil && s.cachedState.equal(currentState) {
		return *s.cached
	}

	loaded := s.loadFromFile()
	s.cached = &loaded
	s.cachedState = currentState
	return loaded
}

// Update applies a partial patch to the current settings, persists the result
// with sparse serialization, and returns the new full settings.
func (s *Service) Update(patch map[string]any) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.loadFromFile()

	patched, err := applyPatch(current, patch)
	if err != nil {
		return Settings{}, fmt.Errorf("settings: apply patch: %w", err)
	}
	patched, err = validateSettings(patched)
	if err != nil {
		return Settings{}, fmt.Errorf("settings: validate: %w", err)
	}

	if err := s.writeSparse(patched); err != nil {
		return Settings{}, err
	}

	s.cached = &patched
	s.cachedState = readFileState(s.path)
	return patched, nil
}

// AddRecentWorkspace pushes a workspace path to the front of the recent list,
// deduplicating and capping at 10 entries.
func (s *Service) AddRecentWorkspace(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.loadFromFile()

	// Build new list: path first, then existing entries minus duplicates.
	seen := map[string]bool{path: true}
	recent := []string{path}
	for _, ws := range current.RecentWorkspaces {
		if !seen[ws] {
			seen[ws] = true
			recent = append(recent, ws)
		}
	}
	if len(recent) > 10 {
		recent = recent[:10]
	}

	current.RecentWorkspaces = recent

	if err := s.writeSparse(current); err != nil {
		log.Printf("settings: persist recent workspace: %v", err)
		return
	}

	s.cached = &current
	s.cachedState = readFileState(s.path)
}

// loadFromFile reads the settings file and merges over defaults.
// Returns DefaultSettings if the file is missing or malformed. Must be called
// with s.mu held (either read or write).
func (s *Service) loadFromFile() Settings {
	data, err := os.ReadFile(s.path)
	if err != nil {
		// Missing file is normal on first run.
		return copyDefaults()
	}

	// Start from defaults, then overlay file values.
	result := copyDefaults()
	if err := json.Unmarshal(data, &result); err != nil {
		log.Printf("settings: malformed JSON in %s, using defaults: %v", s.path, err)
		return copyDefaults()
	}
	return sanitizeLoadedSettings(result)
}

// writeSparse persists only the fields that differ from DefaultSettings.
// Uses atomic write (temp file + rename).
func (s *Service) writeSparse(current Settings) error {
	sparse, err := buildSparseMap(current)
	if err != nil {
		return fmt.Errorf("settings: build sparse map: %w", err)
	}

	data, err := json.MarshalIndent(sparse, "", "  ")
	if err != nil {
		return fmt.Errorf("settings: marshal: %w", err)
	}
	data = append(data, '\n')

	// Ensure the directory exists.
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("settings: create config dir: %w", err)
	}

	// Atomic write: temp file in same directory, then rename.
	tmp, err := os.CreateTemp(dir, "settings-*.tmp")
	if err != nil {
		return fmt.Errorf("settings: create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("settings: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("settings: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("settings: close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("settings: rename temp file: %w", err)
	}
	return nil
}

// buildSparseMap returns a map containing only fields that differ from defaults.
func buildSparseMap(current Settings) (map[string]any, error) {
	currentBytes, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	defaultBytes, err := json.Marshal(DefaultSettings)
	if err != nil {
		return nil, err
	}

	var currentMap, defaultMap map[string]any
	if err := json.Unmarshal(currentBytes, &currentMap); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(defaultBytes, &defaultMap); err != nil {
		return nil, err
	}

	sparse := make(map[string]any)
	for k, v := range currentMap {
		defaultVal, exists := defaultMap[k]
		if !exists || !jsonEqual(v, defaultVal) {
			sparse[k] = v
		}
	}
	return sparse, nil
}

// jsonEqual compares two values after JSON round-tripping to handle type
// normalization (e.g., float64 vs int).
func jsonEqual(a, b any) bool {
	aj, err1 := json.Marshal(a)
	bj, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(aj) == string(bj)
}

// applyPatch merges a partial map into a Settings struct using JSON
// marshal/unmarshal for type-safe conversion.
func applyPatch(base Settings, patch map[string]any) (Settings, error) {
	baseBytes, err := json.Marshal(base)
	if err != nil {
		return Settings{}, err
	}

	var merged map[string]any
	if err := json.Unmarshal(baseBytes, &merged); err != nil {
		return Settings{}, err
	}

	for k, v := range patch {
		merged[k] = v
	}

	mergedBytes, err := json.Marshal(merged)
	if err != nil {
		return Settings{}, err
	}

	var result Settings
	if err := json.Unmarshal(mergedBytes, &result); err != nil {
		return Settings{}, err
	}
	return result, nil
}

// copyDefaults returns a copy of DefaultSettings with a fresh slice for
// RecentWorkspaces to avoid aliasing.
func copyDefaults() Settings {
	d := DefaultSettings
	d.RecentWorkspaces = nil
	return d
}

func readFileState(path string) fileState {
	info, err := os.Stat(path)
	if err != nil {
		return fileState{}
	}
	return fileState{
		exists:  true,
		modTime: info.ModTime(),
		size:    info.Size(),
	}
}

func (s fileState) equal(other fileState) bool {
	return s.exists == other.exists && s.modTime.Equal(other.modTime) && s.size == other.size
}
