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
	Theme              string `json:"theme"`
	TimestampFormat    string `json:"timestampFormat"`
	DefaultProvider    string `json:"defaultProvider"`
	DefaultModelClaude string `json:"defaultModelClaude"`
	DefaultModelCodex  string `json:"defaultModelCodex"`
	// ModelContextWindows remembers the context-window preference per
	// model slug. It lets Claude Sonnet stay on 200k while Opus stays on
	// 1M, and preserves user overrides independently for each model.
	ModelContextWindows map[string]int `json:"modelContextWindows"`
	RecentWorkspaces    []string       `json:"recentWorkspaces"`
	DiffWordWrap        bool           `json:"diffWordWrap"`
	ShowEndOfTurnDiffs  bool           `json:"showEndOfTurnDiffs"`
	// BackgroundTrayExpanded remembers the user's global background-task
	// tray preference. False keeps fresh installs collapsed; opening the
	// tray persists across thread switches and app restarts.
	BackgroundTrayExpanded bool   `json:"backgroundTrayExpanded"`
	StreamingEnabled       bool   `json:"streamingEnabled"`
	ConfirmArchive         bool   `json:"confirmArchive"`
	ConfirmDelete          bool   `json:"confirmDelete"`
	ClaudeBinaryPath       string `json:"claudeBinaryPath"`
	CodexBinaryPath        string `json:"codexBinaryPath"`
	ClaudeEnabled          bool   `json:"claudeEnabled"`
	CodexEnabled           bool   `json:"codexEnabled"`

	// DefaultRuntimeMode is the three-tier approval axis applied to every
	// new thread when the user hasn't picked a different mode at creation
	// time. Accepts "approval-required", "auto-accept-edits", or
	// "full-access"; unknown values are coerced to "full-access" at the
	// provider-mode chokepoint so a stale settings file can't lock the
	// app into an invalid mode.
	DefaultRuntimeMode string `json:"defaultRuntimeMode"`

	// DefaultThreadEnvMode seeds the workspace mode for new draft threads.
	// Accepts "local" or "worktree"; unknown values fall back to "local"
	// when settings are loaded.
	DefaultThreadEnvMode string `json:"defaultThreadEnvMode"`

	// WorktreeBranchPrefix is prepended to auto-generated temporary and
	// semantic worktree branch names. It is intentionally flat (default
	// "ao-") rather than namespace-like ("ao/") so generated branches
	// read like normal feature branches.
	WorktreeBranchPrefix string `json:"worktreeBranchPrefix"`

	// DefaultReasoningEffort is the effort tier seeded on every new
	// thread. Accepts the five values the provider package exposes
	// (low / medium / high / xhigh / max); unknown values coerce to
	// "high".
	DefaultReasoningEffort string `json:"defaultReasoningEffort"`

	// DefaultFastMode seeds the per-thread fast-mode toggle.
	DefaultFastMode bool `json:"defaultFastMode"`

	// DefaultContextWindow seeds the per-thread context-window pref.
	// 200000 and 1000000 are the only schema-legal values; unknown
	// values fall back to 1000000 at the validation layer.
	DefaultContextWindow int `json:"defaultContextWindow"`

	// DefaultMode seeds the per-thread interaction mode (chat / plan /
	// design / discussion). Discussion is reached via a separate flow,
	// but is included in the enum for symmetry with provider.ModeDiscussion.
	// New thread creation intentionally ignores this legacy field and
	// starts in chat mode unless a caller explicitly passes a mode.
	DefaultMode string `json:"defaultMode"`

	// TextGenerationProvider selects which CLI drives non-chat text
	// generation (commit messages today; PR bodies and thread titles
	// eventually). Mirrors t3-code's RoutingTextGeneration: one of
	// {"codex", "claude"}. Empty falls through to the default at the
	// validation layer.
	TextGenerationProvider string `json:"textGenerationProvider"`

	// TextGenerationModel is the model id the text-generation CLI uses.
	// Empty string means "use the per-provider default" (codex ->
	// gpt-5.4-mini, claude -> claude-haiku-4-5). We avoid forcing a
	// concrete default on the field itself because the right model
	// depends on which provider is selected, and a cross-provider
	// default would be wrong half the time.
	TextGenerationModel string `json:"textGenerationModel"`

	// TextGenerationReasoningEffort controls the reasoning budget the
	// text-generation CLI spends. Mirrors the five-tier thread-level
	// enum. Default is "low" — commit/PR message generation benefits
	// more from speed than from heavy reasoning.
	TextGenerationReasoningEffort string `json:"textGenerationReasoningEffort"`

	// Observability — all opt-in. Empty/false defaults leave the app quiet.
	//
	// SECURITY NOTE: this file is stored on disk in plaintext and is
	// read/written without any encryption. That is fine for the fields
	// we currently persist — an OTLP endpoint is not a secret (it's a
	// gRPC/HTTP URL the user already exposes to every app running on
	// their machine). If a future field stores anything that could
	// reasonably be called a secret (API keys, bearer tokens, user
	// credentials), do NOT put it here: settings.json is written with
	// 0644 permissions, landed in a user-visible config dir, and may
	// be synced to cloud backup without the user realising. Put secrets
	// in the OS keychain via a dedicated package and keep this struct
	// for non-sensitive preferences only.
	ObservabilityTracingEnabled  bool   `json:"observabilityTracingEnabled"`
	ObservabilityOtlpEndpoint    string `json:"observabilityOtlpEndpoint"`
	ObservabilityEventLogEnabled bool   `json:"observabilityEventLogEnabled"`
}

