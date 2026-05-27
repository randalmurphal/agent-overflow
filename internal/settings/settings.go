package settings

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

// NetworkSettings groups LAN-bind preferences for the embedded
// transport server. Persisted as a nested object so the JSON shape
// stays stable when more network fields land (origin allow-list,
// TLS hints, etc.).
type NetworkSettings struct {
	// BindAll, when true, asks the transport server to listen on
	// 0.0.0.0 so other devices on the LAN can reach the app. Default
	// false keeps the bind on 127.0.0.1 — the safe loopback behaviour.
	BindAll bool `json:"bindAll"`
}

// EditorSettings groups the open-in-editor preferences. Lives in its
// own nested object so future fields (custom argv template, last-used
// editor for analytics, etc.) can land without reshuffling the
// top-level Settings struct.
type EditorSettings struct {
	// Preference is the editor ID (e.g. "code", "cursor",
	// "env:editor") the user explicitly selected. Empty falls back
	// to the catalog priority order in internal/editor.Resolve.
	Preference string `json:"preference"`
}

// RetentionSettings groups TTL cleanup preferences for the background
// sweeper that prunes stale threads (and their on-disk side effects)
// plus dated provider-event log files and bug-report bookmark files.
// Persisted as a nested object so future retention knobs (per-resource
// overrides, exemption lists) can land without reshuffling Settings.
type RetentionSettings struct {
	// Days is the age threshold in days. Threads whose updated_at is
	// older than now-(Days*24h) are eligible for sweep, as are dated
	// provider-event log files and bug-report bookmark files. A value
	// of 0 disables the sweep entirely.
	Days int `json:"days"`
}

// PaneLayoutSettings stores the user's visible thread-pane arrangement.
// This used to live in webview localStorage, but packaged webviews are not
// durable on every platform. Keep it with settings so app restart behavior
// is owned by the same cross-platform persistence path as sidebar layout.
type PaneLayoutSettings struct {
	Version       int              `json:"version"`
	Panes         []PaneLayoutPane `json:"panes,omitempty"`
	FocusedPaneID string           `json:"focusedPaneId,omitempty"`
}

type PaneLayoutPane struct {
	PaneID   string  `json:"paneId"`
	ThreadID string  `json:"threadId"`
	Ratio    float64 `json:"ratio"`
}

// CurrentSchemaVersion is the version stamped on every Update-written
// settings file. Bump on any breaking shape change so a future loader
// can branch on the version and run a one-shot migration before
// merging defaults.
//
// Backwards compatibility convention: bump only when an existing field
// changes shape or semantics. Adding a new field, even with a
// non-zero default, does not require a bump because the sparse-load
// path tolerates absent keys naturally.
const CurrentSchemaVersion = 1

// Settings holds all user-configurable preferences.
type Settings struct {
	// SchemaVersion is the version of the on-disk shape this struct
	// expects. Older files (or files written before versioning) load as
	// SchemaVersion=0 and the Service treats them identically to
	// CurrentSchemaVersion until a future shape change introduces a
	// migration step. Never written by users; always overwritten to
	// CurrentSchemaVersion on any save via writeSparse.
	SchemaVersion int `json:"$schemaVersion,omitempty"`

	Theme           string `json:"theme"`
	TimestampFormat string `json:"timestampFormat"`
	// SansFont and MonoFont select the typefaces wired into the
	// `--font-sans` and `--font-mono` CSS variables on the frontend.
	// Each is one of {"geist", "hack-nerd", "system"}. "geist" is the
	// eagerly-bundled default; "hack-nerd" lazy-loads a separate woff2
	// chunk so users on the default never pay its bundle cost; "system"
	// falls through to the OS fallback chain and adds zero weight.
	SansFont         string   `json:"sansFont"`
	MonoFont         string   `json:"monoFont"`
	FontSize         int      `json:"fontSize"`
	RecentWorkspaces []string `json:"recentWorkspaces"`
	DiffWordWrap     bool     `json:"diffWordWrap"`
	StreamingEnabled bool     `json:"streamingEnabled"`
	ConfirmArchive   bool     `json:"confirmArchive"`
	ConfirmDelete    bool     `json:"confirmDelete"`
	ClaudeBinaryPath string   `json:"claudeBinaryPath"`
	CodexBinaryPath  string   `json:"codexBinaryPath"`
	ClaudeEnabled    bool     `json:"claudeEnabled"`
	CodexEnabled     bool     `json:"codexEnabled"`

	// DefaultThreadEnvMode seeds the workspace mode for new draft threads.
	// Accepts "local" or "worktree"; unknown values fall back to "local"
	// when settings are loaded.
	DefaultThreadEnvMode string `json:"defaultThreadEnvMode"`

	// WorktreeBranchPrefix is prepended to auto-generated temporary and
	// semantic worktree branch names. It is intentionally flat (default
	// "ao-") rather than namespace-like ("ao/") so generated branches
	// read like normal feature branches.
	WorktreeBranchPrefix string `json:"worktreeBranchPrefix"`

	// PaneDensity controls the minimum workspace pane width before the pane
	// host starts horizontal scrolling. One of {"compact", "comfortable",
	// "spacious"}.
	PaneDensity string `json:"paneDensity"`

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

	// Auto-compact thresholds, percent-of-window. Per provider per tier;
	// model-agnostic by design (the user picks model + active window via
	// the composer's pickers, not via Settings). At session start the
	// resolved threshold is `thread.AutoCompactPercent || settings.<...>`,
	// so editing the slider applies live to the next turn on existing
	// threads unless the user has set a per-thread override.
	//
	// Range 1..90; load-time sanitization clamps out-of-range values to
	// the default (90) rather than rejecting the file.
	ClaudeAutoCompactStandardPercent int `json:"claudeAutoCompactStandardPercent"`
	ClaudeAutoCompactExtendedPercent int `json:"claudeAutoCompactExtendedPercent"`
	CodexAutoCompactStandardPercent  int `json:"codexAutoCompactStandardPercent"`
	CodexAutoCompactExtendedPercent  int `json:"codexAutoCompactExtendedPercent"`

	// Observability — all opt-in. Empty/false defaults leave the app quiet.
	//
	// SECURITY NOTE: this file is stored on disk in plaintext and is
	// read/written without any encryption. settings.json itself lands
	// at 0600 (the default os.CreateTemp picks for the temp file we
	// rename in over the destination), and the parent directory is
	// created at 0700 since this struct now persists per-launch tokens.
	// Even with restrictive perms, anything that could reasonably be
	// called a long-lived secret (API keys, OAuth refresh tokens, user
	// credentials) does NOT belong here: this file lives in a
	// user-visible config dir and may be swept into cloud backup tools
	// without the user realising. Put long-lived secrets in the OS
	// keychain via a dedicated package and keep this struct for
	// preferences plus per-launch bootstrap material only.
	ObservabilityTracingEnabled  bool   `json:"observabilityTracingEnabled"`
	ObservabilityOtlpEndpoint    string `json:"observabilityOtlpEndpoint"`
	ObservabilityEventLogEnabled bool   `json:"observabilityEventLogEnabled"`

	// Network groups LAN-bind preferences. Default zero value keeps
	// the transport on loopback; flipping BindAll triggers a
	// transport-server rebind to 0.0.0.0 without restarting the app.
	Network NetworkSettings `json:"network"`

	// Editor holds the open-in-editor preferences. Default zero value
	// lets internal/editor pick the best available editor via catalog
	// priority; setting Editor.Preference pins a specific one even
	// when later WSL detection finds a higher-priority option.
	Editor EditorSettings `json:"editor"`

	// Retention controls the background TTL sweep. Default
	// Retention.Days = 30 cleans threads, dated provider-event logs,
	// and bug-report bookmark files older than the window. 0 disables.
	Retention RetentionSettings `json:"retention"`

	// GitLabSelfHostedHosts is the user's allowlist of self-hosted
	// GitLab hostnames (bare hosts, e.g. "gitlab.mycompany.com").
	// Origin URLs whose host matches an entry classify as the "gitlab"
	// forge, enabling the Ship Changes wizard, MR labels, and the
	// `glab` CLI integration. `gitlab.com` does not need to be listed;
	// it is recognised by literal hostname match. Entries are stored
	// lowercase, deduped, and stripped of scheme/path on write.
	GitLabSelfHostedHosts []string `json:"gitlabSelfHostedHosts,omitempty"`

	// RemoteEndpoints stores the user's `--connect` targets: remote-
	// hosted backends the desktop binary can attach to instead of
	// booting a local transport. Persisted as a flat list keyed by
	// stable IDs so the settings UI can rename / re-order without
	// disturbing the connect commands the user has already shared.
	//
	// SECURITY NOTE: this list contains ephemeral session tokens. They
	// are stored in plaintext alongside settings.json (file lands at
	// 0600, parent dir at 0700). That matches the threat model
	// documented above — settings.json must not contain anything more
	// sensitive than what a local-process attacker could already read
	// out of running webviews. If the remote endpoints' tokens ever
	// become long-lived bearer tokens, move this field to a
	// keychain-backed store and remove the JSON persistence path.
	RemoteEndpoints []RemoteEndpoint `json:"remoteEndpoints,omitempty"`

	// ProjectSortMode controls sidebar project ordering. One of
	// {"lastActivity", "createdAt", "manual"}. Persisted here rather
	// than in the webview's localStorage because localStorage is
	// ephemeral on some platforms (WebKit2GTK / WSL2).
	ProjectSortMode string `json:"projectSortMode"`

	// CollapsedProjects lists project IDs the user has explicitly
	// collapsed in the sidebar. Projects not in this list default to
	// expanded. Same persistence rationale as ProjectSortMode.
	CollapsedProjects []string `json:"collapsedProjects,omitempty"`

	// PaneLayout stores the visible thread panes, their order/ratios,
	// and the focused pane. Same persistence rationale as
	// ProjectSortMode: webview localStorage is not durable everywhere.
	PaneLayout PaneLayoutSettings `json:"paneLayout"`
}