// DefaultSettings provides sane defaults for all settings fields.
var DefaultSettings = Settings{
	Theme:                  "system",
	TimestampFormat:        "locale",
	DefaultProvider:        "claude",
	DefaultModelClaude:     "claude-opus-4-7",
	DefaultModelCodex:      "gpt-5.5",
	DiffWordWrap:           false,
	ShowEndOfTurnDiffs:     true,
	BackgroundTrayExpanded: false,
	StreamingEnabled:       true,
	ConfirmArchive:         true,
	ConfirmDelete:          true,
	ClaudeBinaryPath:       "claude",
	CodexBinaryPath:        "codex",
	ClaudeEnabled:          true,
	CodexEnabled:           true,
	// DefaultRuntimeMode mirrors provider.DefaultRuntimeMode. Duplicated as
	// a string literal rather than imported so internal/settings doesn't
	// pull in the provider package (tiny leaf package, kept leaf).
	DefaultRuntimeMode:   "full-access",
	DefaultThreadEnvMode: "local",
	WorktreeBranchPrefix: "ao-",
	// DefaultReasoningEffort mirrors provider.DefaultReasoningEffort.
	DefaultReasoningEffort: "high",
	// DefaultFastMode defaults to off — power users opt in per thread.
	DefaultFastMode: false,
	// DefaultContextWindow defaults to the 1M-token tier (Claude's
	// extended beta). Codex ignores the field at the translation
	// boundary.
	DefaultContextWindow: 1000000,
	// DefaultMode mirrors provider.DefaultInteractionMode.
	DefaultMode: "chat",
	// Text-generation defaults: Codex is cheap + fast for short JSON
	// responses, so it's the sensible default. The model stays empty
	// so the call site picks the per-provider default; if the user
	// switches provider without updating model, the app still works.
	TextGenerationProvider:        "codex",
	TextGenerationModel:           "",
	TextGenerationReasoningEffort: "low",
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
	// unknownFields captures any top-level JSON keys from the on-disk file
	// that do not map to a field on the Settings struct. We preserve them
	// verbatim on writeSparse so downgrading the app, or running a build
	// with forward-compat fields the Settings struct doesn't yet know
	// about, does not silently drop those fields. Written under s.mu.
	unknownFields map[string]json.RawMessage
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
// with s.mu held (either read or write). Captures any unknown top-level keys
// into s.unknownFields so a follow-up write preserves them.
func (s *Service) loadFromFile() Settings {
	data, err := os.ReadFile(s.path)
	if err != nil {
		// Missing file is normal on first run.
		s.unknownFields = nil
		return copyDefaults()
	}

	// Start from defaults, then overlay file values.
	result := copyDefaults()
	if err := json.Unmarshal(data, &result); err != nil {
		log.Printf("settings: malformed JSON in %s, using defaults: %v", s.path, err)
		s.unknownFields = nil
		return copyDefaults()
	}
	s.unknownFields = captureUnknownFields(data)
	return sanitizeLoadedSettings(result)
}

// captureUnknownFields returns a map of top-level JSON keys from raw that
// do not correspond to a field on the Settings struct. Used to preserve
// forward-compat / downgrade fields across a write.
func captureUnknownFields(raw []byte) map[string]json.RawMessage {
	var fileMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fileMap); err != nil {
		return nil
	}
	known := knownSettingsFieldNames()
	unknown := make(map[string]json.RawMessage)
	for k, v := range fileMap {
		if _, ok := known[k]; ok {
			continue
		}
		unknown[k] = v
	}
	if len(unknown) == 0 {
		return nil
	}
	return unknown
}

// knownSettingsFieldNames returns the set of JSON field names the Settings
// struct serializes. Computed by marshalling the default Settings value and
// reading its keys — this keeps the set in sync with the struct definition
// automatically as fields are added or renamed.
func knownSettingsFieldNames() map[string]struct{} {
	data, err := json.Marshal(DefaultSettings)
	if err != nil {
		return map[string]struct{}{}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]struct{}{}
	}
	known := make(map[string]struct{}, len(m))
	for k := range m {
		known[k] = struct{}{}
	}
	return known
}

// writeSparse persists only the fields that differ from DefaultSettings.
// Uses atomic write (temp file + rename). Unknown fields previously read
// from the file are preserved alongside the sparse known fields so
// forward-compat / downgrade values are not dropped by an Update.
func (s *Service) writeSparse(current Settings) error {
	sparse, err := buildSparseMap(current)
	if err != nil {
		return fmt.Errorf("settings: build sparse map: %w", err)
	}

	// Merge unknown fields under the sparse known fields. Known keys win
	// if the unknown-fields map somehow contains a clashing key — this
	// can happen only if Settings gained a field since loadFromFile was
	// called, and the new field is a known one now.
	merged := make(map[string]any, len(sparse)+len(s.unknownFields))
	for k, v := range s.unknownFields {
		merged[k] = v
	}
	for k, v := range sparse {
		merged[k] = v
	}

	data, err := json.MarshalIndent(merged, "", "  ")
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