// DefaultSettings provides sane defaults for all settings fields.
var DefaultSettings = Settings{
	Theme:                "system",
	TimestampFormat:      "locale",
	SansFont:             "geist",
	MonoFont:             "geist",
	FontSize:             13,
	DiffWordWrap:         false,
	StreamingEnabled:     true,
	ConfirmArchive:       true,
	ConfirmDelete:        true,
	ClaudeBinaryPath:     "claude",
	CodexBinaryPath:      "codex",
	ClaudeEnabled:        true,
	CodexEnabled:         true,
	DefaultThreadEnvMode: "local",
	WorktreeBranchPrefix: "ao-",
	PaneDensity:          "compact",
	// Text-generation defaults: Codex is cheap + fast for short JSON
	// responses, so it's the sensible default. The model stays empty
	// so the call site picks the per-provider default; if the user
	// switches provider without updating model, the app still works.
	TextGenerationProvider:        "codex",
	TextGenerationModel:           "",
	TextGenerationReasoningEffort: "low",
	// Both providers ship a 90% default — aggressive enough that the
	// user notices auto-compact when it triggers, conservative enough
	// to leave headroom for the final response. The percent applies to
	// whichever tier matches the live context window.
	ClaudeAutoCompactStandardPercent: 90,
	ClaudeAutoCompactExtendedPercent: 90,
	CodexAutoCompactStandardPercent:  90,
	CodexAutoCompactExtendedPercent:  90,
	// Observability defaults to off so there is zero runtime cost for users
	// who don't opt in. The OTLP endpoint is only meaningful when tracing
	// is enabled; we leave it blank so a misconfigured endpoint can't cause
	// silent failures for default users.
	ObservabilityTracingEnabled:  false,
	ObservabilityOtlpEndpoint:    "",
	ObservabilityEventLogEnabled: false,
	// 30 days is the default retention window. The cleanup loop reads
	// this on every tick, so toggling it doesn't require a restart.
	Retention:       RetentionSettings{Days: 30},
	ProjectSortMode: "lastActivity",
	PaneLayout:      PaneLayoutSettings{Version: 1},
}

// AutoCompactPercents returns the per-tier compact thresholds for the
// given provider as (standard, extended). Unknown providers fall back
// to the Claude pair so a stale provider string can't strand a session
// with 0/0 (which would disable auto-compact entirely).
func (s Settings) AutoCompactPercents(provider string) (standard, extended int) {
	switch provider {
	case "codex":
		return s.CodexAutoCompactStandardPercent, s.CodexAutoCompactExtendedPercent
	default:
		return s.ClaudeAutoCompactStandardPercent, s.ClaudeAutoCompactExtendedPercent
	}
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
//
// The "remoteEndpoints" key is rejected at the patch boundary: applyPatch
// merges top-level keys via wholesale assignment, so a caller doing
// `GetSettings -> mutate one field -> Update(full struct)` would clobber
// every saved endpoint's token with the redacted (empty) values returned
// by GetSettings. Tokens are only mutated through the dedicated CRUD
// helpers (AddRemoteEndpoint / UpdateRemoteEndpoint / DeleteRemoteEndpoint
// / TouchRemoteEndpoint) which read the persisted token before writing.
// This guard keeps a future caller — including a refactor or remote
// loopback path — from regressing the contract.
func (s *Service) Update(patch map[string]any) (Settings, error) {
	if _, ok := patch["remoteEndpoints"]; ok {
		return Settings{}, fmt.Errorf("settings: use AddRemoteEndpoint / UpdateRemoteEndpoint / DeleteRemoteEndpoint to mutate remote endpoints")
	}
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
//
// On a JSON parse failure we rename the broken file aside as
// `settings.json.corrupt-<unix>` BEFORE returning defaults, so a
// subsequent writeSparse can't silently overwrite the original. The
// corrupt file is left on disk for the user to inspect or copy fields
// out of — losing remote-endpoint tokens or recent workspaces because
// of one bad write would erase work the user expects to be durable.
// The rename is best-effort; if it fails we still return defaults so
// the app can boot, but we log loudly.
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
		preservedPath := s.path + fmt.Sprintf(".corrupt-%d", time.Now().Unix())
		if renameErr := os.Rename(s.path, preservedPath); renameErr != nil {
			log.Printf("settings: malformed JSON in %s and could not preserve original (%v); falling back to defaults: %v", s.path, renameErr, err)
		} else {
			log.Printf("settings: malformed JSON in %s; original preserved at %s, falling back to defaults: %v", s.path, preservedPath, err)
		}
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
	retired := retiredSettingsFieldNames()
	unknown := make(map[string]json.RawMessage)
	for k, v := range fileMap {
		if _, ok := known[k]; ok {
			continue
		}
		if _, ok := retired[k]; ok {
			continue
		}
		unknown[k] = v
	}
	if len(unknown) == 0 {
		return nil
	}
	return unknown
}

// knownSettingsFieldNames returns the set of JSON field names the
// Settings struct serializes. Computed by reflecting on the struct's
// fields rather than marshalling DefaultSettings — `omitempty` fields
// with zero defaults (e.g. RemoteEndpoints) would be missing from the
// marshalled view, which would mis-classify a user-written value as
// "unknown" and double-publish it through unknownFields preservation.
//
// The reflection path keeps the set in sync with the struct definition
// automatically as fields are added or renamed, same as the marshal
// approach, but without the `omitempty` blind spot.
func knownSettingsFieldNames() map[string]struct{} {
	t := reflect.TypeOf(Settings{})
	known := make(map[string]struct{}, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		// JSON tag may be "name,omitempty" — split and keep just the name.
		name := tag
		if idx := strings.Index(tag, ","); idx >= 0 {
			name = tag[:idx]
		}
		if name == "" {
			continue
		}
		known[name] = struct{}{}
	}
	return known
}

func retiredSettingsFieldNames() map[string]struct{} {
	return map[string]struct{}{
		"defaultProvider":        {},
		"defaultModelClaude":     {},
		"defaultModelCodex":      {},
		"modelContextWindows":    {},
		"defaultMode":            {},
		"defaultRuntimeMode":     {},
		"defaultReasoningEffort": {},
		"defaultFastMode":        {},
		"defaultContextWindow":   {},
	}
}

// writeSparse persists only the fields that differ from DefaultSettings.
// Uses atomic write (temp file + rename). Unknown fields previously read
// from the file are preserved alongside the sparse known fields so
// forward-compat / downgrade values are not dropped by an Update.
//
// Stamps SchemaVersion = CurrentSchemaVersion on every write so a
// future loader can branch on a missing/older version and run a
// migration. Older files written before versioning load as 0; future
// writes by this build re-stamp them to the current version.
func (s *Service) writeSparse(current Settings) error {
	current.SchemaVersion = CurrentSchemaVersion
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

	// Ensure the directory exists. 0700 because this struct now stores
	// per-launch tokens (RemoteEndpoints[*].Token); even though the
	// renamed temp file lands at 0600 itself, a 0755 parent would let
	// other local accounts list the dir contents. MkdirAll is a no-op
	// when dir already exists with looser perms — that's acceptable
	// because the file's own 0600 still gates the contents.
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
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

// copyDefaults returns a copy of DefaultSettings with nil slice fields so
// callers can append without aliasing package-level defaults.
func copyDefaults() Settings {
	d := DefaultSettings
	d.RecentWorkspaces = nil
	d.GitLabSelfHostedHosts = nil
	d.CollapsedProjects = nil
	d.PaneLayout.Panes = nil
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
